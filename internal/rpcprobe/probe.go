// Package rpcprobe provides minimal JSON-RPC 2.0 client primitives for
// integration tests that boot a real Ethereum node (geth, reth, besu,
// nethermind) and assert on-disk state via HTTP RPC.
//
// The exported helpers cover the read-side surface every "the node booted
// and serves the state we wrote" oracle test needs:
//
//   - Call           — generic JSON-RPC call returning the raw result blob.
//   - WaitForRPC     — polls eth_blockNumber until the endpoint responds.
//   - EthGetBalance  — typed eth_getBalance, parsed to *big.Int.
//   - EthGetCode     — typed eth_getCode, returned as raw []byte.
//   - EthGetStorageAt — typed eth_getStorageAt, returned as common.Hash.
//
// Caller passes a fully-qualified URL ("http://host:port") and a block tag
// per the JSON-RPC spec ("0x0", "latest", "earliest", or a hex height).
//
// All helpers return errors instead of t.Fatalf'ing — these run in oracle
// tests where a per-call mismatch should produce a useful per-call error
// (e.g. "balance for 0xabc: got X want Y") rather than crashing the whole
// test before later assertions can run. The caller is responsible for
// fanning errors out via t.Errorf / t.Fatalf as appropriate.
package rpcprobe

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// Request is a JSON-RPC 2.0 request envelope.
type Request struct {
	JSONRPC string        `json:"jsonrpc"`
	Method  string        `json:"method"`
	Params  []any `json:"params"`
	ID      int           `json:"id"`
}

// Response is a JSON-RPC 2.0 response envelope. Exactly one of Result or
// Error is non-zero on a well-formed reply.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result"`
	Error   *Error          `json:"error"`
	ID      int             `json:"id"`
}

// Error is the JSON-RPC error sub-envelope.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Call sends a single JSON-RPC call and returns the raw result blob. A
// non-nil error envelope from the server is surfaced as a Go error.
func Call(url, method string, params []any) (json.RawMessage, error) {
	req := Request{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
		ID:      1,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(body)) //nolint:noctx
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	var rpcResp Response
	if err := json.Unmarshal(raw, &rpcResp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w (body: %s)", err, raw)
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("RPC error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	return rpcResp.Result, nil
}

// WaitForRPC polls eth_blockNumber every 500ms until either the call
// succeeds or the deadline is exceeded. Returns nil on success.
func WaitForRPC(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, err := Call(url, "eth_blockNumber", nil)
		if err == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("RPC at %s did not respond within %s", url, timeout)
}

// EthGetBalance calls eth_getBalance(addr, block) and parses the result
// into a *big.Int. Block is a JSON-RPC tag: "0x0", "latest", "earliest",
// or a hex height (e.g. "0x10").
func EthGetBalance(url string, addr common.Address, block string) (*big.Int, error) {
	raw, err := Call(url, "eth_getBalance", []any{addr.Hex(), block})
	if err != nil {
		return nil, err
	}
	var hexStr string
	if err := json.Unmarshal(raw, &hexStr); err != nil {
		return nil, fmt.Errorf("unmarshal balance: %w (raw: %s)", err, raw)
	}
	hexStr = strings.TrimPrefix(hexStr, "0x")
	if hexStr == "" || hexStr == "0" {
		return new(big.Int), nil
	}
	n := new(big.Int)
	if _, ok := n.SetString(hexStr, 16); !ok {
		return nil, fmt.Errorf("parse hex balance %q", hexStr)
	}
	return n, nil
}

// EthGetCode calls eth_getCode(addr, block) and returns the raw bytecode.
// An empty / non-contract address returns an empty (non-nil) slice.
func EthGetCode(url string, addr common.Address, block string) ([]byte, error) {
	raw, err := Call(url, "eth_getCode", []any{addr.Hex(), block})
	if err != nil {
		return nil, err
	}
	var hexStr string
	if err := json.Unmarshal(raw, &hexStr); err != nil {
		return nil, fmt.Errorf("unmarshal code: %w (raw: %s)", err, raw)
	}
	hexStr = strings.TrimPrefix(hexStr, "0x")
	if hexStr == "" {
		return []byte{}, nil
	}
	// eth_getCode returns whole bytes (even-length hex) per spec; defensive
	// odd-length pad costs nothing and protects against non-conforming
	// implementations.
	if len(hexStr)%2 == 1 {
		hexStr = "0" + hexStr
	}
	b, err := hex.DecodeString(hexStr)
	if err != nil {
		return nil, fmt.Errorf("decode code hex: %w", err)
	}
	return b, nil
}

// EthGetStorageAt calls eth_getStorageAt(addr, slot, block) and returns the
// 32-byte slot value as a common.Hash.
func EthGetStorageAt(url string, addr common.Address, slot common.Hash, block string) (common.Hash, error) {
	raw, err := Call(url, "eth_getStorageAt", []any{addr.Hex(), slot.Hex(), block})
	if err != nil {
		return common.Hash{}, err
	}
	var hexStr string
	if err := json.Unmarshal(raw, &hexStr); err != nil {
		return common.Hash{}, fmt.Errorf("unmarshal storage: %w (raw: %s)", err, raw)
	}
	return common.HexToHash(hexStr), nil
}
