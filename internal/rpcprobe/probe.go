// Package rpcprobe provides minimal JSON-RPC 2.0 client primitives for
// integration tests that boot a real Ethereum node (geth, reth, besu,
// nethermind) and assert on-disk state via HTTP RPC.
//
// The exported helpers cover the read-side surface every "the node booted
// and serves the state we wrote" oracle test needs:
//
//   - Call                 — generic JSON-RPC call returning the raw result blob.
//   - WaitForRPC           — polls eth_blockNumber until the endpoint responds.
//   - EthGetBalance        — typed eth_getBalance, parsed to *big.Int.
//   - EthGetCode           — typed eth_getCode, returned as raw []byte.
//   - EthGetStorageAt      — typed eth_getStorageAt, returned as common.Hash.
//   - EthGetTransactionCount — typed eth_getTransactionCount, parsed to uint64.
//   - EthBlockNumber       — typed eth_blockNumber, parsed to uint64.
//   - BlockByNumber        — eth_getBlockByNumber returning Block (stateRoot, etc).
//   - GenesisStateRoot     — convenience: BlockByNumber("0x0").StateRoot.
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
	// Canonical "0x0" only — short-circuiting on bare "0" (no 0x prefix)
	// would silently treat malformed inputs as zero. MEMORY.md rule:
	// "no silent-zero hex parsers — bug bit twice on this project".
	hexStr = strings.TrimPrefix(hexStr, "0x")
	if hexStr == "" {
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

// EthBlockNumber calls eth_blockNumber and parses the result into uint64.
func EthBlockNumber(url string) (uint64, error) {
	raw, err := Call(url, "eth_blockNumber", nil)
	if err != nil {
		return 0, err
	}
	var hexStr string
	if err := json.Unmarshal(raw, &hexStr); err != nil {
		return 0, fmt.Errorf("unmarshal blockNumber: %w (raw: %s)", err, raw)
	}
	// Canonical "0x0" only — see EthGetBalance for the rationale.
	hexStr = strings.TrimPrefix(hexStr, "0x")
	if hexStr == "" {
		return 0, fmt.Errorf("eth_blockNumber returned empty hex (after 0x trim)")
	}
	n := new(big.Int)
	if _, ok := n.SetString(hexStr, 16); !ok {
		return 0, fmt.Errorf("parse hex blockNumber %q", hexStr)
	}
	return n.Uint64(), nil
}

// EthGetTransactionCount calls eth_getTransactionCount(addr, block) and
// returns the nonce as uint64. Used by e2e suite tests to verify
// spamoor's sender wallet actually sent txs (nonce > 0 → real activity).
func EthGetTransactionCount(url string, addr common.Address, block string) (uint64, error) {
	raw, err := Call(url, "eth_getTransactionCount", []any{addr.Hex(), block})
	if err != nil {
		return 0, err
	}
	var hexStr string
	if err := json.Unmarshal(raw, &hexStr); err != nil {
		return 0, fmt.Errorf("unmarshal txCount: %w (raw: %s)", err, raw)
	}
	// Canonical "0x0" only — see EthGetBalance for the rationale.
	hexStr = strings.TrimPrefix(hexStr, "0x")
	if hexStr == "" {
		return 0, fmt.Errorf("eth_getTransactionCount returned empty hex (after 0x trim)")
	}
	n := new(big.Int)
	if _, ok := n.SetString(hexStr, 16); !ok {
		return 0, fmt.Errorf("parse hex txCount %q", hexStr)
	}
	return n.Uint64(), nil
}

// Block is the subset of eth_getBlockByNumber's reply we read for oracle
// purposes. JSON-RPC returns many more fields; we deliberately only
// surface what callers need so adding a new field is an explicit decision.
type Block struct {
	Number     string         `json:"number"`     // hex height ("0x0", "0x65")
	Hash       common.Hash    `json:"hash"`
	StateRoot  common.Hash    `json:"stateRoot"`
	ParentHash common.Hash    `json:"parentHash"`
	Timestamp  string         `json:"timestamp"`  // hex unix seconds
	GasLimit   string         `json:"gasLimit"`   // hex
	ExtraData  string         `json:"extraData"`  // "0x" + hex bytes
	MixHash    common.Hash    `json:"mixHash"`
	Miner      common.Address `json:"miner"` // coinbase / fee recipient
	Difficulty string         `json:"difficulty"` // hex
}

// BlockByNumber calls eth_getBlockByNumber(blockTag, false) and returns the
// header view. blockTag is a JSON-RPC tag: "0x0", "latest", "earliest", or
// a hex height. The fullTxObjects param is hardcoded to false — oracle
// callers don't read tx bodies through this helper.
//
// Rejects three failure modes that would otherwise silently produce a
// zero-shaped Block: (1) result == null (block not found), (2) Number
// field empty (incomplete reply), (3) StateRoot is zero (misconfigured
// client returning {}-shaped headers). These checks defend the
// cross-client stateRoot invariant against false-PASS via missing
// fields — see MEMORY.md's "no silent-zero hex parsers" guidance.
func BlockByNumber(url, blockTag string) (*Block, error) {
	raw, err := Call(url, "eth_getBlockByNumber", []any{blockTag, false})
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 || string(raw) == "null" {
		return nil, fmt.Errorf("eth_getBlockByNumber(%s): block not found", blockTag)
	}
	var b Block
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("unmarshal block: %w (raw: %s)", err, raw)
	}
	if b.Number == "" {
		return nil, fmt.Errorf("eth_getBlockByNumber(%s): empty Number field (incomplete reply: %s)", blockTag, raw)
	}
	if b.StateRoot == (common.Hash{}) {
		return nil, fmt.Errorf("eth_getBlockByNumber(%s): zero stateRoot (misconfigured client or incomplete reply)", blockTag)
	}
	return &b, nil
}

// GenesisStateRoot is a convenience wrapper around BlockByNumber("0x0").
// The state-root for block 0 is what the cross-client invariant compares
// across all 4 client adapters: same entitygen seed → same canonical-MPT
// root, regardless of on-disk node layout.
func GenesisStateRoot(url string) (common.Hash, error) {
	b, err := BlockByNumber(url, "0x0")
	if err != nil {
		return common.Hash{}, err
	}
	return b.StateRoot, nil
}

// TxRef is the subset of an eth_getBlockByNumber(_, true) transaction we
// read to find contract-creation calls. Contract creations have a null
// "to" field, so To is a pointer.
type TxRef struct {
	Hash  common.Hash     `json:"hash"`
	From  common.Address  `json:"from"`
	To    *common.Address `json:"to"`
	Nonce string          `json:"nonce"` // hex with 0x prefix
}

// BlockWithTxs is BlockByNumber's reply when called with fullTxObjects=true.
type BlockWithTxs struct {
	Number       string  `json:"number"`
	Hash         common.Hash `json:"hash"`
	Transactions []TxRef `json:"transactions"`
}

// BlockByNumberWithTxs calls eth_getBlockByNumber(blockTag, true) and
// returns the block with its full transactions list. Used by spamoor-
// output verification to find the contract-creation tx and its sender.
func BlockByNumberWithTxs(url, blockTag string) (*BlockWithTxs, error) {
	raw, err := Call(url, "eth_getBlockByNumber", []any{blockTag, true})
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 || string(raw) == "null" {
		return nil, fmt.Errorf("eth_getBlockByNumber(%s, true): block not found", blockTag)
	}
	var b BlockWithTxs
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("unmarshal block: %w (raw: %s)", err, raw)
	}
	if b.Number == "" {
		return nil, fmt.Errorf("eth_getBlockByNumber(%s, true): empty Number field", blockTag)
	}
	return &b, nil
}

// TxReceipt is the subset of eth_getTransactionReceipt's reply we read.
// ContractAddress is the deployed-contract address for creation txs and
// the zero address otherwise; Status is "0x1" on success, "0x0" on revert.
type TxReceipt struct {
	TransactionHash common.Hash     `json:"transactionHash"`
	ContractAddress *common.Address `json:"contractAddress"`
	Status          string          `json:"status"`
}

// EthGetTransactionReceipt calls eth_getTransactionReceipt(txHash). Returns
// an error if the receipt is missing or empty-shaped (defensive against
// clients that return {} or null for unknown hashes).
func EthGetTransactionReceipt(url string, txHash common.Hash) (*TxReceipt, error) {
	raw, err := Call(url, "eth_getTransactionReceipt", []any{txHash.Hex()})
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 || string(raw) == "null" {
		return nil, fmt.Errorf("eth_getTransactionReceipt(%s): receipt not found", txHash.Hex())
	}
	var r TxReceipt
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("unmarshal receipt: %w (raw: %s)", err, raw)
	}
	if r.Status == "" {
		return nil, fmt.Errorf("eth_getTransactionReceipt(%s): empty status (incomplete reply)", txHash.Hex())
	}
	return &r, nil
}
