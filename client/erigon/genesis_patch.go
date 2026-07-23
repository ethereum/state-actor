//go:build cgo_erigon

// genesis_patch.go drives MDBX directly (erigontech/mdbx-go is cgo-only,
// with no pure-Go fallback), so it is gated behind cgo_erigon — otherwise
// it would break the pure-Go default build (CGO_ENABLED=0, e.g.
// Dockerfile.geth / the docker-release base image).

package erigon

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"path/filepath"

	"github.com/erigontech/mdbx-go/mdbx"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
	internalerigon "github.com/ethereum/state-actor/internal/erigon"
)

// MDBX env geometry — mirrors Erigon's kv_mdbx.go defaults so the
// daemon's compatibility check passes when it later reopens chaindata.
const (
	headerPatchPageSize        = 4096
	headerPatchMapSize         = 4 * 1024 * 1024 * 1024 * 1024
	headerPatchGrowthStep      = 4 * 1024 * 1024 * 1024
	headerPatchMaxDBs     uint = 200
)

// Bucket names — verbatim from upstream Erigon's kv.* constants
// (db/kv/tables.go on commit 14273f79a6). Note: kv.Headers's string is
// "Header" (singular) and kv.HeadBlockKey / kv.HeadHeaderKey are
// SINGLE-KEY tables whose own bucket name doubles as the only key in
// them — that's upstream's convention, not a state-actor invention.
const (
	bucketHeaders         = "Header"
	bucketHeaderCanonical = "CanonicalHeader"
	bucketHeaderNumber    = "HeaderNumber"
	bucketHeaderTD        = "HeadersTotalDifficulty"
	bucketBlockBody       = "BlockBody"
	bucketLastBlock       = "LastBlock"
	bucketLastHeader      = "LastHeader"
	bucketConfig          = "Config"
	bucketMaxTxNum        = "MaxTxNum"
	bucketSequence        = "Sequence"
)

// keyEthTxSequence is the kv.Sequence key for the kv.EthTx txn-id
// allocator — upstream's convention is the table's own name string
// (kv.EthTx = "BlockTransaction", db/kv/tables.go).
const keyEthTxSequence = "BlockTransaction"

// openChaindataEnv opens the chaindata MDBX env at <dbPath>/chaindata
// with the geometry erigon's compatibility check expects. Caller owns
// env.Close().
func openChaindataEnv(dbPath string) (*mdbx.Env, error) {
	chaindataDir := filepath.Join(dbPath, "chaindata")

	env, err := mdbx.NewEnv(mdbx.Label("chaindata"))
	if err != nil {
		return nil, fmt.Errorf("mdbx.NewEnv: %w", err)
	}
	if err := env.SetOption(mdbx.OptMaxDB, uint64(headerPatchMaxDBs)); err != nil {
		env.Close()
		return nil, fmt.Errorf("mdbx.SetOption(MaxDB): %w", err)
	}
	if err := env.SetGeometry(-1, -1, headerPatchMapSize, headerPatchGrowthStep, -1, headerPatchPageSize); err != nil {
		env.Close()
		return nil, fmt.Errorf("mdbx.SetGeometry: %w", err)
	}
	if err := env.Open(chaindataDir, mdbx.Durable, 0o644); err != nil {
		env.Close()
		return nil, fmt.Errorf("mdbx.Open(%s): %w", chaindataDir, err)
	}
	return env, nil
}

// strictGet wraps txn.Get with a descriptive error on missing rows.
// Returns a wrapped error that includes the bucket label + key shape
// when mdbx.IsNotFound(err) is true. Strict mode: a missing required
// row is a fatal error, signalling that upstream's genesis-write set
// has drifted from what patchGenesisHeaderStateRoot expects.
func strictGet(txn *mdbx.Txn, dbi mdbx.DBI, key []byte, label string) ([]byte, error) {
	v, err := txn.Get(dbi, key)
	if err != nil {
		if mdbx.IsNotFound(err) {
			return nil, fmt.Errorf("%s: key %x not found (genesis-write set drift?)", label, key)
		}
		return nil, fmt.Errorf("%s: Get(%x): %w", label, key, err)
	}
	return v, nil
}

// patchGenesisHeaderStateRoot rewrites block 0's header.stateRoot in
// the chaindata MDBX env AND re-keys every dependent table to use the
// new block hash. This is the only way to keep chaindata internally
// consistent after mutating Header.Root, because Header.Hash() =
// keccak256(rlp(Header)) — mutating Root changes the hash, so EVERY
// table that was keyed by or stored the old hash must be rewritten.
//
// 8 tables are mutated, all atomically in a single MDBX env.Update():
//
//  1. CanonicalHeader  [BE(0)]            value: oldHash       -> newHash
//  2. Header           [BE(0) || hash]    REKEY old->new + new RLP value
//  3. HeaderNumber     [hash]             REKEY old->new (value BE(0) preserved)
//  4. HeadersTotalDifficulty [BE(0) || hash] REKEY old->new (RLP TD preserved)
//  5. BlockBody        [BE(0) || hash]    REKEY old->new + TxCount 2 -> StepSize
//  6. LastBlock        ["LastBlock"]      value: oldHash       -> newHash
//  7. LastHeader       ["LastHeader"]     value: oldHash       -> newHash
//  8. Config           [hash]             REKEY old->new (JSON chain.Config preserved)
//
// MaxTxNum's keys are BE(blockNum) alone (no hash component), so the
// Root patch doesn't invalidate them. We DO, however, overwrite
// MaxTxNum[0] from 1 to StepSize-1 ("fat genesis", step 9 below) — see
// the rationale on the step-9 write. Steps 5 (TxCount) and 10
// (Sequence[EthTx]) keep the fat genesis SELF-HEALING and the txn-id
// space aligned with it — see those steps for the rationale.
//
// Required because `erigon init` writes block 0 with whatever Root its
// empty genesis alloc produced. State-actor's snapshot writer then
// emits all bloat into the snapshot tier — the daemon's first FCU
// recomputes commitment over visible (snapshot) state and rejects the
// erigon-init root. This patch overwrites that Root with the
// HPH-over-everything value computed by commitment.ComputeGenesisRoot.
//
// Without the full re-key (only Header[oldKey] = newRLP, the
// pre-2026-05-30 behavior), the resulting MDBX is four-way
// inconsistent: 7 other tables still reference oldHash; the new RLP
// sits under the wrong Header key; no chain lookup by newHash
// resolves. engine_forkchoiceUpdated falls through to SYNCING and
// the daemon never opens RoSnapshots.ready.
//
// Strict mode: returns a fatal error if any of the touched tables is
// missing its expected row, if the genesis body deviates from the
// erigon-init shape (BaseTxnID=0, TxCount=2), or if Sequence[EthTx]
// is absent. This catches upstream genesis-write-set drift (a new
// Erigon pin removing or renaming a table, or changing the genesis
// body/sequence conventions) at the first bench instead of silently
// producing a broken chaindata.
func patchGenesisHeaderStateRoot(dbPath string, root common.Hash) error {
	env, err := openChaindataEnv(dbPath)
	if err != nil {
		return err
	}
	defer env.Close()

	return env.Update(func(txn *mdbx.Txn) error {
		canonicalDBI, err := txn.OpenDBI(bucketHeaderCanonical, 0, nil, nil)
		if err != nil {
			return fmt.Errorf("OpenDBI(%s): %w", bucketHeaderCanonical, err)
		}
		headersDBI, err := txn.OpenDBI(bucketHeaders, 0, nil, nil)
		if err != nil {
			return fmt.Errorf("OpenDBI(%s): %w", bucketHeaders, err)
		}
		headerNumDBI, err := txn.OpenDBI(bucketHeaderNumber, 0, nil, nil)
		if err != nil {
			return fmt.Errorf("OpenDBI(%s): %w", bucketHeaderNumber, err)
		}
		tdDBI, err := txn.OpenDBI(bucketHeaderTD, 0, nil, nil)
		if err != nil {
			return fmt.Errorf("OpenDBI(%s): %w", bucketHeaderTD, err)
		}
		bodyDBI, err := txn.OpenDBI(bucketBlockBody, 0, nil, nil)
		if err != nil {
			return fmt.Errorf("OpenDBI(%s): %w", bucketBlockBody, err)
		}
		lastBlockDBI, err := txn.OpenDBI(bucketLastBlock, 0, nil, nil)
		if err != nil {
			return fmt.Errorf("OpenDBI(%s): %w", bucketLastBlock, err)
		}
		lastHeaderDBI, err := txn.OpenDBI(bucketLastHeader, 0, nil, nil)
		if err != nil {
			return fmt.Errorf("OpenDBI(%s): %w", bucketLastHeader, err)
		}
		configDBI, err := txn.OpenDBI(bucketConfig, 0, nil, nil)
		if err != nil {
			return fmt.Errorf("OpenDBI(%s): %w", bucketConfig, err)
		}

		blockNumKey := make([]byte, 8)
		binary.BigEndian.PutUint64(blockNumKey, 0)

		oldHash, err := strictGet(txn, canonicalDBI, blockNumKey, bucketHeaderCanonical)
		if err != nil {
			return err
		}
		if len(oldHash) != 32 {
			return fmt.Errorf("%s[0]: len=%d, want 32", bucketHeaderCanonical, len(oldHash))
		}
		oldHeadersKey := append(append(make([]byte, 0, 8+32), blockNumKey...), oldHash...)

		headerRLP, err := strictGet(txn, headersDBI, oldHeadersKey, bucketHeaders)
		if err != nil {
			return err
		}
		var h types.Header
		if err := rlp.DecodeBytes(headerRLP, &h); err != nil {
			return fmt.Errorf("RLP decode block-0 header: %w", err)
		}
		h.Root = root
		newRLP, err := rlp.EncodeToBytes(&h)
		if err != nil {
			return fmt.Errorf("RLP encode patched block-0 header: %w", err)
		}
		newHash := h.Hash()
		newHeadersKey := append(append(make([]byte, 0, 8+32), blockNumKey...), newHash[:]...)

		// 1. Header — rekey + new RLP value.
		if err := txn.Del(headersDBI, oldHeadersKey, nil); err != nil {
			return fmt.Errorf("Del(%s, BE(0)||oldHash): %w", bucketHeaders, err)
		}
		if err := txn.Put(headersDBI, newHeadersKey, newRLP, 0); err != nil {
			return fmt.Errorf("Put(%s, BE(0)||newHash): %w", bucketHeaders, err)
		}

		// 2. CanonicalHeader — overwrite the singleton-by-blockNum value.
		if err := txn.Put(canonicalDBI, blockNumKey, newHash[:], 0); err != nil {
			return fmt.Errorf("Put(%s, BE(0)): %w", bucketHeaderCanonical, err)
		}

		// 3. HeaderNumber — rekey (delete oldHash entry, put newHash entry).
		storedBlockNum, err := strictGet(txn, headerNumDBI, oldHash, bucketHeaderNumber)
		if err != nil {
			return err
		}
		if !bytes.Equal(storedBlockNum, blockNumKey) {
			return fmt.Errorf("%s[oldHash]: stored blockNum=%x, want %x", bucketHeaderNumber, storedBlockNum, blockNumKey)
		}
		if err := txn.Del(headerNumDBI, oldHash, nil); err != nil {
			return fmt.Errorf("Del(%s, oldHash): %w", bucketHeaderNumber, err)
		}
		if err := txn.Put(headerNumDBI, newHash[:], blockNumKey, 0); err != nil {
			return fmt.Errorf("Put(%s, newHash): %w", bucketHeaderNumber, err)
		}

		// 4. HeadersTotalDifficulty — rekey (preserve RLP TD value).
		tdVal, err := strictGet(txn, tdDBI, oldHeadersKey, bucketHeaderTD)
		if err != nil {
			return err
		}
		if err := txn.Del(tdDBI, oldHeadersKey, nil); err != nil {
			return fmt.Errorf("Del(%s, BE(0)||oldHash): %w", bucketHeaderTD, err)
		}
		if err := txn.Put(tdDBI, newHeadersKey, tdVal, 0); err != nil {
			return fmt.Errorf("Put(%s, BE(0)||newHash): %w", bucketHeaderTD, err)
		}

		// 5. BlockBody — rekey + fatten TxCount from 2 to StepSize.
		//
		// The TxCount rewrite makes the fat genesis (step 9) SELF-HEALING.
		// Since erigon 2e41aa8308 (PR #22344, 2026-07-09), an
		// FCU(head=genesis) — benchmarkoor's bootstrap FCU, because our
		// head IS block 0 — flows through the general reorg path:
		// updateForkChoice runs TxNums.Truncate(tx, 0) (wipes the whole
		// MaxTxNum table) then AppendCanonicalTxNums(tx, 0), which
		// re-derives MaxTxNum[0] = BodyForStorage.TxCount - 1 from THIS
		// row. With upstream's TxCount=2 that rebuilt MaxTxNum[0]=1,
		// clobbering step 9's StepSize-1 and failing the Execution stage
		// with "seems broken TxNums index not filled" (commitment anchor
		// txNum=StepSize-1 no longer maps to a block). With
		// TxCount=StepSize the rebuild reproduces StepSize-1 exactly, so
		// the body row and the MaxTxNum row agree on genesis's txNum
		// span no matter which one erigon trusts. The body-derived
		// TxnumReader fallback (BaseTxnID + TxCount - 1, block_reader.go)
		// yields the same value. Safe for readers: ReadBody only panics
		// on TxCount < 2, and CanonicalTransactions tolerates a claimed
		// range with no kv.EthTx entries (short read; step 10 keeps the
		// range empty). Pre-2e41aa8308 daemons never rebuild block 0's
		// entry, so the fattened TxCount is inert there.
		bodyVal, err := strictGet(txn, bodyDBI, oldHeadersKey, bucketBlockBody)
		if err != nil {
			return err
		}
		fatBodyVal, err := fattenGenesisBody(bodyVal)
		if err != nil {
			return fmt.Errorf("%s[BE(0)||oldHash]: %w", bucketBlockBody, err)
		}
		if err := txn.Del(bodyDBI, oldHeadersKey, nil); err != nil {
			return fmt.Errorf("Del(%s, BE(0)||oldHash): %w", bucketBlockBody, err)
		}
		if err := txn.Put(bodyDBI, newHeadersKey, fatBodyVal, 0); err != nil {
			return fmt.Errorf("Put(%s, BE(0)||newHash): %w", bucketBlockBody, err)
		}

		// 6. LastBlock — overwrite singleton value (table-name == sole key).
		if err := txn.Put(lastBlockDBI, []byte(bucketLastBlock), newHash[:], 0); err != nil {
			return fmt.Errorf("Put(%s, %q): %w", bucketLastBlock, bucketLastBlock, err)
		}

		// 7. LastHeader — overwrite singleton value (table-name == sole key).
		if err := txn.Put(lastHeaderDBI, []byte(bucketLastHeader), newHash[:], 0); err != nil {
			return fmt.Errorf("Put(%s, %q): %w", bucketLastHeader, bucketLastHeader, err)
		}

		// 8. Config — rekey the 32-byte hash entry (preserve JSON value).
		// NOTE: Config also holds an entry under the kv.GenesisKey string
		// (the full genesis-JSON), keyed by a fixed string rather than the
		// hash. We MUST NOT touch that one. Reading Config[oldHash]
		// targets the 32-byte hash entry specifically.
		cfgVal, err := strictGet(txn, configDBI, oldHash, bucketConfig)
		if err != nil {
			return err
		}
		if err := txn.Del(configDBI, oldHash, nil); err != nil {
			return fmt.Errorf("Del(%s, oldHash): %w", bucketConfig, err)
		}
		if err := txn.Put(configDBI, newHash[:], cfgVal, 0); err != nil {
			return fmt.Errorf("Put(%s, newHash): %w", bucketConfig, err)
		}

		// 9. MaxTxNum[0] — "fat genesis". erigon init wrote MaxTxNum[0]=1
		// (genesis occupies txNums [0,1]). We overwrite it to StepSize-1
		// so genesis occupies the ENTIRE frozen step 0 ([0, StepSize)).
		// Combined with the commitment anchor txNum=StepSize-1
		// (snapshot_cgo.go), this makes the daemon resume the first live
		// block (block 1) at txNum=StepSize — STEP 1, one step above the
		// frozen [0,1) commitment file. Block 1's commitment then writes
		// to the MDBX hot tier at step 1, which WINS the getLatestFromDb
		// EndTxNum gate (domain.go:1582: an MDBX step-S write beats the
		// frozen file iff lastTxNumOfStep(S) >= files.EndTxNum()=StepSize,
		// i.e. S>=1) instead of being shadowed. This is the no-patch fix
		// for the block-2 "wrong trie root" stall: keep the bloat in flat
		// files, keep only the advancing commitment in MDBX. The genesis
		// block body still has 0 real txs; only the txNum bookkeeping is
		// inflated, which the daemon reads from MaxTxNum (MDBX) directly
		// (block_reader.go:1523 tries MDBX before the snapshot body).
		// Survives daemon boot: WriteGenesisBlock's already-written branch
		// (genesis_write.go:173-242) does NOT re-append TxNums.
		maxTxNumDBI, err := txn.OpenDBI(bucketMaxTxNum, 0, nil, nil)
		if err != nil {
			return fmt.Errorf("OpenDBI(%s): %w", bucketMaxTxNum, err)
		}
		fatGenesisMaxTxNum := make([]byte, 8)
		binary.BigEndian.PutUint64(fatGenesisMaxTxNum, internalerigon.StepSize-1)
		if err := txn.Put(maxTxNumDBI, blockNumKey, fatGenesisMaxTxNum, 0); err != nil {
			return fmt.Errorf("Put(%s, BE(0)=StepSize-1): %w", bucketMaxTxNum, err)
		}

		// 10. Sequence[EthTx] — bump the txn-id allocator from 2 to
		// StepSize so runtime blocks' BaseTxnID starts where genesis's
		// fattened claim (step 5: BaseTxnID=0, TxCount=StepSize) ends.
		// Without this, block 1's body would get BaseTxnID=2 and its
		// real transactions would sit inside genesis's claimed
		// [0, StepSize) txn-id range, so body-range readers
		// (CanonicalTransactions / RawTransactionsRange) walking
		// genesis's range would pick up foreign txns (e.g.
		// eth_getBlockByNumber(0, true) listing block-1 transactions).
		// With the bump, genesis's claimed range is empty in kv.EthTx
		// and both id spaces stay aligned: block 1 starts at StepSize
		// in the body txn-id space AND at MaxTxNum[0]+1 = StepSize in
		// the txNum space. Strict read first: erigon init's
		// WriteBody -> IncrementSequence(kv.EthTx, 2) must have created
		// the entry; its absence signals genesis-write-set drift.
		seqDBI, err := txn.OpenDBI(bucketSequence, 0, nil, nil)
		if err != nil {
			return fmt.Errorf("OpenDBI(%s): %w", bucketSequence, err)
		}
		if _, err := strictGet(txn, seqDBI, []byte(keyEthTxSequence), bucketSequence); err != nil {
			return err
		}
		fatSequence := make([]byte, 8)
		binary.BigEndian.PutUint64(fatSequence, internalerigon.StepSize)
		if err := txn.Put(seqDBI, []byte(keyEthTxSequence), fatSequence, 0); err != nil {
			return fmt.Errorf("Put(%s, %q=StepSize): %w", bucketSequence, keyEthTxSequence, err)
		}

		return nil
	})
}

// bodyForStoragePrefix mirrors the leading fields of upstream
// types.BodyForStorage (execution/types/block.go): BaseTxnID uint64,
// TxCount uint32. Tail round-trips whatever follows (Uncles,
// Withdrawals, and any field upstream appends later — e.g.
// BlockAccessList on EIP-7928 branches) byte-for-byte, so the patch
// works against any BodyForStorage schema that keeps the two leading
// fields.
type bodyForStoragePrefix struct {
	BaseTxnID uint64
	TxCount   uint32
	Tail      []rlp.RawValue `rlp:"tail"`
}

// fattenGenesisBody rewrites the genesis BodyForStorage's TxCount from
// upstream's 2 (leading + trailing system txn, zero real txns) to
// StepSize, preserving every other byte. Strict mode: any deviation
// from the erigon-init genesis shape (BaseTxnID=0, TxCount=2) is a
// fatal error signalling upstream genesis-write-set drift.
func fattenGenesisBody(raw []byte) ([]byte, error) {
	var body bodyForStoragePrefix
	if err := rlp.DecodeBytes(raw, &body); err != nil {
		return nil, fmt.Errorf("RLP decode block-0 BodyForStorage: %w", err)
	}
	if body.BaseTxnID != 0 || body.TxCount != 2 {
		return nil, fmt.Errorf("block-0 BodyForStorage: BaseTxnID=%d TxCount=%d, want 0/2 (genesis-write set drift?)", body.BaseTxnID, body.TxCount)
	}
	body.TxCount = uint32(internalerigon.StepSize)
	out, err := rlp.EncodeToBytes(&body)
	if err != nil {
		return nil, fmt.Errorf("RLP encode fattened block-0 BodyForStorage: %w", err)
	}
	return out, nil
}
