//go:build cgo_besu

package besu

import (
	"context"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/params"
	"github.com/linxGnu/grocksdb"

	"github.com/ethereum/state-actor/genesis"
	"github.com/ethereum/state-actor/internal/besu/keys"
	"github.com/ethereum/state-actor/internal/genesisheader"
	"github.com/ethereum/state-actor/internal/testhex"
)

// TestDifferentialOracle replays Besu-source-derived genesis fixtures
// through state-actor's writer and compares the resulting on-disk world
// state root (and genesis block hash for genesisNonce) against hashes
// pinned by Besu's own GenesisStateTest.java.
//
// This is the load-bearing parity test for the Besu adapter — if it
// passes, our wire format byte-for-byte matches what Besu would write
// for the same alloc.
//
// Requires: -tags cgo_besu and librocksdb. Run via:
//
//	make test-besu-oracle
func TestDifferentialOracle(t *testing.T) {
	t.Run("Genesis1", testDifferentialOracleGenesis1)
	t.Run("GenesisNonce", testDifferentialOracleGenesisNonce)
}

// testDifferentialOracleGenesis1 — 2-account EOA-only alloc.
// Expected stateRoot per GenesisStateTest.java:74-75.
func testDifferentialOracleGenesis1(t *testing.T) {
	const wantStateRoot = "0x92683e6af0f8a932e5fe08c870f2ae9d287e39d4518ec544b0be451f1035fd39"

	allocs := loadFixtureAllocs(t, "testdata/genesis1.json")

	tmpDir := t.TempDir()
	db, err := openBesuDB(tmpDir)
	if err != nil {
		t.Fatalf("openBesuDB: %v", err)
	}
	defer db.Close()

	sink := newNodeSink(db)
	defer sink.Close()

	rootHash, _, err := writeGenesisAllocAccounts(context.Background(), db, sink, allocs)
	if err != nil {
		t.Fatalf("writeGenesisAllocAccounts: %v", err)
	}

	if got := rootHash.Hex(); got != wantStateRoot {
		t.Fatalf("genesis1 stateRoot mismatch:\n  got:  %s\n  want: %s", got, wantStateRoot)
	}

	// Verify the on-disk worldRoot sentinel reflects the same value once
	// SaveWorldState fires.
	if err := sink.SaveWorldState(common.Hash{}, rootHash, nil); err != nil {
		t.Fatalf("SaveWorldState: %v", err)
	}
	verifyWorldRootOnDisk(t, db, rootHash)
}

// testDifferentialOracleGenesisNonce — 2-account alloc with one contract
// (code 0x6010ff, nonce=3) and chain config homestead/eip150/eip158/byzantium/
// constantinople = 0. Expected genesis BLOCK HASH per GenesisStateTest.java:157-159.
func testDifferentialOracleGenesisNonce(t *testing.T) {
	const wantBlockHash = "0x36750291f1a8429aeb553a790dc2d149d04dbba0ca4cfc7fd5eb12d478117c9f"

	g, allocs := loadFixtureGenesis(t, "testdata/genesisNonce.json")

	tmpDir := t.TempDir()
	db, err := openBesuDB(tmpDir)
	if err != nil {
		t.Fatalf("openBesuDB: %v", err)
	}
	defer db.Close()

	sink := newNodeSink(db)
	defer sink.Close()

	rootHash, _, err := writeGenesisAllocAccounts(context.Background(), db, sink, allocs)
	if err != nil {
		t.Fatalf("writeGenesisAllocAccounts: %v", err)
	}

	header := genesisheader.Build(g, 0, common.Hash{}, rootHash)
	if got := header.Hash().Hex(); got != wantBlockHash {
		t.Fatalf("genesisNonce blockHash mismatch:\n  got:  %s\n  want: %s\n  (computed stateRoot: %s)",
			got, wantBlockHash, rootHash.Hex())
	}
}

// loadFixtureAllocs loads the alloc map from a Besu genesis JSON. Handles
// both 0x-prefixed hex and bare-decimal balances (Besu source fixtures use
// each in different files); state-actor's geth-shaped genesis.LoadGenesis
// would refuse genesis1.json's non-standard config block.
func loadFixtureAllocs(t *testing.T, path string) map[common.Address]genesis.GenesisAccount {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("abs %s: %v", path, err)
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var rg struct {
		Alloc map[string]struct {
			Balance string                      `json:"balance"`
			Nonce   string                      `json:"nonce,omitempty"`
			Code    string                      `json:"code,omitempty"`
			Storage map[common.Hash]common.Hash `json:"storage,omitempty"`
		} `json:"alloc"`
	}
	if err := json.Unmarshal(raw, &rg); err != nil {
		t.Fatalf("unmarshal alloc: %v", err)
	}
	out := make(map[common.Address]genesis.GenesisAccount, len(rg.Alloc))
	for addrHex, a := range rg.Alloc {
		bal, ok := new(big.Int).SetString(a.Balance, 0)
		if !ok {
			t.Fatalf("parse balance %q for %s", a.Balance, addrHex)
		}
		var nonce hexutil.Uint64
		if a.Nonce != "" {
			n, ok := new(big.Int).SetString(a.Nonce, 0)
			if !ok || !n.IsUint64() {
				t.Fatalf("parse nonce %q for %s", a.Nonce, addrHex)
			}
			nonce = hexutil.Uint64(n.Uint64())
		}
		var code hexutil.Bytes
		if a.Code != "" && a.Code != "0x" {
			b, err := hexutil.Decode(a.Code)
			if err != nil {
				t.Fatalf("parse code %q: %v", a.Code, err)
			}
			code = b
		}
		out[common.HexToAddress(addrHex)] = genesis.GenesisAccount{
			Balance: (*hexutil.Big)(bal),
			Nonce:   nonce,
			Code:    code,
			Storage: a.Storage,
		}
	}
	return out
}

// loadFixtureGenesis loads both the alloc and a minimal *genesis.Genesis
// from a Besu fixture JSON (for header construction in genesisNonce
// test). After the writer migration, header construction goes through
// internal/genesisheader.Build which takes *genesis.Genesis directly.
//
// Fixtures are pre-London (homestead/constantinople-era), so we attach
// an empty *params.ChainConfig with NO fork blocks/times set —
// IsLondon/IsShanghai/IsCancun/etc all return false, so Build emits a
// pre-London header (no BaseFee, no WithdrawalsHash, no Cancun blob
// fields, no RequestsHash). That matches what besu's
// GenesisState.buildHeader produces for these fixtures — load-bearing
// for the differential oracle's pinned hashes.
//
// genesisheader.Build panics on g.Config == nil, hence the empty
// non-nil ChainConfig (vs leaving Config nil).
func loadFixtureGenesis(t *testing.T, path string) (*genesis.Genesis, map[common.Address]genesis.GenesisAccount) {
	t.Helper()
	allocs := loadFixtureAllocs(t, path)

	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("abs %s: %v", path, err)
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	type rawHdr struct {
		Coinbase   string `json:"coinbase"`
		Difficulty string `json:"difficulty"`
		ExtraData  string `json:"extraData"`
		GasLimit   string `json:"gasLimit"`
		MixHash    string `json:"mixHash"`
		Nonce      string `json:"nonce"`
		Timestamp  string `json:"timestamp"`
	}
	var h rawHdr
	if err := json.Unmarshal(raw, &h); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}

	g := &genesis.Genesis{Config: &params.ChainConfig{}}
	g.Coinbase = common.HexToAddress(h.Coinbase)
	g.Difficulty = (*hexutil.Big)(testhex.Big(t, "difficulty", h.Difficulty))
	if h.ExtraData != "" && h.ExtraData != "0x" {
		ed, err := hexutil.Decode(h.ExtraData)
		if err != nil {
			t.Fatalf("parse extraData: %v", err)
		}
		g.ExtraData = hexutil.Bytes(ed)
	}
	g.GasLimit = hexutil.Uint64(testhex.Uint64(t, "gasLimit", h.GasLimit))
	g.Mixhash = common.HexToHash(h.MixHash)
	g.Nonce = hexutil.Uint64(testhex.Uint64(t, "nonce", h.Nonce))
	g.Timestamp = hexutil.Uint64(testhex.Uint64(t, "timestamp", h.Timestamp))

	return g, allocs
}

// verifyWorldRootOnDisk reopens the DB read-only and checks that
// TRIE_BRANCH_STORAGE[WorldRootKey] equals wantRoot.
func verifyWorldRootOnDisk(t *testing.T, db *besuDB, wantRoot common.Hash) {
	t.Helper()

	// Use the existing handle — read-only reopen would race the still-open
	// writer. We just read directly through the writer's handle. The
	// SaveWorldState above flushed the batch with sync=true so the value
	// is durable.
	ro := grocksdb.NewDefaultReadOptions()
	defer ro.Destroy()
	got, err := db.db.GetCF(ro, db.cfs[cfIdxTrieBranchStorage], keys.WorldRootKey)
	if err != nil {
		t.Fatalf("read worldRoot: %v", err)
	}
	defer got.Free()
	if got.Size() != 32 {
		t.Fatalf("worldRoot size: got %d, want 32", got.Size())
	}
	gotHash := common.BytesToHash(got.Data())
	if gotHash != wantRoot {
		t.Fatalf("on-disk worldRoot != wantRoot:\n  got:  %s\n  want: %s",
			gotHash.Hex(), wantRoot.Hex())
	}
}
