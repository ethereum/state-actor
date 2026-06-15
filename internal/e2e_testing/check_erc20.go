package e2e_testing

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"

	"github.com/ethereum/state-actor/internal/rpcprobe"
	"github.com/ethereum/state-actor/internal/spec"
	"github.com/ethereum/state-actor/internal/specbuild"
	"github.com/ethereum/state-actor/internal/templates"
)

// erc20OracleSampleCap caps the per-entity sample of synthesized
// owners/allowances re-queried via eth_call.
const erc20OracleSampleCap = 5

// CheckERC20Templates verifies, for each spec entity with `template:
// erc20`, that the planted state round-trips via JSON-RPC:
// eth_getCode, name/symbol/decimals, totalSupply, every explicit
// balanceOf + a deterministic sample of random owners, and every
// explicit allowance + a sample of synthesized ones. seed must match
// what specbuild.Build used so random-fill values re-derive identically.
// Returns false on any mismatch.
func CheckERC20Templates(t *testing.T, rpcURL string, specDoc *spec.Spec, seed int64, blockTag string) bool {
	t.Helper()
	if specDoc == nil {
		return true
	}
	passed := true
	for i, ent := range specDoc.Entities {
		if ent.Kind != spec.KindContract || ent.Template != "erc20" {
			continue
		}
		tokenAddr := specbuild.ResolveAddress(seed, ent, i)
		anchor := fmt.Sprintf("entities[%d] (template=erc20", i)
		if ent.Name != "" {
			anchor += fmt.Sprintf(", name=%q", ent.Name)
		}
		anchor += fmt.Sprintf(", addr=%s)", tokenAddr.Hex())

		if !checkERC20Bytecode(t, rpcURL, anchor, tokenAddr, blockTag) {
			passed = false
		}
		if !checkERC20Metadata(t, rpcURL, anchor, tokenAddr, ent, blockTag) {
			passed = false
		}
		if !checkERC20Balances(t, rpcURL, anchor, tokenAddr, ent, seed, blockTag) {
			passed = false
		}
		if !checkERC20Allowances(t, rpcURL, anchor, tokenAddr, ent, seed, blockTag) {
			passed = false
		}
	}
	return passed
}

func checkERC20Bytecode(t *testing.T, rpcURL, anchor string, tokenAddr common.Address, blockTag string) bool {
	t.Helper()
	gotCode, err := rpcprobe.EthGetCode(rpcURL, tokenAddr, blockTag)
	if err != nil {
		t.Errorf("[%s] %s: eth_getCode: %v", blockTag, anchor, err)
		return false
	}
	if !bytes.Equal(gotCode, templates.ERC20RuntimeBytecode) {
		t.Errorf("[%s] %s: eth_getCode mismatch: got %d bytes, want %d bytes (first 16: got=%x want=%x)",
			blockTag, anchor, len(gotCode), len(templates.ERC20RuntimeBytecode),
			safePrefix(gotCode, 16), safePrefix(templates.ERC20RuntimeBytecode, 16))
		return false
	}
	return true
}

func checkERC20Metadata(t *testing.T, rpcURL, anchor string, tokenAddr common.Address, ent spec.Entity, blockTag string) bool {
	t.Helper()
	passed := true

	wantName, _ := ent.Parameters["name"].(string)
	gotName, err := rpcprobe.EthCallERC20Name(rpcURL, tokenAddr, blockTag)
	if err != nil {
		t.Errorf("[%s] %s: eth_call name(): %v", blockTag, anchor, err)
		passed = false
	} else if gotName != wantName {
		t.Errorf("[%s] %s: name() = %q, want %q", blockTag, anchor, gotName, wantName)
		passed = false
	}

	wantSymbol, _ := ent.Parameters["symbol"].(string)
	gotSymbol, err := rpcprobe.EthCallERC20Symbol(rpcURL, tokenAddr, blockTag)
	if err != nil {
		t.Errorf("[%s] %s: eth_call symbol(): %v", blockTag, anchor, err)
		passed = false
	} else if gotSymbol != wantSymbol {
		t.Errorf("[%s] %s: symbol() = %q, want %q", blockTag, anchor, gotSymbol, wantSymbol)
		passed = false
	}

	gotDecimals, err := rpcprobe.EthCallERC20Decimals(rpcURL, tokenAddr, blockTag)
	if err != nil {
		t.Errorf("[%s] %s: eth_call decimals(): %v", blockTag, anchor, err)
		passed = false
	} else if gotDecimals != 18 {
		t.Errorf("[%s] %s: decimals() = %d, want 18", blockTag, anchor, gotDecimals)
		passed = false
	}

	return passed
}

func checkERC20Balances(t *testing.T, rpcURL, anchor string, tokenAddr common.Address, ent spec.Entity, seed int64, blockTag string) bool {
	t.Helper()
	passed := true

	// Validate has already run so re-parse can't fail.
	owners, _ := templates.ParseExplicitOwners(ent.Parameters["owners"])
	totalOwners := len(owners)
	if v, has := ent.Parameters["total_owners"]; has {
		n, _ := templates.ParseNonNegIntParam(v, "total_owners")
		totalOwners = n
	}
	randomCount := totalOwners - len(owners)

	wantTotal := new(uint256.Int)
	for _, o := range owners {
		wantTotal.Add(wantTotal, o.Balance)
	}
	for i := 0; i < randomCount; i++ {
		v := templates.DeterministicRandomBalance(seed, tokenAddr, i)
		wantTotal.Add(wantTotal, new(uint256.Int).SetBytes(v[:]))
	}
	gotTotal, err := rpcprobe.EthCallERC20TotalSupply(rpcURL, tokenAddr, blockTag)
	if err != nil {
		t.Errorf("[%s] %s: eth_call totalSupply(): %v", blockTag, anchor, err)
		passed = false
	} else if gotTotal.Cmp(wantTotal) != 0 {
		t.Errorf("[%s] %s: totalSupply() = %s, want %s", blockTag, anchor, gotTotal.String(), wantTotal.String())
		passed = false
	}

	for _, o := range owners {
		got, err := rpcprobe.EthCallERC20BalanceOf(rpcURL, tokenAddr, o.Address, blockTag)
		if err != nil {
			t.Errorf("[%s] %s: balanceOf(%s): %v", blockTag, anchor, o.Address.Hex(), err)
			passed = false
			continue
		}
		if got.Cmp(o.Balance) != 0 {
			t.Errorf("[%s] %s: balanceOf(%s) = %s, want %s",
				blockTag, anchor, o.Address.Hex(), got.String(), o.Balance.String())
			passed = false
		}
	}

	for _, idx := range sampleIndices(randomCount, erc20OracleSampleCap) {
		holder := templates.DeterministicRandomOwnerAddress(seed, tokenAddr, idx)
		wantVal := templates.DeterministicRandomBalance(seed, tokenAddr, idx)
		want := new(uint256.Int).SetBytes(wantVal[:])
		got, err := rpcprobe.EthCallERC20BalanceOf(rpcURL, tokenAddr, holder, blockTag)
		if err != nil {
			t.Errorf("[%s] %s: balanceOf(synth #%d at %s): %v", blockTag, anchor, idx, holder.Hex(), err)
			passed = false
			continue
		}
		if got.Cmp(want) != 0 {
			t.Errorf("[%s] %s: balanceOf(synth #%d at %s) = %s, want %s",
				blockTag, anchor, idx, holder.Hex(), got.String(), want.String())
			passed = false
		}
	}
	return passed
}

func checkERC20Allowances(t *testing.T, rpcURL, anchor string, tokenAddr common.Address, ent spec.Entity, seed int64, blockTag string) bool {
	t.Helper()
	passed := true

	allowances, _ := templates.ParseExplicitAllowances(ent.Parameters["allowances"])
	totalAllowances := len(allowances)
	if v, has := ent.Parameters["total_allowances"]; has {
		n, _ := templates.ParseNonNegIntParam(v, "total_allowances")
		totalAllowances = n
	}
	randomCount := totalAllowances - len(allowances)

	for _, a := range allowances {
		got, err := rpcprobe.EthCallERC20Allowance(rpcURL, tokenAddr, a.Owner, a.Spender, blockTag)
		if err != nil {
			t.Errorf("[%s] %s: allowance(%s, %s): %v", blockTag, anchor, a.Owner.Hex(), a.Spender.Hex(), err)
			passed = false
			continue
		}
		if got.Cmp(a.Amount) != 0 {
			t.Errorf("[%s] %s: allowance(%s, %s) = %s, want %s",
				blockTag, anchor, a.Owner.Hex(), a.Spender.Hex(), got.String(), a.Amount.String())
			passed = false
		}
	}

	for _, idx := range sampleIndices(randomCount, erc20OracleSampleCap) {
		owner := templates.DeterministicRandomAlwOwnerAddress(seed, tokenAddr, idx)
		spender := templates.DeterministicRandomAlwSpenderAddress(seed, tokenAddr, idx)
		wantVal := templates.DeterministicRandomAllowanceAmount(seed, tokenAddr, idx)
		want := new(uint256.Int).SetBytes(wantVal[:])
		got, err := rpcprobe.EthCallERC20Allowance(rpcURL, tokenAddr, owner, spender, blockTag)
		if err != nil {
			t.Errorf("[%s] %s: allowance(synth #%d %s, %s): %v",
				blockTag, anchor, idx, owner.Hex(), spender.Hex(), err)
			passed = false
			continue
		}
		if got.Cmp(want) != 0 {
			t.Errorf("[%s] %s: allowance(synth #%d %s, %s) = %s, want %s",
				blockTag, anchor, idx, owner.Hex(), spender.Hex(), got.String(), want.String())
			passed = false
		}
	}
	return passed
}

// sampleIndices returns a deterministic sample of up to cap indices
// from [0, count). For count > cap, picks first + last + evenly-spaced
// middle so regressions at boundaries surface.
func sampleIndices(count, cap int) []int {
	if count <= 0 {
		return nil
	}
	if count <= cap {
		out := make([]int, count)
		for i := range out {
			out[i] = i
		}
		return out
	}
	out := []int{0, count - 1}
	step := count / (cap - 1)
	for i := 1; i < cap-1; i++ {
		out = append(out, i*step)
	}
	return out
}
