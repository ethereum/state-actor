package oracle

import (
	"bytes"
	"testing"

	"github.com/nerolation/state-actor/generator"
	"github.com/nerolation/state-actor/internal/entitygen"
	"github.com/nerolation/state-actor/internal/rpcprobe"
)

// CheckEntities re-queries every entitygen-injected balance / code /
// storage slot at blockTag via JSON-RPC and reports any mismatch via
// t.Errorf. Returns false on any mismatch, true if everything checks
// out.
//
// Used by the per-client e2e suites at "0x0" (Phase 4: oracle re-query
// against genesis) and "latest" (Phase 6: oracle re-query post-spamoor,
// asserting entitygen entities are unchanged because spamoor touches a
// disjoint address space).
//
// Caveat: callers should pass the SAME (eoas, contracts) pair the
// writer wrote — i.e. the output of Reproduce(cfg) with matching seed/
// counts. Mismatch in those args produces spurious errors.
func CheckEntities(t *testing.T, rpcURL string, eoas, contracts []*entitygen.Account, blockTag string) bool {
	t.Helper()
	passed := true
	for _, eoa := range eoas {
		got, err := rpcprobe.EthGetBalance(rpcURL, eoa.Address, blockTag)
		if err != nil {
			t.Errorf("[%s] eth_getBalance %s: %v", blockTag, eoa.Address.Hex(), err)
			passed = false
			continue
		}
		want := eoa.StateAccount.Balance.ToBig()
		if got.Cmp(want) != 0 {
			t.Errorf("[%s] eth_getBalance %s: got %s want %s",
				blockTag, eoa.Address.Hex(), got.String(), want.String())
			passed = false
		}
	}
	for _, c := range contracts {
		gotCode, err := rpcprobe.EthGetCode(rpcURL, c.Address, blockTag)
		if err != nil {
			t.Errorf("[%s] eth_getCode %s: %v", blockTag, c.Address.Hex(), err)
			passed = false
		} else if !bytes.Equal(gotCode, c.Code) {
			t.Errorf("[%s] eth_getCode %s: len got=%d want=%d (first 32 bytes: got=%x want=%x)",
				blockTag, c.Address.Hex(), len(gotCode), len(c.Code),
				safePrefix(gotCode, 32), safePrefix(c.Code, 32))
			passed = false
		}
		for _, slot := range c.Storage {
			got, err := rpcprobe.EthGetStorageAt(rpcURL, c.Address, slot.Key, blockTag)
			if err != nil {
				t.Errorf("[%s] eth_getStorageAt %s slot %s: %v",
					blockTag, c.Address.Hex(), slot.Key.Hex(), err)
				passed = false
				continue
			}
			if got != slot.Value {
				t.Errorf("[%s] eth_getStorageAt %s slot %s: got %s want %s",
					blockTag, c.Address.Hex(), slot.Key.Hex(), got.Hex(), slot.Value.Hex())
				passed = false
			}
		}
	}
	return passed
}

// CheckInjections verifies that every address in cfg.InjectAddresses has
// a non-zero balance and every address in cfg.GenesisAccounts has the
// correct code via eth_getCode. Reports any mismatch via t.Errorf;
// returns false on any mismatch, true if everything checks out.
//
// This catches the exact bug class that PR review C1+C2 surfaced: a
// writer silently drops InjectAddresses or GenesisAccounts (because
// the field is unused by that client's writer) — Phase 4 fails loudly
// with "expected code at addr X, got empty" instead of the regression
// only surfacing via the cross-client genesis-root aggregator.
//
// Used by the per-client e2e suites at "0x0" (Phase 4: oracle re-query
// against genesis). Pairs with CheckEntities — that one covers
// entitygen synthetic entities; this one covers static cfg-driven
// injections.
func CheckInjections(t *testing.T, rpcURL string, cfg *generator.Config, blockTag string) bool {
	t.Helper()
	if cfg == nil {
		return true
	}
	passed := true
	for _, addr := range cfg.InjectAddresses {
		got, err := rpcprobe.EthGetBalance(rpcURL, addr, blockTag)
		if err != nil {
			t.Errorf("[%s] inject eth_getBalance %s: %v", blockTag, addr.Hex(), err)
			passed = false
			continue
		}
		if got.Sign() == 0 {
			t.Errorf("[%s] inject %s: balance is zero (writer dropped InjectAddresses?)", blockTag, addr.Hex())
			passed = false
		}
	}
	for addr, wantCode := range cfg.GenesisCode {
		gotCode, err := rpcprobe.EthGetCode(rpcURL, addr, blockTag)
		if err != nil {
			t.Errorf("[%s] alloc eth_getCode %s: %v", blockTag, addr.Hex(), err)
			passed = false
			continue
		}
		if !bytes.Equal(gotCode, wantCode) {
			t.Errorf("[%s] alloc eth_getCode %s: got len=%d want len=%d (first 32 bytes: got=%x want=%x) — writer dropped GenesisAccounts/Code?",
				blockTag, addr.Hex(), len(gotCode), len(wantCode),
				safePrefix(gotCode, 32), safePrefix(wantCode, 32))
			passed = false
		}
	}
	return passed
}

// safePrefix returns the first n bytes of b, or all of b if len(b) < n.
// Used in CheckEntities's eth_getCode mismatch error message — keeps
// the per-client test output bounded when a writer regression produces
// large bytecode diffs.
func safePrefix(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[:n]
}
