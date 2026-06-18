package templates

import (
	"crypto/ecdsa"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/nerolation/state-actor/internal/spec"
)

func init() {
	Register(&sequentialPkeyEOAsTemplate{})
}

// sequentialPkeyEOAsTemplate handles
// `kind: contract, template: sequential_pkey_eoas`. One entity expands
// into `count` plain EOAs whose addresses are *derived* from the
// sequence of private keys [start_pkey, start_pkey + count) — i.e.
// EOA #i has private key (start_pkey + i) interpreted as a 32-byte
// big-endian secp256k1 scalar, and address keccak256(pubkey)[12:].
//
// Where `sequential_eoas` plants EOAs at sequential ADDRESSES (and the
// caller doesn't need / know their pkeys), this template plants EOAs at
// sequential PRIVATE KEYS so the caller can sign transactions FROM
// them. Anchor: the entity's resolved `address:` field is IGNORED —
// each EOA's address comes from its pkey derivation, not from a YAML
// anchor.
//
// Backs execution-specs/tests/benchmark/stateful/bloatnet/
// test_transaction_types.py's `yield_distinct_sender()` pattern, which
// constructs senders as `EOA(key=SENDER_BASE_KEY + i)` and assumes the
// resulting addresses are pre-funded on-chain. The matching
// state-actor entity is:
//
//   - kind: contract
//     template: sequential_pkey_eoas
//     parameters:
//       start_pkey: "0x1111111111111111111111111111111111111111111111111111111111111111"
//       count: 150000
//       balance: "1000000000000000000"  # 1 ETH/account, optional
//
// `balance` defaults to 1 wei if omitted and MUST be non-zero — zero-
// balance plain EOAs are pruned by EIP-161.
//
// `start_pkey` must be a valid secp256k1 scalar (in [1, n-1] where n
// is the curve order). The implementation rejects 0 and any pkey at or
// beyond the curve order; given a starting pkey at the very top of the
// range, the iteration walks past the order and the affected
// derivations error out (no silent wrap to 0).
type sequentialPkeyEOAsTemplate struct{}

// TemplateNameSequentialPkeyEOAs is the registry key for this template.
const TemplateNameSequentialPkeyEOAs = "sequential_pkey_eoas"

func (sequentialPkeyEOAsTemplate) Name() string      { return TemplateNameSequentialPkeyEOAs }
func (sequentialPkeyEOAsTemplate) UserVisible() bool { return true }

func (sequentialPkeyEOAsTemplate) HonoredEntityFields() EntityFieldSet {
	return EntityFieldSet{
		Balance:              EntityFieldSupport{Alternative: "parameters.balance"},
		Nonce:                EntityFieldSupport{}, // derived EOAs are always nonce 0
		Code:                 EntityFieldSupport{}, // plain EOAs carry no code
		ApproximateSizeBytes: EntityFieldSupport{Alternative: "parameters.count"},
	}
}

// sequentialPkeyEOAsParams is the typed result of
// parseSequentialPkeyEOAsParams.
type sequentialPkeyEOAsParams struct {
	startPkey *big.Int     // 32-byte scalar in [1, secp256k1Order)
	count     uint64       // in [1, 2^32]
	balance   *uint256.Int // never nil, never zero; defaults to 1 wei
}

// parseSequentialPkeyEOAsParams is the single validation+parse boundary
// for this template. ValidateParameters and Expand both call it, so a
// parameter set that validates is — by construction — the same one that
// expands; no check can drift between the two entry points.
func parseSequentialPkeyEOAsParams(params map[string]any) (sequentialPkeyEOAsParams, error) {
	var pp sequentialPkeyEOAsParams
	if err := RejectUnknownKeys(params, "sequential_pkey_eoas", []string{
		"start_pkey", "count", "balance",
	}); err != nil {
		return pp, err
	}
	for _, required := range []string{"start_pkey", "count"} {
		if _, ok := params[required]; !ok {
			return pp, fmt.Errorf("sequential_pkey_eoas: missing required parameter %q", required)
		}
	}
	pkeyBytes, err := ParseHexBytesParam(params["start_pkey"], "start_pkey")
	if err != nil {
		return pp, fmt.Errorf("sequential_pkey_eoas: %w", err)
	}
	if len(pkeyBytes) != 32 {
		return pp, fmt.Errorf("sequential_pkey_eoas: start_pkey must be exactly 32 bytes (got %d)", len(pkeyBytes))
	}
	startInt := new(big.Int).SetBytes(pkeyBytes)
	if startInt.Sign() == 0 {
		return pp, fmt.Errorf("sequential_pkey_eoas: start_pkey must be a non-zero secp256k1 scalar")
	}
	if startInt.Cmp(secp256k1Order) >= 0 {
		return pp, fmt.Errorf("sequential_pkey_eoas: start_pkey >= secp256k1 curve order")
	}
	pp.startPkey = startInt
	count, err := ParseUint64Param(params["count"], "count")
	if err != nil {
		return pp, fmt.Errorf("sequential_pkey_eoas: %w", err)
	}
	if count == 0 {
		return pp, fmt.Errorf("sequential_pkey_eoas: count must be >= 1 (count=0 emits nothing; delete the entity instead)")
	}
	// Practical ceiling: 2^32 keys × ~218 B/account already eclipses
	// any production fixture; treat larger as a typo.
	if count > practicalFanoutLimit {
		return pp, fmt.Errorf("sequential_pkey_eoas: count=%d exceeds practical limit (2^32)", count)
	}
	pp.count = count
	pp.balance = uint256.NewInt(1)
	if v, has := params["balance"]; has {
		b, err := ParseUint256Param(v, "balance")
		if err != nil {
			return pp, fmt.Errorf("sequential_pkey_eoas: %w", err)
		}
		if b.IsZero() {
			return pp, fmt.Errorf("sequential_pkey_eoas: balance must be > 0 (zero-balance EOAs are pruned by EIP-161)")
		}
		pp.balance = b
	}
	return pp, nil
}

func (sequentialPkeyEOAsTemplate) ValidateParameters(params map[string]any) error {
	_, err := parseSequentialPkeyEOAsParams(params)
	return err
}

func (sequentialPkeyEOAsTemplate) Expand(ctx Context, e spec.Entity) ([]PreAllocEntity, error) {
	pp, err := parseSequentialPkeyEOAsParams(e.Parameters)
	if err != nil {
		return nil, err
	}
	out := make([]PreAllocEntity, 0, pp.count)
	cur := new(big.Int).Set(pp.startPkey)
	one := big.NewInt(1)
	seen := make(map[common.Address]struct{}, pp.count)
	for i := uint64(0); i < pp.count; i++ {
		if cur.Sign() == 0 || cur.Cmp(secp256k1Order) >= 0 {
			return nil, fmt.Errorf(
				"sequential_pkey_eoas: derived pkey #%d falls outside [1, secp256k1_order); pick a smaller count or different start_pkey",
				i)
		}
		priv, err := derivePrivateKey(cur)
		if err != nil {
			return nil, fmt.Errorf("sequential_pkey_eoas: derive pkey #%d: %w", i, err)
		}
		addr := crypto.PubkeyToAddress(priv.PublicKey)
		// Defense in depth: sequential pkeys derive distinct
		// addresses by construction (secp256k1 is injective on a
		// connected subset of the scalar field), but a duplicate
		// here would silently overwrite a planted entry downstream.
		// Surface it instead.
		if _, dup := seen[addr]; dup {
			return nil, fmt.Errorf("sequential_pkey_eoas: duplicate derived address %s at pkey #%d", addr.Hex(), i)
		}
		seen[addr] = struct{}{}
		out = append(out, PreAllocEntity{
			Address: addr,
			Account: &types.StateAccount{
				Nonce:    0,
				Balance:  new(uint256.Int).Set(pp.balance),
				Root:     types.EmptyRootHash,
				CodeHash: types.EmptyCodeHash[:],
			},
		})
		cur.Add(cur, one)
	}
	return out, nil
}

// secp256k1Order is the order n of the secp256k1 curve. Private keys
// must be in [1, n-1]; values outside this range are invalid and
// crypto.ToECDSA rejects them. Sourced from the canonical SEC2 spec.
var secp256k1Order, _ = new(big.Int).SetString(
	"fffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141",
	16,
)

// derivePrivateKey is the bridge between this template's big.Int
// scalar arithmetic and go-ethereum's *ecdsa.PrivateKey. Pads the
// scalar to 32 bytes big-endian (crypto.ToECDSA's strict format) and
// returns the resulting key, surfacing any out-of-range error from
// go-ethereum.
func derivePrivateKey(scalar *big.Int) (*ecdsa.PrivateKey, error) {
	var buf [32]byte
	b := scalar.Bytes()
	copy(buf[32-len(b):], b)
	return crypto.ToECDSA(buf[:])
}
