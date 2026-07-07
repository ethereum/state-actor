//go:build cgo_erigon_commitment

package commitment

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"

	gethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	erigoncommon "github.com/erigontech/erigon/common"
	erigonkv "github.com/erigontech/erigon/db/kv"
	erigoncommitment "github.com/erigontech/erigon/execution/commitment"

	"github.com/ethereum/state-actor/internal/streamsort"
)

// erigonHash converts a geth-style 32-byte hash to Erigon's equivalent
// type. Both are `[32]byte`; this is a free byte-by-byte copy.
func erigonHash(h gethcommon.Hash) erigoncommon.Hash {
	var out erigoncommon.Hash
	copy(out[:], h[:])
	return out
}

// Account is the state-actor-facing input shape for one alloc entry.
// Used by EncodeAccountUpdate to produce the bytes the orchestrator
// writes into the commitmentInputStore during the streaming autofill
// loop. NOT consumed directly by ComputeGenesisRoot anymore —
// ComputeGenesisRoot reads encoded Update bytes from the
// commitmentInputStore via streamsort.Get.
type Account struct {
	Address gethcommon.Address
	Nonce   uint64
	Balance *uint256.Int
	Code    []byte
	Storage map[gethcommon.Hash]gethcommon.Hash
}

// Result carries the outputs of a successful commitment computation.
type Result struct {
	Root     gethcommon.Hash
	HPHState []byte
	// BranchCount is the number of distinct branch-node prefixes the walk
	// produced. Production reads it as the commitment-domain key count for
	// WriteCommitment.
	BranchCount uint64
	// branches is the live branch store RETAINED past ComputeGenesisRoot
	// (the Updates/etl engine path): production streams it via BranchIterate
	// straight into snap.WriteCommitment and must call CloseBranches when
	// done. Nil once closed.
	branches *branchStore
	// branchSinks is the Direct-Drive Fold's equivalent: the per-worker
	// write-once branch stores (each internally sorted; nibble-disjoint
	// prefixes + the unique root row). BranchIterate k-way-merges them in
	// ascending prefix order. At most one of branches/branchSinks is set (a
	// zero or closed Result has neither; DDF sets branchSinks, possibly empty).
	branchSinks []*streamsort.Store
}

// BranchIterate streams every (prefix, branchData) row in ascending prefix
// order from the retained branch source. Key/value byte slices alias
// Pebble's buffers — copy if retained beyond the callback. Callable
// repeatedly until CloseBranches.
func (r *Result) BranchIterate(yield func(prefix, data []byte) error) error {
	if r.branchSinks != nil {
		return mergedBranchIterate(r.branchSinks, yield)
	}
	if r.branches == nil {
		return errors.New("commitment.Result.BranchIterate: branch source closed or not retained")
	}
	return r.branches.iterate(yield)
}

// CloseBranches releases the retained on-disk branch source. Idempotent;
// safe on a zero Result.
func (r *Result) CloseBranches() {
	for _, s := range r.branchSinks {
		s.Close()
	}
	r.branchSinks = nil
	if r.branches == nil {
		return
	}
	r.branches.close()
	r.branches = nil
}

// EncodeAccountUpdate returns the Update.Encode bytes for an account
// (nonce + balance + codeHash). Callers Put this into the
// commitmentInputStore keyed by plain 20-byte address. ctx.Account
// later Decodes it back into an erigoncommitment.Update.
//
// Splits cleanly from the snapshot SerialiseV3 encoding (Update.Encode
// is HPH's internal wire format; SerialiseV3 is Erigon's state-domain
// .kv value format — different shapes).
func EncodeAccountUpdate(nonce uint64, balance *uint256.Int, code []byte) []byte {
	if len(code) > 0 {
		h := crypto.Keccak256Hash(code)
		return EncodeAccountUpdateCodeHash(nonce, balance, &h)
	}
	return EncodeAccountUpdateCodeHash(nonce, balance, nil)
}

// EncodeAccountUpdateCodeHash is EncodeAccountUpdate with a PRECOMPUTED code
// hash, letting a caller that already keccak'd the code (e.g. the erigon
// writer, which hashes it once for the snapshot SerialiseV3 CodeHash) avoid
// hashing the same bytes a second time. codeHash != nil → the account has code
// with that keccak256 (sets CodeUpdate); nil → no code (empty-code hash).
func EncodeAccountUpdateCodeHash(nonce uint64, balance *uint256.Int, codeHash *gethcommon.Hash) []byte {
	upd := erigoncommitment.Update{
		Flags: erigoncommitment.NonceUpdate | erigoncommitment.BalanceUpdate,
		Nonce: nonce,
	}
	if balance != nil {
		upd.Balance = *balance
	}
	if codeHash != nil {
		upd.CodeHash = erigonHash(*codeHash)
		upd.Flags |= erigoncommitment.CodeUpdate
	} else {
		upd.CodeHash = erigonHash(emptyCodeHash)
	}
	var numBuf [binary.MaxVarintLen64]byte
	return upd.Encode(nil, numBuf[:])
}

// EncodeStorageUpdate returns the Update.Encode bytes for one storage
// slot value. The value is LEFT-aligned into Update.Storage[0:len]
// (matching Erigon's TouchStorage invariant at commitment.go:1746-1753).
//
// Callers Put this into commitmentInputStore keyed by addr(20)||slot(32).
// An all-zero value should NOT be encoded — caller filters out.
func EncodeStorageUpdate(value []byte) []byte {
	trimmed := trimLeadingZeros(value)
	upd := erigoncommitment.Update{
		Flags:      erigoncommitment.StorageUpdate,
		StorageLen: int8(len(trimmed)),
	}
	copy(upd.Storage[:], trimmed)
	var numBuf [binary.MaxVarintLen64]byte
	return upd.Encode(nil, numBuf[:])
}

// NumInputParts is the commitment-input partition count — equal to the
// ParallelHashSort nibble-shard count. The input is split into 16 sub-stores by
// the first nibble of the keccak-hashed key; each of the 16 concurrent workers
// reads exactly ONE sub-store, so the 16 Pebble block caches are disjoint →
// the cross-worker per-block atomic-refcount ping-pong (the profiled 58%
// contention) disappears. Root is unaffected: partitioning only decides which
// file holds a value, never the keccak order or the fold.
const NumInputParts = 16

// InputPart maps a plain key to its sub-store index (0..15): the first nibble of
// KeyToHexNibbleHash(plainKey) — exactly how ParallelHashSort shards its workers
// (upstream: t.nibbles[hashedKey[0]].Collect). For storage keys the hash derives
// from the account address, so an account and all its storage share one part.
func InputPart(plainKey []byte) int {
	return int(erigoncommitment.KeyToHexNibbleHash(plainKey)[0])
}

// InputKeying selects how the commit-input sub-store rows are keyed.
type InputKeying int

const (
	// KeyingPlain: rows keyed by plainKey (addr / addr||slot), value = the
	// bare encoded Update. Used by the Golden-B oracle's engine/plain leg.
	KeyingPlain InputKeying = iota
	// KeyingHashed: rows keyed by HashedKey(plainKey), value =
	// EncodeInputRow(plainKey, update) — each store is in the engine's exact
	// fold order: the default Direct-Drive Fold streams it via cursors, and
	// the engine fallback serves Account/Storage from ONE reused SeekGE
	// Getter. Production always writes this keying.
	KeyingHashed
)

// HashedKey returns KeyToHexNibbleHash(plainKey) — the nibblized keccak
// (64 B account / 128 B storage) that ParallelHashSort sorts by. KeyingHashed
// keys each commit-input sub-store by THIS so a worker's hashed-sorted reads
// are sequential on disk (the reused-SeekGE-Getter fast path); its first
// nibble [0] is the sub-store index (== InputPart).
func HashedKey(plainKey []byte) []byte {
	return erigoncommitment.KeyToHexNibbleHash(plainKey)
}

// EncodeInputRow builds a KeyingHashed commit-input VALUE: the sub-stores are
// keyed by the HASHED key, so the plain key must travel in the value for the
// Touch to recover it (TouchPlainKeyDirect needs the plain key). Layout:
// 1-byte len(plainKey) || plainKey || updateBytes (plainKey is 20 or 52 B).
func EncodeInputRow(plainKey, updateBytes []byte) []byte {
	out := make([]byte, 0, 1+len(plainKey)+len(updateBytes))
	out = append(out, byte(len(plainKey)))
	out = append(out, plainKey...)
	out = append(out, updateBytes...)
	return out
}

// DecodeInputRow splits a value produced by EncodeInputRow back into
// (plainKey, updateBytes). The returned slices alias enc.
func DecodeInputRow(enc []byte) (plainKey, updateBytes []byte, err error) {
	if len(enc) == 0 {
		return nil, nil, errors.New("commitment.DecodeInputRow: empty row")
	}
	n := int(enc[0])
	if 1+n > len(enc) {
		return nil, nil, fmt.Errorf("commitment.DecodeInputRow: plainKey len %d exceeds row %d", n, len(enc))
	}
	return enc[1 : 1+n], enc[1+n:], nil
}

// ComputeGenesisRoot folds the 16 nibble-partitioned commitment-input
// sub-stores (populated by the writeSnapshots streaming loop: one row per
// account + one per non-zero slot, encoded via EncodeAccountUpdate /
// EncodeStorageUpdate) into the genesis root. inputStores must have
// len == NumInputParts and be keyed per `keying` (a layout skew is
// rejected below). Branch rows are retained
// on the Result and streamed to WriteCommitment via BranchIterate.
//
// Two paths: KeyingHashed + STATE_ACTOR_COMMITMENT_DIRECT (default) → the
// Direct-Drive Fold (directdrive.go); otherwise the upstream Updates/etl
// engine below, whose etl spill lands under tmpDir — which must be real
// bind-mounted disk, not tmpfs (the spill is tens of GB at bench scale).
func ComputeGenesisRoot(inputStores []*streamsort.Store, tmpDir string, keying InputKeying) (Result, error) {
	if len(inputStores) != NumInputParts {
		return Result{}, fmt.Errorf("commitment.ComputeGenesisRoot: got %d input stores, want %d", len(inputStores), NumInputParts)
	}
	// Layout sanity: the stores' key shape must match the declared keying —
	// plain keys are 20/52 bytes, hashed 64/128 (disjoint sets). A skew would
	// otherwise fold mis-derived keccaks into a silently wrong root.
	for _, s := range inputStores {
		cur, err := s.NewCursor()
		if err != nil {
			return Result{}, fmt.Errorf("commitment.ComputeGenesisRoot: keying probe: %w", err)
		}
		valid, n := cur.Valid(), 0
		if valid {
			n = len(cur.Key())
		}
		probeErr := cur.Err()
		_ = cur.Close()
		if probeErr != nil {
			return Result{}, fmt.Errorf("commitment.ComputeGenesisRoot: keying probe: %w", probeErr)
		}
		if !valid {
			continue // empty sub-store — probe the next
		}
		if hashedLen := n == 64 || n == 128; hashedLen != (keying == KeyingHashed) {
			return Result{}, fmt.Errorf("commitment.ComputeGenesisRoot: input key length %d does not match declared keying", n)
		}
		break
	}
	// Direct-Drive Fold: single-shot + hashed layout → feed the vendored
	// engine straight from the sorted cursors, skipping Touch/Updates/etl
	// entirely (STATE_ACTOR_COMMITMENT_DIRECT=0 falls back to the engine
	// path below for A/B and emergencies).
	if keying == KeyingHashed && directEnabled() {
		return ComputeGenesisRootDirect(inputStores, tmpDir)
	}
	// Live read-write branch sink shared by all 16 nibble-disjoint workers
	// + the root ctx. Replaces the in-memory mergedBranches map. Created
	// under tmpDir (real disk). On success, OWNERSHIP TRANSFERS to the
	// returned Result (Result.BranchIterate streams it into the commitment
	// .kv writer; Result.CloseBranches releases it) — closed here only on
	// error paths.
	branches, err := newBranchStore(tmpDir)
	if err != nil {
		return Result{}, fmt.Errorf("commitment.ComputeGenesisRoot: branch store: %w", err)
	}
	handedOff := false
	defer func() {
		if !handedOff {
			branches.close()
		}
	}()

	// factory yields a subtreeCtx bound to the SHARED live branch store.
	// Workers write disjoint first-nibble prefixes, so concurrent PutBranch
	// (pebble Set) has no key-overwrite hazard and needs no per-worker merge.
	factory := func() (erigoncommitment.PatriciaContext, func()) {
		sc := &subtreeCtx{inputStores: inputStores, branches: branches, keying: keying, getterPart: -1}
		return sc, func() {
			if sc.getter != nil {
				_ = sc.getter.Close()
			}
		}
	}

	// rootCtx shares the same store; the root-level PutBranch from foldBranch
	// (and the ApplyAndClearInlineDeferredUpdates flush below) lands in it.
	// Its closeFn releases the getter it may have bound (hashed keying).
	rootCtx, rootCtxClose := factory()
	defer rootCtxClose()

	hph := erigoncommitment.NewHexPatriciaHashed(20 /* accountKeyLen */, rootCtx)
	pph := erigoncommitment.NewConcurrentPatriciaHashed(hph, rootCtx)
	defer pph.Close()

	// Single-shot engine: Touch every key into ONE Updates, then one
	// concurrent 16-way Process. ModeDirect TouchPlainKeyDirect records
	// (hashedKey, plainKey) into the per-nibble etl collector + dedup map;
	// the *Update arg is discarded (HPH re-fetches via ctx.Account/Storage).
	// The dedup map is O(total-keys) in RAM — fine for the oracle/fallback
	// scale this path serves; the default DDF path above has no such map.
	upds := erigoncommitment.NewUpdates(erigoncommitment.ModeDirect, tmpDir, erigoncommitment.KeyToHexNibbleHash)
	upds.SetConcurrentCommitment(true) // ParallelHashSort needs ModeDirect + sortPerNibble
	var placeholder erigoncommitment.Update
	for i := range inputStores {
		if err := inputStores[i].Iterate(func(k, v []byte) error {
			// KeyingHashed stores are keyed by the hashed key; the plain key
			// TouchPlainKeyDirect needs travels in the row value.
			if keying == KeyingHashed {
				plainKey, _, e := DecodeInputRow(v)
				if e != nil {
					return fmt.Errorf("decode input row: %w", e)
				}
				upds.TouchPlainKeyDirect(string(plainKey), &placeholder)
			} else {
				upds.TouchPlainKeyDirect(string(k), &placeholder)
			}
			return nil
		}); err != nil {
			return Result{}, fmt.Errorf("commitment.ComputeGenesisRoot: store %d: %w", i, err)
		}
	}
	rootBytes, err := pph.Process(context.Background(), upds, "state-actor-genesis", nil,
		erigoncommitment.WarmupConfig{CtxFactory: factory})
	if err != nil {
		return Result{}, fmt.Errorf("commitment.ComputeGenesisRoot: Process: %w", err)
	}
	// ParallelHashSort returns rootHash WITHOUT flushing the root's deferred
	// branch (SpawnSubTrie disables defer only for the per-nibble mounts) —
	// flush it into the live store here. (hph == pph.RootTrie().)
	if err := hph.ApplyAndClearInlineDeferredUpdates(); err != nil {
		return Result{}, fmt.Errorf("commitment.ComputeGenesisRoot: ApplyAndClearInlineDeferredUpdates: %w", err)
	}

	if len(rootBytes) != 32 {
		return Result{}, fmt.Errorf("commitment.ComputeGenesisRoot: unexpected root hash length %d", len(rootBytes))
	}
	var root gethcommon.Hash
	copy(root[:], rootBytes)

	// HPHState comes from the root trie after all subtrees have folded
	// back into root.grid[0]. RootTrie() returns the same root HPH we
	// constructed above.
	hphState, err := pph.RootTrie().EncodeCurrentState(nil)
	if err != nil {
		return Result{}, fmt.Errorf("commitment.ComputeGenesisRoot: EncodeCurrentState: %w", err)
	}

	// Count distinct prefixes for WriteCommitment's keyCount (a pure
	// sequential scan — no re-Put: the branch store itself is retained on
	// the Result and streamed directly into the commitment .kv writer,
	// eliding the old branchesOut re-sort store, a full extra Pebble
	// write+compaction+read of the ~44 GB branch set at 100 GB scale).
	var branchCount uint64
	if err := branches.iterate(func(prefix, data []byte) error {
		branchCount++
		return nil
	}); err != nil {
		return Result{}, fmt.Errorf("commitment.ComputeGenesisRoot: count branches: %w", err)
	}

	handedOff = true
	return Result{Root: root, HPHState: hphState, BranchCount: branchCount, branches: branches}, nil
}

// ComputeGenesisRootFromAccounts is a backward-compat wrapper for
// small in-memory inputs (tests + the H4 invariance proof). Materialises
// the slice into a temp streamsort + calls the streaming
// ComputeGenesisRoot. Not for production at bench scale.
func ComputeGenesisRootFromAccounts(accounts []Account) (Result, error) {
	return ComputeGenesisRootFromAccountsKeyed(accounts, KeyingPlain)
}

// ComputeGenesisRootFromAccountsKeyed is ComputeGenesisRootFromAccounts with
// an explicit input keying, so tests can drive BOTH layouts over the same
// alloc and assert byte-identical results (the KeyingHashed safety gate).
func ComputeGenesisRootFromAccountsKeyed(accounts []Account, keying InputKeying) (Result, error) {
	// 16 nibble-partitioned input sub-stores (matches production).
	stores := make([]*streamsort.Store, 0, NumInputParts)
	closeAll := func() {
		for _, s := range stores {
			s.Close()
		}
	}
	for i := 0; i < NumInputParts; i++ {
		s, err := streamsort.New("")
		if err != nil {
			closeAll()
			return Result{}, fmt.Errorf("ComputeGenesisRootFromAccounts: streamsort.New[%d]: %w", i, err)
		}
		stores = append(stores, s)
	}
	defer closeAll()

	put := func(part int, plainKey, updateBytes []byte) error {
		if keying == KeyingHashed {
			return stores[part].Put(HashedKey(plainKey), EncodeInputRow(plainKey, updateBytes))
		}
		return stores[part].Put(plainKey, updateBytes)
	}
	for _, a := range accounts {
		var balance *uint256.Int
		if a.Balance != nil {
			balance = a.Balance
		}
		acctBytes := EncodeAccountUpdate(a.Nonce, balance, a.Code)
		part := InputPart(a.Address[:]) // storage shares this part (hash derives from addr)
		if err := put(part, a.Address[:], acctBytes); err != nil {
			return Result{}, fmt.Errorf("ComputeGenesisRootFromAccounts: put account %s: %w", a.Address.Hex(), err)
		}
		// Storage entries keyed by addr||slot. Skip all-zero values.
		for slot, val := range a.Storage {
			trimmed := trimLeadingZeros(val[:])
			if len(trimmed) == 0 {
				continue
			}
			composite := make([]byte, 0, 52)
			composite = append(composite, a.Address[:]...)
			composite = append(composite, slot[:]...)
			storBytes := EncodeStorageUpdate(val[:])
			if err := put(part, composite, storBytes); err != nil {
				return Result{}, fmt.Errorf("ComputeGenesisRootFromAccounts: put storage %s/%s: %w", a.Address.Hex(), slot.Hex(), err)
			}
		}
	}
	for i := range stores {
		if err := stores[i].Finalize(); err != nil {
			return Result{}, fmt.Errorf("ComputeGenesisRootFromAccounts: Finalize[%d]: %w", i, err)
		}
	}
	// Tests/H4-invariance use tiny inputs; "" (os.TempDir) is fine here.
	// The retained branch source transfers to the caller: CloseBranches
	// when done (the input stores close here — the branch source is an
	// independent store and outlives them).
	return ComputeGenesisRoot(stores, "", keying)
}

// subtreeCtx implements erigoncommitment.PatriciaContext for the ENGINE
// fallback: one per ParallelHashSort worker (+ the root HPH), all sharing
// the live branch store. Workers write disjoint first-nibble prefixes, so
// concurrent PutBranch/Branch are safe with no overwrite ambiguity.
type subtreeCtx struct {
	// inputStores is the 16 nibble-partitioned commitment-input sub-stores,
	// keyed per `keying`. Account/Storage route by InputPart(plainKey); a
	// concurrent worker only ever hits one part, so its Pebble cache is
	// uncontended.
	inputStores []*streamsort.Store
	branches    *branchStore
	keying      InputKeying
	// getter is a lazily-opened reused SeekGE cursor on this ctx's ONE
	// sub-store — the KeyingHashed fast path (a 16-way worker reads its
	// nibble in hashed-sorted order, so SeekGE stays in-file, skipping the
	// per-Get iterator build). getterPart tracks its binding: -1 = none yet,
	// 0..15 = bound, -2 = retired (a nibble-spanning ctx — the root/serial
	// engine — falls back to db.Get). Closed by the factory's closeFn.
	getter     *streamsort.Getter
	getterPart int
}

// lookup returns the encoded Update bytes for plainKey (nil if absent),
// routing to the right nibble sub-store per the ctx's keying.
func (c *subtreeCtx) lookup(plainKey []byte) ([]byte, error) {
	part := InputPart(plainKey)
	if c.keying == KeyingPlain {
		return c.inputStores[part].Get(plainKey)
	}
	// KeyingHashed: rows keyed by the hashed key, value wraps the plain key.
	hashedKey := HashedKey(plainKey)
	var (
		row []byte
		err error
	)
	switch c.getterPart {
	case part:
		row, err = c.getter.Get(hashedKey)
	case -1:
		g, gerr := c.inputStores[part].NewGetter()
		if gerr != nil {
			return nil, gerr
		}
		c.getter, c.getterPart = g, part
		row, err = c.getter.Get(hashedKey)
	default:
		// A part different from the getter's binding → this ctx spans
		// nibbles (the root/serial ctx). Retire the getter; use db.Get.
		if c.getter != nil {
			_ = c.getter.Close()
			c.getter = nil
		}
		c.getterPart = -2
		row, err = c.inputStores[part].Get(hashedKey)
	}
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, nil
	}
	_, updateBytes, derr := DecodeInputRow(row)
	if derr != nil {
		return nil, derr
	}
	return updateBytes, nil
}

func (c *subtreeCtx) Branch(prefix []byte) ([]byte, erigonkv.Step, error) {
	data, err := c.branches.get(prefix)
	if err != nil {
		return nil, 0, err
	}
	return data, 0, nil
}

func (c *subtreeCtx) PutBranch(prefix []byte, data []byte, prevData []byte) error {
	// pebble copies key+value internally, so no manual copy is needed.
	return c.branches.set(prefix, data)
}

func (c *subtreeCtx) Account(plainKey []byte) (*erigoncommitment.Update, error) {
	enc, err := c.lookup(plainKey)
	if err != nil {
		return nil, fmt.Errorf("commitment.subtreeCtx.Account: lookup(%x): %w", plainKey, err)
	}
	if enc == nil {
		u := new(erigoncommitment.Update)
		u.Flags = erigoncommitment.DeleteUpdate
		return u, nil
	}
	var u erigoncommitment.Update
	pos, err := u.Decode(enc, 0)
	if err != nil {
		return nil, fmt.Errorf("commitment.subtreeCtx.Account: decode plainKey=%x: %w", plainKey, err)
	}
	if pos != len(enc) {
		return nil, fmt.Errorf("commitment.subtreeCtx.Account: trailing bytes after decode")
	}
	if u.Flags&erigoncommitment.StorageUpdate != 0 {
		return nil, errors.New("commitment.subtreeCtx.Account: read storage entry via Account()")
	}
	return &u, nil
}

func (c *subtreeCtx) Storage(plainKey []byte) (*erigoncommitment.Update, error) {
	enc, err := c.lookup(plainKey)
	if err != nil {
		return nil, fmt.Errorf("commitment.subtreeCtx.Storage: lookup(%x): %w", plainKey, err)
	}
	if enc == nil {
		u := new(erigoncommitment.Update)
		u.Flags = erigoncommitment.DeleteUpdate
		return u, nil
	}
	var u erigoncommitment.Update
	pos, err := u.Decode(enc, 0)
	if err != nil {
		return nil, fmt.Errorf("commitment.subtreeCtx.Storage: decode plainKey=%x: %w", plainKey, err)
	}
	if pos != len(enc) {
		return nil, fmt.Errorf("commitment.subtreeCtx.Storage: trailing bytes after decode")
	}
	return &u, nil
}

var emptyCodeHash = gethcommon.Hash{
	0xc5, 0xd2, 0x46, 0x01, 0x86, 0xf7, 0x23, 0x3c,
	0x92, 0x7e, 0x7d, 0xb2, 0xdc, 0xc7, 0x03, 0xc0,
	0xe5, 0x00, 0xb6, 0x53, 0xca, 0x82, 0x27, 0x3b,
	0x7b, 0xfa, 0xd8, 0x04, 0x5d, 0x85, 0xa4, 0x70,
}

func trimLeadingZeros(b []byte) []byte {
	i := 0
	for i < len(b) && b[i] == 0 {
		i++
	}
	return b[i:]
}
