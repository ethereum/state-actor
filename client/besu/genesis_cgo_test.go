//go:build cgo_besu

package besu

import (
	"context"
	"math/big"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	gethrlp "github.com/ethereum/go-ethereum/rlp"
	"github.com/holiman/uint256"
	"github.com/linxGnu/grocksdb"

	"github.com/ethereum/state-actor/generator"
	"github.com/ethereum/state-actor/genesis"
	"github.com/ethereum/state-actor/internal/besu/keys"
)

// TestGenesisBlockHashPerFork runs the besu writer on a one-account state
// and reads the genesis rows back from the BLOCKCHAIN and VARIABLES CFs.
// Besu rebuilds the genesis header from the chainspec at boot and compares
// its hash with chainHeadHash, so the stored header must hash to the value
// that a client derives from the same genesis.
//
// The want hashes come from `geth init` (go-ethereum v1.17.6) on an empty
// state, the same values that internal/genesisheader pins. The writer
// refuses an empty state, so the test puts the empty root into the stored
// header before it compares the hash. With amsterdamTime set, besu 26.9.0
// builds the same header: GenesisState.buildHeader adds balHash =
// EMPTY_BAL_HASH and slotNumber = 0 after requestsHash.
func TestGenesisBlockHashPerFork(t *testing.T) {
	for _, tc := range []struct {
		fork          string
		want          common.Hash
		wantAmsterdam bool
	}{
		{"osaka", common.HexToHash("0xcb47a1d8b28059a38b6a6d1bc93fa9e53e3f3534571339fe6c63205a1d97cf7f"), false},
		{"amsterdam", common.HexToHash("0xf2ca3be956811381db0b48028135ddde3053f592125d041acd67d19f832b9645"), true},
	} {
		t.Run(tc.fork, func(t *testing.T) {
			g, err := genesis.BuildSynthetic(tc.fork, big.NewInt(1337), 60_000_000,
				1_700_000_000, []byte{0xde, 0xad, 0xbe, 0xef})
			if err != nil {
				t.Fatalf("BuildSynthetic(%s): %v", tc.fork, err)
			}
			datadir := t.TempDir()
			stats, err := Run(context.Background(), generator.Config{
				DBPath:  datadir,
				Genesis: g,
				GenesisAccounts: map[common.Address]*types.StateAccount{
					common.HexToAddress("0x1000"): {
						Balance:  uint256.NewInt(1),
						Root:     types.EmptyRootHash,
						CodeHash: types.EmptyCodeHash.Bytes(),
					},
				},
			}, Options{})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}

			head := common.BytesToHash(readCF(t, datadir, cfIdxVariables, keys.ChainHeadHashKey))
			canonical := readCF(t, datadir, cfIdxBlockchain, keys.CanonicalHashKey(0))
			if got := common.BytesToHash(canonical); got != head {
				t.Errorf("canonical hash 0 = %s, chainHeadHash = %s", got.Hex(), head.Hex())
			}

			var header types.Header
			raw := readCF(t, datadir, cfIdxBlockchain, keys.BlockHeaderKey(head))
			if err := gethrlp.DecodeBytes(raw, &header); err != nil {
				t.Fatalf("decode stored header: %v", err)
			}
			if got := header.Hash(); got != head {
				t.Errorf("stored header hash = %s, chainHeadHash = %s", got.Hex(), head.Hex())
			}
			if header.Root != stats.StateRoot {
				t.Errorf("header root = %s, state root = %s", header.Root.Hex(), stats.StateRoot.Hex())
			}
			header.Root = types.EmptyRootHash
			if got := header.Hash(); got != tc.want {
				t.Errorf("header hash with the empty root = %s, want %s (geth v1.17.6 init)",
					got.Hex(), tc.want.Hex())
			}
			if header.RequestsHash == nil || *header.RequestsHash != types.EmptyRequestsHash {
				t.Errorf("requestsHash = %v, want %s", header.RequestsHash, types.EmptyRequestsHash.Hex())
			}
			if !tc.wantAmsterdam {
				if header.BlockAccessListHash != nil || header.SlotNumber != nil {
					t.Errorf("%s header has Amsterdam fields: blockAccessListHash=%v slotNumber=%v",
						tc.fork, header.BlockAccessListHash, header.SlotNumber)
				}
				return
			}
			if header.BlockAccessListHash == nil ||
				*header.BlockAccessListHash != types.EmptyBlockAccessListHash {
				t.Errorf("blockAccessListHash = %v, want %s",
					header.BlockAccessListHash, types.EmptyBlockAccessListHash.Hex())
			}
			if header.SlotNumber == nil || *header.SlotNumber != 0 {
				t.Errorf("slotNumber = %v, want 0", header.SlotNumber)
			}
		})
	}
}

// readCF reads one value from the RocksDB under <datadir>/database. It
// fails the test when the key is absent.
func readCF(t *testing.T, datadir string, cfIdx int, key []byte) []byte {
	t.Helper()

	cfNames := keys.BonsaiCFNames()
	cfOpts := make([]*grocksdb.Options, len(cfNames))
	for i := range cfOpts {
		cfOpts[i] = grocksdb.NewDefaultOptions()
	}
	defer func() {
		for _, o := range cfOpts {
			o.Destroy()
		}
	}()
	dbOpts := grocksdb.NewDefaultOptions()
	defer dbOpts.Destroy()

	db, cfs, err := grocksdb.OpenDbForReadOnlyColumnFamilies(
		dbOpts, filepath.Join(datadir, "database"), cfNames, cfOpts, false)
	if err != nil {
		t.Fatalf("open RocksDB: %v", err)
	}
	defer func() {
		for _, h := range cfs {
			h.Destroy()
		}
		db.Close()
	}()

	ro := grocksdb.NewDefaultReadOptions()
	defer ro.Destroy()
	got, err := db.GetCF(ro, cfs[cfIdx], key)
	if err != nil {
		t.Fatalf("GetCF(%x): %v", key, err)
	}
	defer got.Free()
	if !got.Exists() {
		t.Fatalf("key %x not found in CF %d", key, cfIdx)
	}
	return append([]byte(nil), got.Data()...)
}
