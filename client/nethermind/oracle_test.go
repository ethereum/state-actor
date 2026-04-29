//go:build cgo_neth

package nethermind

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"

	"github.com/nerolation/state-actor/internal/neth"
)

// TestDifferentialOracle is the Tier 2 differential oracle: state-actor's
// genesis-block hash for each of the three vendored Parity chainspec
// fixtures (from Nethermind.Blockchain.Test.GenesisBuilderTests at upstream
// SHA 09bd5a2d) MUST byte-equal the golden hash Nethermind itself
// computes. This pins the encoding/HalfPath/StackTrie pipeline against an
// external reference and guards against silent drift.
//
// Citation: src/Nethermind/Nethermind.Blockchain.Test/GenesisBuilderTests.cs
// at SHA 09bd5a2d, lines 26-34. The test class loads each fixture via
// Nethermind's ChainSpec parser and asserts `Block.Hash` equals the
// hard-coded value below.
func TestDifferentialOracle(t *testing.T) {
	fixturesDir := filepath.Join("..", "..", "internal", "neth", "testdata")

	cases := []struct {
		name             string
		fixtureFile      string
		wantGenesisHash  string
	}{
		{
			name:            "empty_accounts_and_storages",
			fixtureFile:     "empty_accounts_and_storages.json",
			wantGenesisHash: "0x61b2253366eab37849d21ac066b96c9de133b8c58a9a38652deae1dd7ec22e7b",
		},
		{
			name:            "empty_accounts_and_codes",
			fixtureFile:     "empty_accounts_and_codes.json",
			wantGenesisHash: "0xfa3da895e1c2a4d2673f60dd885b867d60fb6d823abaf1e5276a899d7e2feca5",
		},
		{
			name:            "hive_zero_balance_test",
			fixtureFile:     "hive_zero_balance_test.json",
			wantGenesisHash: "0x62839401df8970ec70785f62e9e9d559b256a9a10b343baf6c064747b094de09",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			spec, err := loadParityChainspec(filepath.Join(fixturesDir, tc.fixtureFile))
			if err != nil {
				t.Fatalf("load %s: %v", tc.fixtureFile, err)
			}

			accounts, codes, storages := paritySpecToStateAccounts(t, spec)
			t.Logf("%s: %d allocations → %d state entries (after EIP-161 filter), %d with storage",
				tc.name, len(spec.Accounts), len(accounts), len(storages))

			tmpDir := t.TempDir()
			dbs, err := openNethDBs(tmpDir)
			if err != nil {
				t.Fatalf("open DBs: %v", err)
			}
			defer dbs.Close()

			stateRoot := common.Hash(neth.EmptyTreeHash)
			if len(accounts) > 0 {
				stateRoot, err = writeGenesisAllocAccounts(dbs, accounts, codes, storages)
				if err != nil {
					t.Fatalf("write genesis alloc: %v", err)
				}
			}

			header := buildHeaderFromParitySpec(t, spec, stateRoot)

			got := header.Hash().Hex()
			if !strings.EqualFold(got, tc.wantGenesisHash) {
				t.Errorf("genesis hash mismatch\n  got:  %s\n  want: %s\n  state root: %s",
					got, tc.wantGenesisHash, stateRoot.Hex())
			}
		})
	}
}

// parityChainspec is a minimal Parity-format chainspec — only the fields
// that affect the genesis block hash. Hex fields are parsed manually
// because the Parity emitter writes leading zeros (e.g. "0x0a10000000"),
// which go-ethereum's hexutil rejects for the Uint64 type.
type parityChainspec struct {
	Genesis struct {
		Difficulty string `json:"difficulty"`
		Author     string `json:"author"`
		Timestamp  string `json:"timestamp"`
		ParentHash string `json:"parentHash"`
		ExtraData  string `json:"extraData"`
		GasLimit   string `json:"gasLimit"`
		Seal       struct {
			Ethereum struct {
				Nonce   string `json:"nonce"`
				MixHash string `json:"mixHash"`
			} `json:"ethereum"`
		} `json:"seal"`
	} `json:"genesis"`
	Params struct {
		ChainID string `json:"chainID"`
	} `json:"params"`
	Accounts map[string]parityAccount `json:"accounts"`
}

type parityAccount struct {
	Balance string            `json:"balance"`
	Nonce   string            `json:"nonce"`
	Code    string            `json:"code"`
	Storage map[string]string `json:"storage"`
	Builtin *json.RawMessage  `json:"builtin"`
}

// utf8BOM is the byte sequence some editors prepend to JSON files.
// Nethermind's hive_zero_balance_test.json carries one; encoding/json
// rejects it as `invalid character 'ï' looking for beginning of value`.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

func loadParityChainspec(path string) (*parityChainspec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	data = bytes.TrimPrefix(data, utf8BOM)
	var spec parityChainspec
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("unmarshal %s: %w", path, err)
	}
	return &spec, nil
}

// parseHexU64 parses a "0x..." string into a uint64, tolerating leading
// zero digits (which go-ethereum's hexutil.Uint64 rejects). Empty input
// returns 0. Anything malformed (bad hex digit, overflow) is a fixture
// bug — fail the test loud rather than silently zeroing.
//
// `field` is a human-readable name used in the failure message; pass the
// JSON path of the field being parsed (e.g. "genesis.gasLimit").
func parseHexU64(t *testing.T, field, s string) uint64 {
	t.Helper()
	if s == "" {
		return 0
	}
	stripped := strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if stripped == "" {
		return 0
	}
	v := new(big.Int)
	if _, ok := v.SetString(stripped, 16); !ok {
		t.Fatalf("oracle: parse %s: not hex: %q", field, s)
	}
	if !v.IsUint64() {
		t.Fatalf("oracle: parse %s: value overflows uint64: %q", field, s)
	}
	return v.Uint64()
}

// parseHexBig parses a "0x..." string into a big.Int, tolerating leading
// zeros. Empty input returns 0. Bad hex fails the test (silently zeroing
// would mask malformed fixtures behind a wrong-genesis-hash failure).
func parseHexBig(t *testing.T, field, s string) *big.Int {
	t.Helper()
	v := new(big.Int)
	if s == "" {
		return v
	}
	stripped := strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if stripped == "" {
		return v
	}
	if _, ok := v.SetString(stripped, 16); !ok {
		t.Fatalf("oracle: parse %s: not hex: %q", field, s)
	}
	return v
}

// parseHexBytes parses a "0x..."-prefixed hex string into bytes. Empty
// input or "0x" returns nil; bad hex fails the test.
func parseHexBytes(t *testing.T, field, s string) []byte {
	t.Helper()
	if s == "" {
		return nil
	}
	stripped := strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if stripped == "" {
		return nil
	}
	if len(stripped)%2 == 1 {
		stripped = "0" + stripped
	}
	b, err := hex.DecodeString(stripped)
	if err != nil {
		t.Fatalf("oracle: parse %s: %v (input %q)", field, err, s)
	}
	return b
}

// parseHexHash parses a 32-byte common.Hash. Returns the zero hash for
// empty input; fails the test if the input is non-empty but doesn't
// decode to exactly 32 bytes (common.HexToHash silently zeros these).
func parseHexHash(t *testing.T, field, s string) common.Hash {
	t.Helper()
	if s == "" || s == "0x" {
		return common.Hash{}
	}
	b := parseHexBytes(t, field, s)
	if len(b) != common.HashLength {
		t.Fatalf("oracle: parse %s: expected %d bytes, got %d (%q)", field, common.HashLength, len(b), s)
	}
	var h common.Hash
	copy(h[:], b)
	return h
}

// parseHexAddress parses a 20-byte common.Address. Returns the zero
// address for empty input; fails the test on length mismatch (common's
// HexToAddress silently truncates/zeros otherwise).
func parseHexAddress(t *testing.T, field, s string) common.Address {
	t.Helper()
	if s == "" || s == "0x" {
		return common.Address{}
	}
	b := parseHexBytes(t, field, s)
	if len(b) != common.AddressLength {
		t.Fatalf("oracle: parse %s: expected %d bytes, got %d (%q)", field, common.AddressLength, len(b), s)
	}
	var a common.Address
	copy(a[:], b)
	return a
}

// paritySpecToStateAccounts converts Parity allocations into the
// (StateAccount, code, storage) triple writeGenesisAllocAccounts expects.
// Empty accounts (EIP-161-empty: zero balance, zero nonce, no code, no
// storage) and `builtin`-only entries (precompile gas-pricing
// pseudoaccounts) are dropped — Nethermind's GenesisBuilder skips them at
// genesis time and the state root we compute has to match.
//
// Any field that fails to parse `t.Fatalf`s loud through the helper
// pipeline; we never silently zero a malformed fixture field.
func paritySpecToStateAccounts(t *testing.T, spec *parityChainspec) (
	map[common.Address]*types.StateAccount,
	map[common.Address][]byte,
	map[common.Address]map[common.Hash]common.Hash,
) {
	t.Helper()
	accounts := make(map[common.Address]*types.StateAccount)
	codes := make(map[common.Address][]byte)
	storages := make(map[common.Address]map[common.Hash]common.Hash)

	for addrStr, acc := range spec.Accounts {
		field := func(suffix string) string { return "accounts[" + addrStr + "]." + suffix }

		balance := parseHexBig(t, field("balance"), acc.Balance)
		nonce := parseHexU64(t, field("nonce"), acc.Nonce)
		code := parseHexBytes(t, field("code"), acc.Code)

		// Nethermind's GenesisBuilder treats `balance` PRESENCE as the
		// keep/drop signal, not its value: `hive_zero_balance_test.json`
		// includes a `0x...03` precompile with `"balance": "0x0"` and the
		// expected golden hash has it persisted in state. So drop only
		// when balance is ABSENT (the hexutil zero-value here is "").
		hasExplicitBalance := acc.Balance != ""

		// Builtin pseudoaccounts without any state field at all (no
		// balance, nonce, code, or storage) are gas-pricing entries only.
		isBuiltinOnly := acc.Builtin != nil &&
			!hasExplicitBalance &&
			nonce == 0 &&
			len(code) == 0 &&
			len(acc.Storage) == 0
		if isBuiltinOnly {
			continue
		}

		// EIP-161 empty: balance=0 AND not explicit, nonce=0, code=0x,
		// storage={}. Explicit balance keeps the account regardless of value.
		isEmpty := !hasExplicitBalance &&
			balance.Sign() == 0 &&
			nonce == 0 &&
			len(code) == 0 &&
			len(acc.Storage) == 0
		if isEmpty {
			continue
		}

		addr := parseHexAddress(t, "accounts."+addrStr+" key", addrStr)
		bal256, overflow := uint256.FromBig(balance)
		if overflow {
			t.Fatalf("oracle: parse %s: balance overflows uint256: %s", field("balance"), balance.String())
		}
		sa := &types.StateAccount{
			Nonce:    nonce,
			Balance:  bal256,
			Root:     types.EmptyRootHash,
			CodeHash: types.EmptyCodeHash[:],
		}
		if len(code) > 0 {
			codes[addr] = code
		}
		if len(acc.Storage) > 0 {
			normalized := make(map[common.Hash]common.Hash, len(acc.Storage))
			for k, v := range acc.Storage {
				// Storage keys/values may be non-canonical lengths in
				// Parity fixtures (e.g. "0x0" for slot 0). Pad through
				// common.HexToHash, which left-pads short input to 32
				// bytes — that matches Nethermind's parser. Anything
				// LONGER than 32 bytes is a fixture bug; surface it.
				if hb := parseHexBytes(t, field("storage["+k+"] key"), k); len(hb) > common.HashLength {
					t.Fatalf("oracle: storage key for %s too long: %d bytes (%q)", addrStr, len(hb), k)
				}
				if vb := parseHexBytes(t, field("storage["+k+"] value"), v); len(vb) > common.HashLength {
					t.Fatalf("oracle: storage value for %s too long: %d bytes (%q)", addrStr, len(vb), v)
				}
				kh := common.HexToHash(k)
				vh := common.HexToHash(v)
				if vh == (common.Hash{}) {
					continue // zero slot = deletion, skip
				}
				normalized[kh] = vh
			}
			if len(normalized) > 0 {
				storages[addr] = normalized
			}
		}
		accounts[addr] = sa
	}

	return accounts, codes, storages
}

// buildHeaderFromParitySpec builds a go-ethereum types.Header whose RLP
// keccak yields the same genesis hash Nethermind computes — every field
// Nethermind hashes is sourced from the Parity chainspec, with the
// state root provided by the caller.
//
// Hash-relevant fields that Nethermind sets at genesis (per
// HeaderDecoder.cs at SHA 09bd5a2d) and how we mirror each:
//   ParentHash   ← spec.Genesis.ParentHash
//   UncleHash    ← types.EmptyUncleHash (no uncles at genesis, fixed)
//   Coinbase     ← spec.Genesis.Author
//   Root         ← caller's stateRoot (already computed)
//   TxHash       ← types.EmptyTxsHash (genesis has no txs, fixed)
//   ReceiptHash  ← types.EmptyReceiptsHash (genesis has no receipts, fixed)
//   Bloom        ← zero (no logs at genesis, fixed)
//   Difficulty   ← spec.Genesis.Difficulty
//   Number       ← 0 (genesis is block 0, fixed)
//   GasLimit     ← spec.Genesis.GasLimit
//   GasUsed      ← 0 (genesis has no execution, fixed)
//   Time         ← spec.Genesis.Timestamp
//   Extra        ← spec.Genesis.ExtraData
//   MixDigest    ← spec.Genesis.Seal.Ethereum.MixHash
//   Nonce        ← spec.Genesis.Seal.Ethereum.Nonce
//
// Optional EIP-1559 / Shanghai / Cancun / Prague fields stay nil — the
// three vendored fixtures all target Berlin or earlier (the test class
// uses `Berlin.Instance` as ISpecProvider), so no `baseFeePerGas` or
// `withdrawalsRoot` are emitted in the genesis header.
func buildHeaderFromParitySpec(t *testing.T, spec *parityChainspec, stateRoot common.Hash) *types.Header {
	t.Helper()
	gasLimit := parseHexU64(t, "genesis.gasLimit", spec.Genesis.GasLimit)
	timestamp := parseHexU64(t, "genesis.timestamp", spec.Genesis.Timestamp)

	var nonce types.BlockNonce
	nb := parseHexBytes(t, "genesis.seal.ethereum.nonce", spec.Genesis.Seal.Ethereum.Nonce)
	if len(nb) > 8 {
		t.Fatalf("oracle: parse genesis.seal.ethereum.nonce: %d bytes, max 8", len(nb))
	}
	copy(nonce[8-len(nb):], nb)

	return &types.Header{
		ParentHash:  parseHexHash(t, "genesis.parentHash", spec.Genesis.ParentHash),
		UncleHash:   types.EmptyUncleHash,
		Coinbase:    parseHexAddress(t, "genesis.author", spec.Genesis.Author),
		Root:        stateRoot,
		TxHash:      types.EmptyTxsHash,
		ReceiptHash: types.EmptyReceiptsHash,
		Bloom:       types.Bloom{},
		Difficulty:  parseHexBig(t, "genesis.difficulty", spec.Genesis.Difficulty),
		Number:      new(big.Int),
		GasLimit:    gasLimit,
		GasUsed:     0,
		Time:        timestamp,
		Extra:       parseHexBytes(t, "genesis.extraData", spec.Genesis.ExtraData),
		MixDigest:   parseHexHash(t, "genesis.seal.ethereum.mixHash", spec.Genesis.Seal.Ethereum.MixHash),
		Nonce:       nonce,
	}
}
