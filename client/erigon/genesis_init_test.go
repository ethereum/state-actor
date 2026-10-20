//go:build cgo_erigon

package erigon

import (
	"bytes"
	"encoding/binary"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/erigontech/mdbx-go/mdbx"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"

	stategenesis "github.com/ethereum/state-actor/genesis"
	"github.com/ethereum/state-actor/internal/genesisheader"
)

// TestPatchGenesisHeaderStateRoot_ErigonInitHeader runs the real
// `erigon init` (the pinned binary that Dockerfile.erigon builds) on the
// genesis.json that writeGenesisJSON emits. It then reads block 0 from
// chaindata and checks three facts for each fork:
//
//   - go-ethereum's types.Header decodes the erigon header RLP and encodes
//     it again to the same bytes. patchGenesisHeaderStateRoot depends on
//     this round trip when it sets the stateRoot.
//   - The erigon genesis hash equals the hash of genesisheader.Build for
//     the same stateRoot. genesisheader.Build is pinned to `geth init`
//     (v1.17.6), so erigon writes the same header as geth.
//   - An Amsterdam header has blockAccessListHash = the empty-list hash
//     and slotNumber = 0. An Osaka header has neither field.
//
// The erigon library cannot be linked into this package (its secp256k1
// clashes with go-ethereum's), so the test uses the CLI. It skips when
// the binary is absent, as on a host build.
func TestPatchGenesisHeaderStateRoot_ErigonInitHeader(t *testing.T) {
	if _, err := os.Stat(erigonBinary); err != nil {
		t.Skipf("%s not found (run inside the Dockerfile.erigon builder image): %v", erigonBinary, err)
	}
	for _, fork := range []string{"osaka", "amsterdam"} {
		t.Run(fork, func(t *testing.T) {
			g, err := stategenesis.BuildSynthetic(fork, big.NewInt(1337), 60_000_000,
				1_700_000_000, []byte{0xde, 0xad, 0xbe, 0xef})
			if err != nil {
				t.Fatalf("BuildSynthetic(%s): %v", fork, err)
			}
			dbPath := t.TempDir()
			genesisPath := filepath.Join(dbPath, "genesis.json")
			if err := writeGenesisJSON(g, genesisPath, nil); err != nil {
				t.Fatalf("writeGenesisJSON: %v", err)
			}
			out, err := exec.Command(erigonBinary, "init", "--datadir", dbPath, genesisPath).CombinedOutput()
			if err != nil {
				t.Fatalf("erigon init: %v\n%s", err, out)
			}

			headerRLP := readGenesisHeaderRLP(t, dbPath)
			var h types.Header
			if err := rlp.DecodeBytes(headerRLP, &h); err != nil {
				t.Fatalf("go-ethereum types.Header: decode erigon RLP: %v", err)
			}
			gethRLP, err := rlp.EncodeToBytes(&h)
			if err != nil {
				t.Fatalf("go-ethereum header RLP: %v", err)
			}
			if !bytes.Equal(gethRLP, headerRLP) {
				t.Errorf("header RLP round trip changed the bytes:\n  erigon: %x\n  geth:   %x", headerRLP, gethRLP)
			}

			want := genesisheader.Build(g, 0, common.Hash{}, h.Root).Hash()
			if got := h.Hash(); got != want {
				t.Errorf("erigon genesis hash = %s; want %s (genesisheader.Build, root %s)",
					got.Hex(), want.Hex(), h.Root.Hex())
			}

			if g.Config.AmsterdamTime == nil {
				if h.BlockAccessListHash != nil || h.SlotNumber != nil {
					t.Errorf("Osaka header: blockAccessListHash=%v slotNumber=%v; want both absent",
						h.BlockAccessListHash, h.SlotNumber)
				}
				return
			}
			if h.BlockAccessListHash == nil || *h.BlockAccessListHash != types.EmptyBlockAccessListHash {
				t.Errorf("blockAccessListHash = %v; want %s", h.BlockAccessListHash, types.EmptyBlockAccessListHash.Hex())
			}
			if h.SlotNumber == nil || *h.SlotNumber != 0 {
				t.Errorf("slotNumber = %v; want 0", h.SlotNumber)
			}
		})
	}
}

// readGenesisHeaderRLP returns the block-0 header RLP that `erigon init`
// wrote to <dbPath>/chaindata.
func readGenesisHeaderRLP(t *testing.T, dbPath string) []byte {
	t.Helper()
	env, err := openChaindataEnv(dbPath)
	if err != nil {
		t.Fatalf("openChaindataEnv: %v", err)
	}
	defer env.Close()

	var out []byte
	if err := env.View(func(txn *mdbx.Txn) error {
		canonicalDBI, err := txn.OpenDBI(bucketHeaderCanonical, 0, nil, nil)
		if err != nil {
			return err
		}
		headersDBI, err := txn.OpenDBI(bucketHeaders, 0, nil, nil)
		if err != nil {
			return err
		}
		blockNumKey := make([]byte, 8)
		binary.BigEndian.PutUint64(blockNumKey, 0)
		hash, err := strictGet(txn, canonicalDBI, blockNumKey, bucketHeaderCanonical)
		if err != nil {
			return err
		}
		v, err := strictGet(txn, headersDBI, append(blockNumKey, hash...), bucketHeaders)
		if err != nil {
			return err
		}
		out = bytes.Clone(v)
		return nil
	}); err != nil {
		t.Fatalf("read block-0 header: %v", err)
	}
	return out
}
