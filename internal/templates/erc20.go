package templates

import (
	"encoding/binary"
	"fmt"
	"iter"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/ethereum/state-actor/internal/spec"
)

func init() {
	Register(&erc20Template{})
}

// OpenZeppelin v5 ERC20 storage layout. The numbers here must match the
// reference contract's slot positions exactly; tested in erc20_test.go.
const (
	erc20SlotBalances    = 0 // mapping(address => uint256)
	erc20SlotAllowances  = 1 // mapping(address => mapping(address => uint256))
	erc20SlotTotalSupply = 2
	erc20SlotName        = 3 // string (short-string layout when ≤31 bytes)
	erc20SlotSymbol      = 4 // string (short-string layout when ≤31 bytes)
)

// erc20MetadataSlots is the maximum number of non-holder storage slots the
// template writes: _totalSupply (slot 2), _name (slot 3), _symbol (slot 4).
// _name and _symbol are always written; _totalSupply only when supply > 0 (see
// the !totalSupply.IsZero() guard in Expand). The approximate_size_bytes
// fallback reserves this many slots from the budget. Keep in sync with the
// metadata writes in Expand.
const erc20MetadataSlots = 3

// erc20FixedDecimals is the only accepted value for the `decimals`
// parameter. OZ v5's `decimals()` is a pure function returning 18;
// planting a different value in storage has no effect at the RPC. Users
// who want non-18 tokens use the `raw` template.
const erc20FixedDecimals = 18

// erc20Template handles `kind: contract, template: erc20`. See
// docs/SPEC.md for the parameter schema. _totalSupply is auto-summed
// from explicit + random balances; users cannot override it.
type erc20Template struct{}

// TemplateNameERC20 is the registry key for this template.
const TemplateNameERC20 = "erc20"

func (erc20Template) Name() string      { return TemplateNameERC20 }
func (erc20Template) UserVisible() bool { return true }

func (erc20Template) HonoredEntityFields() EntityFieldSet {
	h := EntityFieldSupport{Honored: true}
	return EntityFieldSet{
		Balance:              h,
		Nonce:                h,                    // floored to 1 in Expand
		Code:                 EntityFieldSupport{}, // the template owns the runtime; spec.Validate already forbids template+code
		ApproximateSizeBytes: h,                    // fallback sizing when neither total_owners nor total_allowances is set
	}
}

func (erc20Template) ValidateParameters(params map[string]any) error {
	required := []string{"symbol", "name", "decimals"}
	for _, k := range required {
		v, ok := params[k]
		if !ok {
			return fmt.Errorf("erc20: missing required parameter %q", k)
		}
		if v == nil {
			return fmt.Errorf("erc20: parameter %q is null", k)
		}
	}

	// Surface the `holders` → `total_owners` rename with a migration hint.
	if _, has := params["holders"]; has {
		return fmt.Errorf("erc20: `holders` was renamed to `total_owners` " +
			"(also accepts an `owners` list for granular entries). " +
			"See docs/SPEC.md for the new schema.")
	}

	if s, _ := params["symbol"].(string); len(s) > 31 {
		return fmt.Errorf("erc20: symbol %q exceeds 31 bytes (OZ v5 short-string limit)", s)
	}
	if s, _ := params["name"].(string); len(s) > 31 {
		return fmt.Errorf("erc20: name %q exceeds 31 bytes (OZ v5 short-string limit)", s)
	}

	var dec int
	switch d := params["decimals"].(type) {
	case int:
		dec = d
	case int64:
		dec = int(d)
	default:
		return fmt.Errorf("erc20: decimals must be an integer (got %T)", params["decimals"])
	}
	if dec != erc20FixedDecimals {
		return fmt.Errorf("erc20: decimals must equal %d (OZ v5 base default); "+
			"use the `raw` template for non-%d tokens (got %d)",
			erc20FixedDecimals, erc20FixedDecimals, dec)
	}

	for k := range params {
		switch k {
		case "symbol", "name", "decimals",
			"owners", "allowances", "total_owners", "total_allowances":
		default:
			return fmt.Errorf("erc20: unknown parameter %q", k)
		}
	}

	owners, err := ParseExplicitOwners(params["owners"])
	if err != nil {
		return err
	}
	allowances, err := ParseExplicitAllowances(params["allowances"])
	if err != nil {
		return err
	}

	totalOwners := 0
	if v, has := params["total_owners"]; has {
		n, err := ParseNonNegIntParam(v, "total_owners")
		if err != nil {
			return err
		}
		totalOwners = n
	}
	if len(owners) > totalOwners && totalOwners > 0 {
		return fmt.Errorf("erc20: len(owners)=%d > total_owners=%d (set total_owners >= %d or remove explicit owners)",
			len(owners), totalOwners, len(owners))
	}

	totalAllowances := 0
	if v, has := params["total_allowances"]; has {
		n, err := ParseNonNegIntParam(v, "total_allowances")
		if err != nil {
			return err
		}
		totalAllowances = n
	}
	if len(allowances) > totalAllowances && totalAllowances > 0 {
		return fmt.Errorf("erc20: len(allowances)=%d > total_allowances=%d (set total_allowances >= %d or remove explicit allowances)",
			len(allowances), totalAllowances, len(allowances))
	}

	return nil
}

func (erc20Template) Expand(ctx Context, e spec.Entity) ([]PreAllocEntity, error) {
	symbol, _ := e.Parameters["symbol"].(string)
	name, _ := e.Parameters["name"].(string)

	balance := uint256.NewInt(0)
	if e.Balance != nil {
		balance = e.Balance.V
	}

	owners, err := ParseExplicitOwners(e.Parameters["owners"])
	if err != nil {
		return nil, err
	}
	allowances, err := ParseExplicitAllowances(e.Parameters["allowances"])
	if err != nil {
		return nil, err
	}

	totalOwners := len(owners)
	if v, has := e.Parameters["total_owners"]; has {
		n, err := ParseNonNegIntParam(v, "total_owners")
		if err != nil {
			return nil, err
		}
		totalOwners = n
	}
	totalAllowances := len(allowances)
	if v, has := e.Parameters["total_allowances"]; has {
		n, err := ParseNonNegIntParam(v, "total_allowances")
		if err != nil {
			return nil, err
		}
		totalAllowances = n
	}

	// Fallback sizing via approximate_size_bytes. Honors the universal
	// storage-sizing knob (docs/SPEC.md) only when neither total_owners
	// nor total_allowances is set; explicit sizing always wins. The slot
	// budget translates to additional random owners (one slot per holder),
	// minus the metadata slots the contract also occupies (up to
	// erc20MetadataSlots — _name/_symbol always, _totalSupply when supply > 0)
	// so the resulting on-disk footprint stays within the requested budget.
	if e.ApproximateSizeBytes > 0 {
		_, hasTotalOwners := e.Parameters["total_owners"]
		_, hasTotalAllowances := e.Parameters["total_allowances"]
		if !hasTotalOwners && !hasTotalAllowances {
			slotsBudget := ctx.Sizer.SlotsForBytes(ctx.ClientName, e.ApproximateSizeBytes)
			derived := slotsBudget - erc20MetadataSlots
			// max(), not assignment: a budget-derived count must never shrink
			// an explicit owners floor (totalOwners starts at len(owners)). A
			// sub-metadata budget makes derived <= 0, which this guard
			// harmlessly ignores (SlotsForBytes returns a signed int, so no
			// unsigned underflow).
			if derived > totalOwners {
				totalOwners = derived
			}
		}
	}

	randomOwnerCount := totalOwners - len(owners)
	randomAllowanceCount := totalAllowances - len(allowances)

	explicit := map[common.Hash]common.Hash{}
	explicit[uint64SlotKey(erc20SlotName)] = packShortString(name)
	explicit[uint64SlotKey(erc20SlotSymbol)] = packShortString(symbol)

	totalSupply := new(uint256.Int)

	for _, o := range owners {
		explicit[balanceSlotKey(o.Address)] = o.Balance.Bytes32()
		totalSupply.Add(totalSupply, o.Balance)
	}
	for _, a := range allowances {
		explicit[allowanceSlotKey(a.Owner, a.Spender)] = a.Amount.Bytes32()
	}

	// Sum random balances. The random iter is pure, so iterating it
	// here for the sum and again below for the storage iter yields the
	// same (key, value) pairs.
	for i := 0; i < randomOwnerCount; i++ {
		v := DeterministicRandomBalance(ctx.Seed, ctx.ResolvedAddress, i)
		totalSupply.Add(totalSupply, new(uint256.Int).SetBytes(v[:]))
	}

	if !totalSupply.IsZero() {
		explicit[uint64SlotKey(erc20SlotTotalSupply)] = totalSupply.Bytes32()
	}

	storage := MapToSeq(explicit)
	if randomOwnerCount > 0 {
		storage = Concat(storage, erc20BalancesIter(ctx.Seed, ctx.ResolvedAddress, randomOwnerCount))
	}
	if randomAllowanceCount > 0 {
		storage = Concat(storage, erc20RandomAllowancesIter(ctx.Seed, ctx.ResolvedAddress, randomAllowanceCount))
	}

	// Floor nonce at 1 (EIP-161: contracts have nonce ≥ 1).
	nonce := e.Nonce
	if nonce == 0 {
		nonce = 1
	}
	codeHash := crypto.Keccak256Hash(ERC20RuntimeBytecode)
	acc := &types.StateAccount{
		Nonce:    nonce,
		Balance:  balance,
		Root:     types.EmptyRootHash,
		CodeHash: codeHash.Bytes(),
	}

	return []PreAllocEntity{{
		Address: ctx.ResolvedAddress,
		Account: acc,
		Code:    ERC20RuntimeBytecode,
		Storage: storage,
	}}, nil
}

// erc20BalancesIter emits synthesized `_balances[holder]` entries for
// the random-fill portion of an erc20 contract. Holder address is
// keccak256(seed||token||index)[12:]; balance comes from
// DeterministicRandomBalance — both pure functions of the inputs.
// The slot key follows Solidity's mapping rule:
// keccak256(leftPad32(addr) || leftPad32(slot=0)).
func erc20BalancesIter(seed int64, tokenAddr common.Address, count int) iter.Seq2[common.Hash, common.Hash] {
	return func(yield func(common.Hash, common.Hash) bool) {
		var holderBuf [8 + common.AddressLength + 8]byte
		binary.BigEndian.PutUint64(holderBuf[:8], uint64(seed))
		copy(holderBuf[8:8+common.AddressLength], tokenAddr[:])

		var mapKeyBuf [64]byte // leftPad32(holder) || leftPad32(slot=0)

		for i := range count {
			binary.BigEndian.PutUint64(holderBuf[8+common.AddressLength:], uint64(i))
			holderHash := crypto.Keccak256(holderBuf[:])
			copy(mapKeyBuf[12:32], holderHash[12:])
			slotKey := crypto.Keccak256Hash(mapKeyBuf[:])
			val := DeterministicRandomBalance(seed, tokenAddr, i)
			if !yield(slotKey, val) {
				return
			}
		}
	}
}

// uint64SlotKey turns a small slot index into its 32-byte big-endian
// representation (top-level slots, not mappings).
func uint64SlotKey(slot uint64) common.Hash {
	var h common.Hash
	binary.BigEndian.PutUint64(h[24:32], slot)
	return h
}

// packShortString packs a ≤31-byte string into a Solidity short-string
// slot: [bytes 0..len-1] [zero 0..30] [length*2 at 31]. Panics on
// over-length input — callers validate first.
func packShortString(s string) common.Hash {
	if len(s) > 31 {
		panic(fmt.Sprintf("packShortString: input too long (%d bytes); callers must validate", len(s)))
	}
	var h common.Hash
	copy(h[:len(s)], s)
	h[31] = byte(len(s) * 2)
	return h
}
