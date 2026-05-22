package e2e_testing

import (
	"bytes"
	"sort"
	"testing"

	"github.com/ethereum/go-ethereum/common"

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

// CheckInjections verifies the writer landed cfg-driven pre-alloc on-chain:
//
//   - every GenesisAccounts entry with a non-zero Balance matches via
//     eth_getBalance.
//   - every GenesisCode entry's bytecode matches via eth_getCode.
//   - every GenesisStorage entry's slots match via eth_getStorageAt,
//     sampled to bound RPC round-trip cost (see sampleStorageSlots).
//
// Reports any mismatch via t.Errorf; returns false on any mismatch,
// true if everything checks out.
//
// Used by the per-client e2e suites at "0x0" (Phase 4: oracle re-query
// against genesis). Pairs with CheckEntities — that one covers
// entitygen synthetic entities; this one covers cfg-driven entries
// (spec entities via Config.PreAlloc).
func CheckInjections(t *testing.T, rpcURL string, cfg *generator.Config, blockTag string) bool {
	t.Helper()
	if cfg == nil {
		return true
	}
	passed := true
	for addr, acct := range cfg.GenesisAccounts {
		if acct == nil {
			continue
		}
		// Balance check covers EVERY entity, zero-balance included. The
		// previous skip-on-zero left YAML entries like `zero-balance-eoa`
		// (entity #20) and the canonical syscontracts (which all have
		// zero balance) unverified — a writer bug that wrote balance=N
		// for a speced balance=0 entry would slip through silently.
		if acct.Balance != nil {
			got, err := rpcprobe.EthGetBalance(rpcURL, addr, blockTag)
			if err != nil {
				t.Errorf("[%s] alloc eth_getBalance %s: %v", blockTag, addr.Hex(), err)
				passed = false
			} else if want := acct.Balance.ToBig(); got.Cmp(want) != 0 {
				t.Errorf("[%s] alloc eth_getBalance %s: got %s want %s — writer dropped GenesisAccounts balance?",
					blockTag, addr.Hex(), got.String(), want.String())
				passed = false
			}
		}
		// Nonce check: only when the spec/template set an explicit non-zero
		// nonce. Zero is the StateAccount default — re-querying every zero-
		// nonce entity would balloon Phase 4 RPC calls without catching
		// any class of writer bug.
		if acct.Nonce > 0 {
			gotNonce, err := rpcprobe.EthGetTransactionCount(rpcURL, addr, blockTag)
			if err != nil {
				t.Errorf("[%s] alloc eth_getTransactionCount %s: %v", blockTag, addr.Hex(), err)
				passed = false
			} else if gotNonce != acct.Nonce {
				t.Errorf("[%s] alloc eth_getTransactionCount %s: got %d want %d — writer dropped GenesisAccounts nonce?",
					blockTag, addr.Hex(), gotNonce, acct.Nonce)
				passed = false
			}
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
	// Storage verification: every spec entity with non-empty storage gets
	// up to storageSampleSize slots re-queried via eth_getStorageAt and
	// compared byte-for-byte. Sampling bounds RPC roundtrips at
	// O(addresses × storageSampleSize); for the CI baseline fixture
	// (~6 entities with storage, 5 slots each) that's ~30 RPC calls.
	// This catches the bug class: ERC-20 holder balances + 7702 EOA
	// storage-bloat slots landed in the writer but vanished by RPC time.
	for addr, slots := range cfg.GenesisStorage {
		if len(slots) == 0 {
			continue
		}
		for _, slot := range sampleStorageSlots(slots) {
			wantValue := slots[slot]
			got, err := rpcprobe.EthGetStorageAt(rpcURL, addr, slot, blockTag)
			if err != nil {
				t.Errorf("[%s] alloc eth_getStorageAt %s slot %s: %v",
					blockTag, addr.Hex(), slot.Hex(), err)
				passed = false
				continue
			}
			if got != wantValue {
				t.Errorf("[%s] alloc eth_getStorageAt %s slot %s: got %s want %s — writer dropped GenesisStorage?",
					blockTag, addr.Hex(), slot.Hex(), got.Hex(), wantValue.Hex())
				passed = false
			}
		}
	}
	return passed
}

// storageSampleSize is the per-entity slot-sample cap for CheckInjections.
// Picks first / last / middle + (size-4) interior slots from a sorted-by-key
// view of the entity's storage. Bounds RPC roundtrips per Phase 4
// invocation regardless of fixture size.
const storageSampleSize = 5

// sampleStorageSlots picks up to storageSampleSize keys deterministically
// from a storage map. Sorts by hex key, then picks first, last, middle,
// and additional interior keys spaced evenly. Deterministic — same input
// → same sampled keys → reproducible test signal across runs and across
// clients.
//
// Returns all keys when len(slots) <= storageSampleSize; otherwise
// returns exactly storageSampleSize keys.
func sampleStorageSlots(slots map[common.Hash]common.Hash) []common.Hash {
	if len(slots) <= storageSampleSize {
		out := make([]common.Hash, 0, len(slots))
		for k := range slots {
			out = append(out, k)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Hex() < out[j].Hex() })
		return out
	}
	keys := make([]common.Hash, 0, len(slots))
	for k := range slots {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Hex() < keys[j].Hex() })

	// Sample at evenly-spaced positions: 0, N-1, then the (storageSampleSize-2)
	// middle positions spread across the interior. Integer-rounded so the
	// pick is exactly storageSampleSize keys.
	out := make([]common.Hash, 0, storageSampleSize)
	n := len(keys)
	for i := 0; i < storageSampleSize; i++ {
		// idx = round(i * (n-1) / (storageSampleSize-1)) ; gives 0 at i=0
		// and n-1 at i=storageSampleSize-1, evenly spaced between.
		idx := i * (n - 1) / (storageSampleSize - 1)
		out = append(out, keys[idx])
	}
	return out
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
