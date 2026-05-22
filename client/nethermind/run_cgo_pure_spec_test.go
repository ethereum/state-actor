//go:build cgo_neth

package nethermind

import (
	"context"
	"iter"
	"math/big"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"

	"github.com/nerolation/state-actor/generator"
	"github.com/nerolation/state-actor/genesis"
	"github.com/nerolation/state-actor/internal/templates"
)

// TestPureSpecDispatchUsesStreamingPath pins that a Config with no
// synthetic accounts/contracts and a non-empty PreAlloc routes through
// writeSyntheticAccounts (which has Phase 0 spec-storage streaming),
// so the spec entity's Account.Root is spliced rather than left at
// EmptyRootHash.
func TestPureSpecDispatchUsesStreamingPath(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cgo dispatch test in -short mode")
	}

	g, err := genesis.BuildSynthetic("osaka", big.NewInt(1337), 30_000_000, 0, nil)
	if err != nil {
		t.Fatalf("BuildSynthetic: %v", err)
	}

	addr := common.HexToAddress("0x0000000000000000000000000000000000005555")
	specAccount := &types.StateAccount{
		Nonce:    1,
		Balance:  uint256.NewInt(1_000_000),
		Root:     types.EmptyRootHash,
		CodeHash: types.EmptyCodeHash[:],
	}
	specStorage := map[common.Hash]common.Hash{
		common.HexToHash("0x01"): common.HexToHash("0xaa"),
		common.HexToHash("0x02"): common.HexToHash("0xbb"),
		common.HexToHash("0x03"): common.HexToHash("0xcc"),
	}

	// Pure-spec test: no AutoFill (synthetic top-up disabled), just PreAlloc.
	cfg := generator.Config{
		DBPath:   filepath.Join(t.TempDir(), "neth"),
		TrieMode: generator.TrieModeMPT,
		Genesis:  g,
		PreAlloc: []templates.PreAllocEntity{{
			Address: addr,
			Account: specAccount,
			Storage: storageIterFromMap(specStorage),
		}},
	}

	stats, err := Run(context.Background(), cfg, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if specAccount.Root == types.EmptyRootHash {
		t.Fatalf("spec entity Root is still EmptyRootHash — Phase 0 did not run; "+
			"got Root=%s (cfg.StateRoot=%s)", specAccount.Root.Hex(), stats.StateRoot.Hex())
	}
	if stats.StateRoot == (common.Hash{}) {
		t.Fatalf("state root is zero hash")
	}
	if stats.AccountBytes == 0 {
		t.Errorf("stats.AccountBytes == 0")
	}
}

func storageIterFromMap(m map[common.Hash]common.Hash) iter.Seq2[common.Hash, common.Hash] {
	if len(m) == 0 {
		return nil
	}
	return func(yield func(common.Hash, common.Hash) bool) {
		for k, v := range m {
			if !yield(k, v) {
				return
			}
		}
	}
}
