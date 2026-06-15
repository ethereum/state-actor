package e2e_testing

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/params"

	"github.com/ethereum/state-actor/internal/rpcprobe"
	"github.com/ethereum/state-actor/internal/syscontracts"
)

// fakeRPC is a JSON-RPC 2.0 mock keyed by method name with an optional
// per-address override (key shape "<method>:<addr>") for getCode /
// getStorageAt / getBalance — needed when one address must return "0x"
// while the others return real code. Mirrors the pattern in
// internal/rpcprobe/probe_test.go:17 but kept local so the
// e2e_testing package doesn't depend on test-only exports from
// rpcprobe.
type fakeRPC struct {
	srv      *httptest.Server
	mu       sync.Mutex
	addrSeen map[string]int // method+addr → count, for "did the loop reach every entry?" gates
}

func newFakeRPC(t *testing.T, results map[string]any) *fakeRPC {
	t.Helper()
	f := &fakeRPC{addrSeen: make(map[string]int)}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcprobe.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var perAddrKey string
		switch req.Method {
		case "eth_getCode", "eth_getStorageAt", "eth_getBalance":
			if len(req.Params) > 0 {
				if addr, ok := req.Params[0].(string); ok {
					perAddrKey = req.Method + ":" + addr
					f.mu.Lock()
					f.addrSeen[perAddrKey]++
					f.mu.Unlock()
				}
			}
		}
		val, ok := results[perAddrKey]
		if !ok {
			val, ok = results[req.Method]
		}
		if !ok {
			_ = json.NewEncoder(w).Encode(rpcprobe.Response{
				JSONRPC: "2.0", ID: req.ID,
				Error: &rpcprobe.Error{Code: -32601, Message: "method not found"},
			})
			return
		}
		raw, _ := json.Marshal(val)
		_ = json.NewEncoder(w).Encode(rpcprobe.Response{
			JSONRPC: "2.0", ID: req.ID, Result: raw,
		})
	}))
	return f
}

func (f *fakeRPC) URL() string { return f.srv.URL }
func (f *fakeRPC) Close()      { f.srv.Close() }
func (f *fakeRPC) seenAddr(method string, addr string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addrSeen[method+":"+addr]
}

// ---- CheckChainID ----------------------------------------------------

func TestCheckChainID_Match(t *testing.T) {
	f := newFakeRPC(t, map[string]any{"eth_chainId": "0x539"}) // 1337
	defer f.Close()
	sub := &testing.T{}
	if !CheckChainID(sub, f.URL(), 1337) {
		t.Errorf("expected pass for matching chainId")
	}
	if sub.Failed() {
		t.Errorf("expected no t.Errorf for matching chainId")
	}
}

func TestCheckChainID_Mismatch(t *testing.T) {
	f := newFakeRPC(t, map[string]any{"eth_chainId": "0x1"})
	defer f.Close()
	sub := &testing.T{}
	if CheckChainID(sub, f.URL(), 1337) {
		t.Errorf("expected fail for chainId mismatch")
	}
	if !sub.Failed() {
		t.Errorf("expected t.Errorf for chainId mismatch")
	}
}

func TestCheckChainID_RPCError(t *testing.T) {
	f := newFakeRPC(t, nil) // no eth_chainId handler
	defer f.Close()
	sub := &testing.T{}
	if CheckChainID(sub, f.URL(), 1337) {
		t.Errorf("expected fail when eth_chainId errors")
	}
	if !sub.Failed() {
		t.Errorf("expected t.Errorf for RPC error")
	}
}

// ---- CheckCanonicalSyscontracts --------------------------------------

func TestCheckCanonicalSyscontracts_AllPresent(t *testing.T) {
	f := newFakeRPC(t, map[string]any{"eth_getCode": "0xdeadbeef"})
	defer f.Close()
	sub := &testing.T{}
	if !CheckCanonicalSyscontracts(sub, f.URL(), "0x0") {
		t.Errorf("expected pass when all 5 syscontracts have code")
	}
	if sub.Failed() {
		t.Errorf("expected no t.Errorf when all 5 present")
	}
}

func TestCheckCanonicalSyscontracts_AccumulatesAllFailures(t *testing.T) {
	// All 5 syscontracts return "0x" (empty code). The function MUST
	// query EVERY address — not short-circuit on the first failure.
	// Regression gate for a future loop refactor that converts
	// `passed = false; continue` into `return false`. Verified by
	// counting addresses seen in the fake server, since we can't
	// directly count t.Errorf calls on the synthetic sub-T.
	f := newFakeRPC(t, map[string]any{"eth_getCode": "0x"})
	defer f.Close()
	sub := &testing.T{}
	if CheckCanonicalSyscontracts(sub, f.URL(), "0x0") {
		t.Errorf("expected fail when 0/5 syscontracts have code")
	}
	for _, c := range canonicalSyscontracts {
		if got := f.seenAddr("eth_getCode", c.addr.Hex()); got != 1 {
			t.Errorf("syscontract %s not reached (seen %d times) — loop short-circuited?",
				c.label, got)
		}
	}
}

func TestCheckCanonicalSyscontracts_LastMissing(t *testing.T) {
	// Make the LAST syscontract (DepositContract) missing — guards
	// against a future "return on first miss" regression that would
	// only run 1/5 addresses and pass when the missing one is the
	// first canonicalSyscontracts entry.
	f := newFakeRPC(t, map[string]any{
		"eth_getCode": "0xdeadbeef",
		"eth_getCode:" + syscontracts.DepositContractAddress.Hex(): "0x",
	})
	defer f.Close()
	sub := &testing.T{}
	if CheckCanonicalSyscontracts(sub, f.URL(), "0x0") {
		t.Errorf("expected fail when DepositContract missing")
	}
	// All 5 addresses must have been queried.
	for _, c := range canonicalSyscontracts {
		if got := f.seenAddr("eth_getCode", c.addr.Hex()); got != 1 {
			t.Errorf("syscontract %s not reached (seen %d times)", c.label, got)
		}
	}
}

// ---- CheckChainAdvanced ----------------------------------------------

func TestCheckChainAdvanced_NonZero(t *testing.T) {
	f := newFakeRPC(t, map[string]any{"eth_blockNumber": "0x1"})
	defer f.Close()
	sub := &testing.T{}
	if !CheckChainAdvanced(sub, f.URL()) {
		t.Errorf("expected pass for block-number=1")
	}
	if sub.Failed() {
		t.Errorf("expected no t.Errorf")
	}
}

func TestCheckChainAdvanced_Zero(t *testing.T) {
	f := newFakeRPC(t, map[string]any{"eth_blockNumber": "0x0"})
	defer f.Close()
	sub := &testing.T{}
	if CheckChainAdvanced(sub, f.URL()) {
		t.Errorf("expected fail for block-number=0")
	}
	if !sub.Failed() {
		t.Errorf("expected t.Errorf for zero block-number")
	}
}

// ---- CheckBeaconRootsRingBuffer --------------------------------------

// fullBlock returns a JSON shape that satisfies BlockByNumber's
// stateRoot non-zero + number non-empty guards. Tests pass an explicit
// `timestamp`; everything else is dummy.
func fullBlock(timestamp string) map[string]any {
	return map[string]any{
		"number":     "0x1",
		"stateRoot":  "0x1111111111111111111111111111111111111111111111111111111111111111",
		"timestamp":  timestamp,
		"parentHash": "0x0000000000000000000000000000000000000000000000000000000000000000",
	}
}

func TestCheckBeaconRootsRingBuffer_NonZeroSlot(t *testing.T) {
	f := newFakeRPC(t, map[string]any{
		"eth_getBlockByNumber": fullBlock("0x100"), // ts=256 → slot=256
		"eth_getStorageAt":     "0xab" + strings.Repeat("00", 31),
	})
	defer f.Close()
	sub := &testing.T{}
	if !CheckBeaconRootsRingBuffer(sub, f.URL()) {
		t.Errorf("expected pass for non-zero ring-buffer slot")
	}
	if sub.Failed() {
		t.Errorf("expected no t.Errorf")
	}
	// BeaconRoots address must be the slot read (not, say, DepositContract).
	if got := f.seenAddr("eth_getStorageAt", params.BeaconRootsAddress.Hex()); got != 1 {
		t.Errorf("BeaconRoots address not queried: seen %d times", got)
	}
}

func TestCheckBeaconRootsRingBuffer_EmptyTimestamp(t *testing.T) {
	f := newFakeRPC(t, map[string]any{
		"eth_getBlockByNumber": fullBlock("0x"),
	})
	defer f.Close()
	sub := &testing.T{}
	if CheckBeaconRootsRingBuffer(sub, f.URL()) {
		t.Errorf("expected fail for empty timestamp")
	}
	if !sub.Failed() {
		t.Errorf("expected t.Errorf")
	}
}

func TestCheckBeaconRootsRingBuffer_LiteralZeroTimestamp(t *testing.T) {
	// Regression gate for T1.1: `tsHex == "0"` (literal "0x0") must be
	// rejected as "missing or literal zero", NOT silently fall through
	// to a misleading "EIP-4788 pre-exec didn't fire" message.
	f := newFakeRPC(t, map[string]any{
		"eth_getBlockByNumber": fullBlock("0x0"),
	})
	defer f.Close()
	sub := &testing.T{}
	if CheckBeaconRootsRingBuffer(sub, f.URL()) {
		t.Errorf("expected fail for literal-zero timestamp")
	}
	if !sub.Failed() {
		t.Errorf("expected t.Errorf")
	}
	// Crucially: eth_getStorageAt should NEVER have been called because
	// we rejected the timestamp before deriving the slot.
	if got := f.seenAddr("eth_getStorageAt", params.BeaconRootsAddress.Hex()); got != 0 {
		t.Errorf("eth_getStorageAt called %d times after literal-zero ts guard — should be 0", got)
	}
}

func TestCheckBeaconRootsRingBuffer_ParseError(t *testing.T) {
	f := newFakeRPC(t, map[string]any{
		"eth_getBlockByNumber": fullBlock("0xZZZ"),
	})
	defer f.Close()
	sub := &testing.T{}
	if CheckBeaconRootsRingBuffer(sub, f.URL()) {
		t.Errorf("expected fail for unparseable timestamp")
	}
	if !sub.Failed() {
		t.Errorf("expected t.Errorf")
	}
}

func TestCheckBeaconRootsRingBuffer_ModulusRollover(t *testing.T) {
	// ts=0x1FFF (=8191), slot = 8191 % 8191 = 0. Pins the `% 8191`
	// formula vs an off-by-one (`% 8192` would give slot=8191).
	f := newFakeRPC(t, map[string]any{
		"eth_getBlockByNumber": fullBlock("0x1FFF"),
		"eth_getStorageAt":     "0xab" + strings.Repeat("00", 31),
	})
	defer f.Close()
	sub := &testing.T{}
	if !CheckBeaconRootsRingBuffer(sub, f.URL()) {
		t.Errorf("expected pass at ts=8191 (slot=0 from modulus rollover)")
	}
	if sub.Failed() {
		t.Errorf("expected no t.Errorf")
	}
}

func TestCheckBeaconRootsRingBuffer_ZeroSlotValue(t *testing.T) {
	f := newFakeRPC(t, map[string]any{
		"eth_getBlockByNumber": fullBlock("0x100"),
		"eth_getStorageAt":     "0x" + strings.Repeat("00", 32),
	})
	defer f.Close()
	sub := &testing.T{}
	if CheckBeaconRootsRingBuffer(sub, f.URL()) {
		t.Errorf("expected fail for zero-valued ring-buffer slot")
	}
	if !sub.Failed() {
		t.Errorf("expected t.Errorf")
	}
}
