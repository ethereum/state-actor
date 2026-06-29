package templates

import (
	"encoding/binary"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/ethereum/state-actor/internal/spec"
)

func init() {
	Register(&sequentialPkeyDelegationsTemplate{})
}

// delegationDesignatorPrefix is the 3-byte EIP-7702 marker; the full code
// is the prefix || 20-byte delegate target (23 bytes total).
const delegationDesignatorPrefix = "\xef\x01\x00"

// sequentialPkeyDelegationsTemplate handles
// `kind: contract, template: sequential_pkey_delegations`. One entity
// expands into `count` EIP-7702-delegated EOAs. Authority #i has private
// key (start_pkey + i) and code 0xef0100 || target_i, where target_i is
// the CREATE2 address keccak256(0xff ++ factory ++ salt ++
// keccak256(initcode))[12:] for salt = salt_start + i. Authority and
// target are paired by index.
//
// Backs execution-specs `yield_distinct_delegate_receiver()`
// (account_sender_receiver.py): authority EOA(key=DELEGATE_BASE_KEY + i)
// delegates to EXISTING_CONTRACT_DIFF receiver i. The target derivation
// mirrors create2_deploys, so pointing this template at the same
// code_pattern + factory + salt_start as a create2_deploys entity makes
// every authority delegate to that entity's planted contract.
//
// Only the target's initcode hash matters here (the target contract
// itself is planted by the paired create2_deploys entity); this template
// emits just the 23-byte delegation markers.
type sequentialPkeyDelegationsTemplate struct{}

// TemplateNameSequentialPkeyDelegations is the registry key for this template.
const TemplateNameSequentialPkeyDelegations = "sequential_pkey_delegations"

func (sequentialPkeyDelegationsTemplate) Name() string {
	return TemplateNameSequentialPkeyDelegations
}
func (sequentialPkeyDelegationsTemplate) UserVisible() bool { return true }

// HonoredEntityFields: derived authorities are fully described by the
// parameters (nonce 1, balance from params, code = delegation marker);
// entity-level fields would silently not apply, so they are rejected.
func (sequentialPkeyDelegationsTemplate) HonoredEntityFields() EntityFieldSet {
	return EntityFieldSet{
		Balance:              EntityFieldSupport{Alternative: "parameters.balance"},
		Nonce:                EntityFieldSupport{}, // delegated authorities are always nonce 1
		Code:                 EntityFieldSupport{}, // code is the derived delegation marker
		ApproximateSizeBytes: EntityFieldSupport{Alternative: "parameters.count"},
	}
}

// sequentialPkeyDelegationsParams is the typed result of the parse boundary.
type sequentialPkeyDelegationsParams struct {
	startPkey *big.Int       // 32-byte scalar in [1, secp256k1Order)
	count     uint64         // in [1, 2^32]
	balance   *uint256.Int   // never nil, never zero; defaults to 1 wei
	factory   common.Address // target CREATE2 factory; defaults to Arachnid
	saltStart uint64         // first target salt
	initHash  common.Hash    // keccak256 of the target initcode
}

// parseSequentialPkeyDelegationsParams is the single validation+parse
// boundary; ValidateParameters and Expand both call it so a parameter set
// that validates is the one that expands.
func parseSequentialPkeyDelegationsParams(params map[string]any) (sequentialPkeyDelegationsParams, error) {
	var pp sequentialPkeyDelegationsParams
	if err := RejectUnknownKeys(params, "sequential_pkey_delegations", []string{
		"start_pkey", "count", "balance", "code_pattern", "initcode", "factory", "salt_start",
	}); err != nil {
		return pp, err
	}
	for _, required := range []string{"start_pkey", "count"} {
		if _, ok := params[required]; !ok {
			return pp, fmt.Errorf("sequential_pkey_delegations: missing required parameter %q", required)
		}
	}

	// Authority key base (start_pkey + i), same derivation as sequential_pkey_eoas.
	pkeyBytes, err := ParseHexBytesParam(params["start_pkey"], "start_pkey")
	if err != nil {
		return pp, fmt.Errorf("sequential_pkey_delegations: %w", err)
	}
	if len(pkeyBytes) != 32 {
		return pp, fmt.Errorf("sequential_pkey_delegations: start_pkey must be exactly 32 bytes (got %d)", len(pkeyBytes))
	}
	startInt := new(big.Int).SetBytes(pkeyBytes)
	if startInt.Sign() == 0 {
		return pp, fmt.Errorf("sequential_pkey_delegations: start_pkey must be a non-zero secp256k1 scalar")
	}
	if startInt.Cmp(secp256k1Order) >= 0 {
		return pp, fmt.Errorf("sequential_pkey_delegations: start_pkey >= secp256k1 curve order")
	}
	pp.startPkey = startInt

	count, err := ParseUint64Param(params["count"], "count")
	if err != nil {
		return pp, fmt.Errorf("sequential_pkey_delegations: %w", err)
	}
	if count == 0 {
		return pp, fmt.Errorf("sequential_pkey_delegations: count must be >= 1 (count=0 emits nothing; delete the entity instead)")
	}
	if count > practicalFanoutLimit {
		return pp, fmt.Errorf("sequential_pkey_delegations: count=%d exceeds practical limit (2^32)", count)
	}
	pp.count = count

	pp.balance = uint256.NewInt(1)
	if v, has := params["balance"]; has {
		b, err := ParseUint256Param(v, "balance")
		if err != nil {
			return pp, fmt.Errorf("sequential_pkey_delegations: %w", err)
		}
		if b.IsZero() {
			return pp, fmt.Errorf("sequential_pkey_delegations: balance must be > 0 (omit balance to use the 1 wei default)")
		}
		pp.balance = b
	}

	// Delegation target initcode: code_pattern XOR initcode (exactly one).
	// Only its keccak256 is used — to derive the target CREATE2 address.
	initcode, err := parseDelegationTargetInitcode(params)
	if err != nil {
		return pp, err
	}
	pp.initHash = crypto.Keccak256Hash(initcode)

	pp.factory = CanonicalCREATE2FactoryAddress
	if v, has := params["factory"]; has {
		f, err := ParseAddressParam(v, "factory")
		if err != nil {
			return pp, fmt.Errorf("sequential_pkey_delegations: %w", err)
		}
		pp.factory = f
	}
	if v, has := params["salt_start"]; has {
		s, err := ParseUint64Param(v, "salt_start")
		if err != nil {
			return pp, fmt.Errorf("sequential_pkey_delegations: %w", err)
		}
		pp.saltStart = s
	}
	return pp, nil
}

// parseDelegationTargetInitcode resolves the target initcode from either a
// named code_pattern or a literal initcode (exactly one required).
func parseDelegationTargetInitcode(params map[string]any) ([]byte, error) {
	_, hasPattern := params["code_pattern"]
	_, hasInitcode := params["initcode"]
	switch {
	case hasPattern && hasInitcode:
		return nil, fmt.Errorf("sequential_pkey_delegations: set exactly one of `code_pattern` or `initcode`, not both")
	case hasPattern:
		name, ok := params["code_pattern"].(string)
		if !ok {
			return nil, fmt.Errorf("sequential_pkey_delegations: code_pattern must be a string (got %T)", params["code_pattern"])
		}
		if !IsKnownCodePattern(name) {
			return nil, fmt.Errorf("sequential_pkey_delegations: unknown code_pattern %q (known: %s)",
				name, strings.Join(knownCodePatterns, ", "))
		}
		return codePatternInitcodeFor(name)
	case hasInitcode:
		ic, err := ParseHexBytesParam(params["initcode"], "initcode")
		if err != nil {
			return nil, fmt.Errorf("sequential_pkey_delegations: %w", err)
		}
		if len(ic) == 0 {
			return nil, fmt.Errorf("sequential_pkey_delegations: initcode must be non-empty")
		}
		return ic, nil
	default:
		return nil, fmt.Errorf("sequential_pkey_delegations: missing delegation target — set `code_pattern` or `initcode`")
	}
}

func (sequentialPkeyDelegationsTemplate) ValidateParameters(params map[string]any) error {
	_, err := parseSequentialPkeyDelegationsParams(params)
	return err
}

func (sequentialPkeyDelegationsTemplate) Expand(ctx Context, e spec.Entity) ([]PreAllocEntity, error) {
	pp, err := parseSequentialPkeyDelegationsParams(e.Parameters)
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
				"sequential_pkey_delegations: derived pkey #%d falls outside [1, secp256k1_order); pick a smaller count or different start_pkey",
				i)
		}
		priv, err := derivePrivateKey(cur)
		if err != nil {
			return nil, fmt.Errorf("sequential_pkey_delegations: derive pkey #%d: %w", i, err)
		}
		authority := crypto.PubkeyToAddress(priv.PublicKey)
		if _, dup := seen[authority]; dup {
			return nil, fmt.Errorf("sequential_pkey_delegations: duplicate authority %s at pkey #%d", authority.Hex(), i)
		}
		seen[authority] = struct{}{}

		// Target #i: CREATE2 address for salt = salt_start + i.
		var salt [32]byte
		binary.BigEndian.PutUint64(salt[24:], pp.saltStart+i)
		target := crypto.CreateAddress2(pp.factory, salt, pp.initHash[:])

		// Code = 0xef0100 || target (23 bytes).
		code := make([]byte, 0, len(delegationDesignatorPrefix)+common.AddressLength)
		code = append(code, delegationDesignatorPrefix...)
		code = append(code, target[:]...)

		out = append(out, PreAllocEntity{
			Address: authority,
			Account: &types.StateAccount{
				// Delegating via SetCode bumps the authority nonce to 1.
				Nonce:    1,
				Balance:  new(uint256.Int).Set(pp.balance),
				Root:     types.EmptyRootHash,
				CodeHash: crypto.Keccak256Hash(code).Bytes(),
			},
			Code: code,
		})
		cur.Add(cur, one)
	}
	return out, nil
}
