package e2e_testing

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/ethereum/state-actor/internal/entitygen"
)

// rpcStub responds to eth_getBalance / eth_getCode / eth_getStorageAt
// from a static map keyed by addr→balance | addr→code | addr+slot→value.
// Anything missing returns the zero value (so we can simulate the
// "writer didn't actually write the entity" failure mode).
type rpcStub struct {
	balances map[common.Address]string         // hex
	codes    map[common.Address]string         // hex
	storage  map[common.Address]map[common.Hash]common.Hash
}

func (s *rpcStub) Server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string  `json:"method"`
			Params []any   `json:"params"`
			ID     int     `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var result any
		switch req.Method {
		case "eth_getBalance":
			addr := common.HexToAddress(req.Params[0].(string))
			result = s.balances[addr]
			if result == "" {
				result = "0x0"
			}
		case "eth_getCode":
			addr := common.HexToAddress(req.Params[0].(string))
			result = s.codes[addr]
			if result == "" {
				result = "0x"
			}
		case "eth_getStorageAt":
			addr := common.HexToAddress(req.Params[0].(string))
			slot := common.HexToHash(req.Params[1].(string))
			if m, ok := s.storage[addr]; ok {
				if v, ok := m[slot]; ok {
					result = v.Hex()
				}
			}
			if result == nil {
				result = "0x0000000000000000000000000000000000000000000000000000000000000000"
			}
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"error": map[string]any{"code": -32601, "message": "method not found"},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": req.ID, "result": result,
		})
	}))
}

func TestCheckEntities_HappyPath(t *testing.T) {
	addr := common.HexToAddress("0xaaaa")
	caddr := common.HexToAddress("0xbbbb")
	slotKey := common.HexToHash("0x1")
	slotVal := common.HexToHash("0x2a")
	bal, _ := uint256.FromBig(common.Big0) // start at 0; we'll set below
	bal.SetUint64(0xde0b6b3a7640000) // 1 ETH
	stub := &rpcStub{
		balances: map[common.Address]string{addr: "0xde0b6b3a7640000"},
		codes:    map[common.Address]string{caddr: "0xdeadbeef"},
		storage:  map[common.Address]map[common.Hash]common.Hash{caddr: {slotKey: slotVal}},
	}
	srv := stub.Server(t)
	defer srv.Close()

	eoas := []*entitygen.Account{
		{Address: addr, AddrHash: common.Hash{}, StateAccount: &types.StateAccount{Balance: bal}},
	}
	contracts := []*entitygen.Account{
		{
			Address:      caddr,
			Code:         []byte{0xde, 0xad, 0xbe, 0xef},
			Storage:      []entitygen.StorageSlot{{Key: slotKey, Value: slotVal}},
			StateAccount: &types.StateAccount{Balance: new(uint256.Int)},
		},
	}
	if !CheckEntities(t, srv.URL, eoas, contracts, "latest") {
		t.Fatal("expected pass on happy-path stub; got false")
	}
}

// Mismatch detection (where CheckEntities returns false) is exercised
// implicitly through the per-client e2e suites — when a writer
// regression breaks balance/code/storage values, those tests fail with
// CheckEntities returning false. Unit-testing it here would require a
// mock testing.T to capture t.Errorf calls without poisoning the
// parent, which Go doesn't make trivial. The happy-path test +
// safePrefix coverage is what locks the helper's contract; the
// mismatch path is covered end-to-end.

func TestSafePrefix(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		n    int
		want []byte
	}{
		{"shorter than n", []byte{0x01, 0x02}, 5, []byte{0x01, 0x02}},
		{"equal to n", []byte{0x01, 0x02, 0x03}, 3, []byte{0x01, 0x02, 0x03}},
		{"longer than n", []byte{0x01, 0x02, 0x03, 0x04}, 2, []byte{0x01, 0x02}},
		{"empty input", []byte{}, 5, []byte{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := safePrefix(tc.in, tc.n)
			if string(got) != string(tc.want) {
				t.Errorf("got %x, want %x", got, tc.want)
			}
		})
	}
}

// TestSampleStorageSlotsReturnsAllWhenSmall: when the storage map has
// ≤ storageSampleSize entries, every key must be sampled (sorted).
func TestSampleStorageSlotsReturnsAllWhenSmall(t *testing.T) {
	slots := map[common.Hash]common.Hash{
		common.HexToHash("0x03"): {},
		common.HexToHash("0x01"): {},
		common.HexToHash("0x02"): {},
	}
	got := sampleStorageSlots(slots)
	if len(got) != 3 {
		t.Fatalf("got %d keys, want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Hex() > got[i].Hex() {
			t.Errorf("output not sorted: %v", got)
		}
	}
}

// TestSampleStorageSlotsCapsAtSampleSize: large maps get exactly
// storageSampleSize keys.
func TestSampleStorageSlotsCapsAtSampleSize(t *testing.T) {
	slots := make(map[common.Hash]common.Hash, 1000)
	for i := 0; i < 1000; i++ {
		var k common.Hash
		k[31] = byte(i & 0xff)
		k[30] = byte((i >> 8) & 0xff)
		slots[k] = common.HexToHash("0xaa")
	}
	got := sampleStorageSlots(slots)
	if len(got) != storageSampleSize {
		t.Fatalf("got %d keys, want %d", len(got), storageSampleSize)
	}
}

// TestSampleStorageSlotsDeterministic: same input → same output across
// invocations. Critical because the test signal depends on which slots
// got sampled.
func TestSampleStorageSlotsDeterministic(t *testing.T) {
	slots := make(map[common.Hash]common.Hash, 50)
	for i := 0; i < 50; i++ {
		var k common.Hash
		k[31] = byte(i)
		slots[k] = common.HexToHash("0xaa")
	}
	a := sampleStorageSlots(slots)
	b := sampleStorageSlots(slots)
	if len(a) != len(b) {
		t.Fatalf("count mismatch: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("sample[%d] differs across runs: %s vs %s", i, a[i].Hex(), b[i].Hex())
		}
	}
}

// TestSampleStorageSlotsSpread: the sampled keys span the sorted range —
// first and last are always included. Catches a regression where sampling
// clusters at one end.
func TestSampleStorageSlotsSpread(t *testing.T) {
	slots := make(map[common.Hash]common.Hash, 100)
	for i := 0; i < 100; i++ {
		var k common.Hash
		k[31] = byte(i)
		slots[k] = common.HexToHash("0xaa")
	}
	keys := make([]common.Hash, 0, len(slots))
	for k := range slots {
		keys = append(keys, k)
	}
	sortHashes(keys)

	got := sampleStorageSlots(slots)
	if got[0] != keys[0] {
		t.Errorf("first sampled key is not the smallest: got %s, want %s",
			got[0].Hex(), keys[0].Hex())
	}
	if got[len(got)-1] != keys[len(keys)-1] {
		t.Errorf("last sampled key is not the largest: got %s, want %s",
			got[len(got)-1].Hex(), keys[len(keys)-1].Hex())
	}
}

// sortHashes — test-only helper to avoid importing sort in the file's
// public surface. Mirrors sampleStorageSlots's internal sort.
func sortHashes(s []common.Hash) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1].Hex() > s[j].Hex(); j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
