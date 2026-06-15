package genesisheader

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/state-actor/genesis"
)

// TestBuild_OsakaSmokeTest sanity-checks that internal/genesisheader.Build
// accepts a fork=osaka chain config without panicking + without producing
// any new fields beyond Prague's RequestsHash. Osaka adds no new genesis-
// header fields per go-ethereum v1.17.2, so the same Prague-shape header
// is correct.
func TestBuild_OsakaSmokeTest(t *testing.T) {
	g, err := genesis.BuildSynthetic("osaka", big.NewInt(1337), 60_000_000, 0, nil)
	if err != nil {
		t.Fatalf("BuildSynthetic(osaka): %v", err)
	}
	if g.Config.OsakaTime == nil || *g.Config.OsakaTime != 0 {
		t.Fatalf("OsakaTime: got %v, want *0", g.Config.OsakaTime)
	}
	h := Build(g, 0, common.Hash{}, types.EmptyRootHash)
	if h.GasLimit != 60_000_000 {
		t.Errorf("GasLimit: got %d, want 60000000", h.GasLimit)
	}
	// Osaka inherits Prague's header shape — RequestsHash should be set.
	if h.RequestsHash == nil {
		t.Error("RequestsHash should be set under Osaka (inherits Prague)")
	}
	// Shanghai withdrawals + Cancun blob fields also inherited.
	if h.WithdrawalsHash == nil {
		t.Error("WithdrawalsHash should be set under Osaka (inherits Shanghai)")
	}
	if h.ParentBeaconRoot == nil {
		t.Error("ParentBeaconRoot should be set under Osaka (inherits Cancun)")
	}
}
