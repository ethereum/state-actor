package specbuild

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/ethereum/state-actor/internal/spec"
)

func TestResolveAddressExplicit(t *testing.T) {
	want := common.HexToAddress("0x1111111111111111111111111111111111111111")
	wantHex := spec.HexAddress(want)
	e := spec.Entity{Kind: spec.KindEOA, Address: &wantHex}
	got := ResolveAddress(42, e, 0)
	if got != want {
		t.Errorf("explicit: got %v, want %v", got, want)
	}
}

func TestResolveAddressNameDerivedStable(t *testing.T) {
	// Same seed + same name → same address, regardless of index.
	e := spec.Entity{Kind: spec.KindContract, Template: "erc20", Name: "usdc-clone"}
	a := ResolveAddress(42, e, 0)
	b := ResolveAddress(42, e, 5)
	if a != b {
		t.Errorf("name-derived address must be index-stable: got %v vs %v", a, b)
	}
}

func TestResolveAddressNameSeedChangesAddress(t *testing.T) {
	e := spec.Entity{Kind: spec.KindContract, Template: "erc20", Name: "x"}
	a := ResolveAddress(42, e, 0)
	b := ResolveAddress(43, e, 0)
	if a == b {
		t.Errorf("changing seed must change address: both %v", a)
	}
}

func TestResolveAddressPositionDerived(t *testing.T) {
	e := spec.Entity{Kind: spec.KindContract, Template: "erc20"} // no address, no name
	a := ResolveAddress(42, e, 0)
	b := ResolveAddress(42, e, 1)
	if a == b {
		t.Errorf("position-derived addresses must differ across indices: both %v", a)
	}
}

func TestResolveAddressDeterministicAcrossRuns(t *testing.T) {
	e := spec.Entity{Kind: spec.KindContract, Template: "erc20", Name: "stable"}
	first := ResolveAddress(42, e, 0)
	second := ResolveAddress(42, e, 0)
	if first != second {
		t.Errorf("derive must be deterministic, got %v vs %v", first, second)
	}
}
