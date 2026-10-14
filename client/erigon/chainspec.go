package erigon

import (
	"encoding/json"
	"fmt"
	"math/big"
	"os"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/ethereum/state-actor/genesis"
	"github.com/ethereum/state-actor/internal/templates"
)

// writeGenesisJSON emits an Erigon-compatible genesis.json at outPath
// embedding the full synthetic alloc (PreAlloc + AutoFill).
//
// Erigon's `types.Genesis` is field-compatible with go-ethereum's
// `core.Genesis` JSON form for the standard mainnet schema. We start
// from state-actor's `genesis.Genesis` (whose JSON tags already match)
// and overlay the alloc map.
//
// At 25 GB target-size the resulting genesis.json approaches multi-GB.
// `erigon init` reads it into memory — if the bench fails for memory
// reasons, the iteration loop's first fix is to chunk the alloc writes
// across multiple `erigon init` invocations (Erigon's init is
// idempotent / additive for non-conflicting addresses).
func writeGenesisJSON(
	g *genesis.Genesis,
	outPath string,
	preAllocAccounts map[common.Address]*allocAccount,
) error {
	if g == nil {
		return fmt.Errorf("erigon.writeGenesisJSON: nil genesis")
	}
	raw, err := json.Marshal(g)
	if err != nil {
		return fmt.Errorf("marshal base genesis: %w", err)
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		return fmt.Errorf("unmarshal base genesis: %w", err)
	}

	// Build the alloc map in JSON-native form. Each address maps to
	// {balance, nonce, code, storage}. Erigon expects:
	//   balance: hex-encoded big int OR decimal string (geth standard)
	//   nonce:   hex u64 ("0x...")
	//   code:    hex bytes ("0x...")
	//   storage: { hex_slot_key: hex_value }
	alloc := make(map[string]map[string]any, len(preAllocAccounts))
	for addr, a := range preAllocAccounts {
		entry := map[string]any{}
		if a.Balance != nil && a.Balance.Sign() != 0 {
			entry["balance"] = hexutil.EncodeBig(a.Balance)
		} else {
			entry["balance"] = "0x0"
		}
		if a.Nonce != 0 {
			entry["nonce"] = hexutil.EncodeUint64(a.Nonce)
		}
		if code := a.code(); len(code) > 0 {
			entry["code"] = hexutil.Encode(code)
		}
		if len(a.Storage) > 0 {
			storMap := make(map[string]string, len(a.Storage))
			for slot, val := range a.Storage {
				storMap[slot.Hex()] = val.Hex()
			}
			entry["storage"] = storMap
		}
		alloc[addr.Hex()] = entry
	}
	spec["alloc"] = alloc

	// Cancun-active force-fields (matches reth's handling at
	// client/reth/chainspec.go:42-54).
	if g.Config != nil && g.Config.IsCancun(big.NewInt(0), uint64(g.Timestamp)) {
		if v, ok := spec["excessBlobGas"]; !ok || v == nil {
			spec["excessBlobGas"] = "0x0"
		}
		if v, ok := spec["blobGasUsed"]; !ok || v == nil {
			spec["blobGasUsed"] = "0x0"
		}
	}

	out, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal genesis.json: %w", err)
	}
	if err := os.WriteFile(outPath, append(out, '\n'), 0o644); err != nil {
		return fmt.Errorf("write genesis.json: %w", err)
	}
	return nil
}

// allocAccount is the orchestrator-internal alloc record. Distinct from
// any external type so we don't tie genesis.json generation to a
// specific upstream schema.
type allocAccount struct {
	Balance *big.Int
	Nonce   uint64
	Code    []byte
	Storage map[common.Hash]common.Hash

	// lazyCode, when set, makes the code on demand in place of Code.
	lazyCode *templates.LazyCode
}

// code returns Code, or a new copy from lazyCode.
func (a *allocAccount) code() []byte {
	if a.lazyCode != nil {
		return a.lazyCode.Make()
	}
	return a.Code
}

// hasCode reports whether the account has code, without a call to
// lazyCode.Make.
func (a *allocAccount) hasCode() bool {
	return len(a.Code) > 0 || a.lazyCode != nil
}
