package rpcprobe

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// fakeRPCServer is a minimal JSON-RPC 2.0 server keyed by method →
// response-value. Each request reads its method name and looks it up in
// the map; if absent the server responds with a JSON-RPC error envelope.
func fakeRPCServer(t *testing.T, methodResults map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		val, ok := methodResults[req.Method]
		if !ok {
			_ = json.NewEncoder(w).Encode(Response{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &Error{Code: -32601, Message: "method not found"},
			})
			return
		}
		raw, _ := json.Marshal(val)
		_ = json.NewEncoder(w).Encode(Response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  raw,
		})
	}))
}

func TestCall_Success(t *testing.T) {
	srv := fakeRPCServer(t, map[string]any{"web3_clientVersion": "Geth/v1.17.2"})
	defer srv.Close()
	raw, err := Call(srv.URL, "web3_clientVersion", nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var got string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != "Geth/v1.17.2" {
		t.Errorf("got %q, want %q", got, "Geth/v1.17.2")
	}
}

func TestCall_RPCError(t *testing.T) {
	srv := fakeRPCServer(t, nil)
	defer srv.Close()
	_, err := Call(srv.URL, "method_that_doesnt_exist", nil)
	if err == nil {
		t.Fatal("expected RPC error, got nil")
	}
	if !strings.Contains(err.Error(), "method not found") {
		t.Errorf("expected method-not-found, got %v", err)
	}
}

func TestWaitForRPC_Success(t *testing.T) {
	srv := fakeRPCServer(t, map[string]any{"eth_blockNumber": "0x0"})
	defer srv.Close()
	if err := WaitForRPC(srv.URL, 2*time.Second); err != nil {
		t.Fatalf("WaitForRPC: %v", err)
	}
}

func TestWaitForRPC_Timeout(t *testing.T) {
	// Server that 500's everything - WaitForRPC should keep retrying then time out.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()
	err := WaitForRPC(srv.URL, 1*time.Second)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "did not respond") {
		t.Errorf("expected timeout message, got %v", err)
	}
}

func TestEthGetBalance(t *testing.T) {
	srv := fakeRPCServer(t, map[string]any{"eth_getBalance": "0xde0b6b3a7640000"}) // 1 ETH
	defer srv.Close()
	got, err := EthGetBalance(srv.URL, common.HexToAddress("0xabc"), "latest")
	if err != nil {
		t.Fatalf("EthGetBalance: %v", err)
	}
	if got.String() != "1000000000000000000" {
		t.Errorf("got %s, want 1000000000000000000", got.String())
	}
}

func TestEthGetBalance_Zero(t *testing.T) {
	srv := fakeRPCServer(t, map[string]any{"eth_getBalance": "0x0"})
	defer srv.Close()
	got, err := EthGetBalance(srv.URL, common.HexToAddress("0xabc"), "latest")
	if err != nil {
		t.Fatalf("EthGetBalance: %v", err)
	}
	if got.Sign() != 0 {
		t.Errorf("got %s, want 0", got.String())
	}
}

func TestEthGetCode(t *testing.T) {
	srv := fakeRPCServer(t, map[string]any{"eth_getCode": "0xdeadbeef"})
	defer srv.Close()
	got, err := EthGetCode(srv.URL, common.HexToAddress("0xabc"), "latest")
	if err != nil {
		t.Fatalf("EthGetCode: %v", err)
	}
	want := []byte{0xde, 0xad, 0xbe, 0xef}
	if string(got) != string(want) {
		t.Errorf("got %x, want %x", got, want)
	}
}

func TestEthGetCode_Empty(t *testing.T) {
	srv := fakeRPCServer(t, map[string]any{"eth_getCode": "0x"})
	defer srv.Close()
	got, err := EthGetCode(srv.URL, common.HexToAddress("0xabc"), "latest")
	if err != nil {
		t.Fatalf("EthGetCode: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d bytes, want 0", len(got))
	}
}

func TestEthGetStorageAt(t *testing.T) {
	want := common.HexToHash("0x000000000000000000000000000000000000000000000000000000000000002a")
	srv := fakeRPCServer(t, map[string]any{"eth_getStorageAt": want.Hex()})
	defer srv.Close()
	got, err := EthGetStorageAt(srv.URL, common.HexToAddress("0xabc"), common.HexToHash("0x1"), "latest")
	if err != nil {
		t.Fatalf("EthGetStorageAt: %v", err)
	}
	if got != want {
		t.Errorf("got %s, want %s", got.Hex(), want.Hex())
	}
}

func TestEthBlockNumber(t *testing.T) {
	srv := fakeRPCServer(t, map[string]any{"eth_blockNumber": "0x65"}) // 101
	defer srv.Close()
	got, err := EthBlockNumber(srv.URL)
	if err != nil {
		t.Fatalf("EthBlockNumber: %v", err)
	}
	if got != 101 {
		t.Errorf("got %d, want 101", got)
	}
}

func TestBlockByNumber(t *testing.T) {
	root := "0xabcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234" // arbitrary fixture; RPC plumbing test only
	srv := fakeRPCServer(t, map[string]any{
		"eth_getBlockByNumber": map[string]any{
			"number":     "0x0",
			"hash":       "0x1111111111111111111111111111111111111111111111111111111111111111",
			"stateRoot":  root,
			"parentHash": "0x0000000000000000000000000000000000000000000000000000000000000000",
			"timestamp":  "0x0",
		},
	})
	defer srv.Close()
	b, err := BlockByNumber(srv.URL, "0x0")
	if err != nil {
		t.Fatalf("BlockByNumber: %v", err)
	}
	if b.StateRoot.Hex() != root {
		t.Errorf("StateRoot got %s, want %s", b.StateRoot.Hex(), root)
	}
	if b.Number != "0x0" {
		t.Errorf("Number got %s, want 0x0", b.Number)
	}
}

func TestBlockByNumber_NotFound(t *testing.T) {
	// Server returns null result (block beyond tip).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Response{
			JSONRPC: "2.0",
			ID:      1,
			Result:  json.RawMessage("null"),
		})
	}))
	defer srv.Close()
	_, err := BlockByNumber(srv.URL, "0x999")
	if err == nil {
		t.Fatal("expected not-found error")
	}
	if !strings.Contains(err.Error(), "block not found") {
		t.Errorf("expected 'block not found', got %v", err)
	}
}

func TestGenesisStateRoot(t *testing.T) {
	root := "0xabcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234" // arbitrary fixture; RPC plumbing test only
	srv := fakeRPCServer(t, map[string]any{
		"eth_getBlockByNumber": map[string]any{
			"number":    "0x0",
			"stateRoot": root,
		},
	})
	defer srv.Close()
	got, err := GenesisStateRoot(srv.URL)
	if err != nil {
		t.Fatalf("GenesisStateRoot: %v", err)
	}
	if got.Hex() != root {
		t.Errorf("got %s, want %s", got.Hex(), root)
	}
}
