package oracle

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/nerolation/state-actor/internal/entitygen"
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
