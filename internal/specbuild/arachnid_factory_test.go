package specbuild

import (
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/nerolation/state-actor/internal/spec"
	"github.com/nerolation/state-actor/internal/templates"
)

// TestArachnidFactoryRequired pins the cross-entity invariant: a
// create2_deploys entry that uses the Arachnid factory (factory: unset,
// the default) fails Build when no create2_factory entity declares the
// Arachnid address.
func TestArachnidFactoryRequired(t *testing.T) {
	s := &spec.Spec{Entities: []spec.Entity{
		{
			Kind:     spec.KindContract,
			Template: "create2_deploys",
			Parameters: map[string]any{
				"initcode":   "0xfe",
				"runtime":    "0x00",
				"salt_count": 1,
			},
		},
	}}
	_, _, err := Build(s, defaultOpts)
	if err == nil {
		t.Fatalf("expected error when create2_deploys uses default Arachnid factory but no create2_factory entity is present")
	}
	if !strings.Contains(err.Error(), templates.CanonicalCREATE2FactoryAddress.Hex()) {
		t.Errorf("error must mention the canonical Arachnid address; got: %v", err)
	}
}

// TestArachnidFactorySatisfiedByDefaultAddress pins that a create2_factory
// entity with neither `address:` nor `name:` set defaults to the
// Arachnid address and therefore satisfies the requirement.
func TestArachnidFactorySatisfiedByDefaultAddress(t *testing.T) {
	s := &spec.Spec{Entities: []spec.Entity{
		{
			Kind:     spec.KindContract,
			Template: "create2_factory",
			// No address, no name → defaults to Arachnid.
		},
		{
			Kind:     spec.KindContract,
			Template: "create2_deploys",
			Parameters: map[string]any{
				"initcode":   "0xfe",
				"runtime":    "0x00",
				"salt_count": 1,
			},
		},
	}}
	if _, _, err := Build(s, defaultOpts); err != nil {
		t.Fatalf("Build: %v", err)
	}
}

// TestArachnidFactorySatisfiedByExplicitAddress pins that an explicit
// `address:` equal to the Arachnid address also satisfies the
// requirement (defense in depth for users who prefer to be explicit).
func TestArachnidFactorySatisfiedByExplicitAddress(t *testing.T) {
	arachnid := spec.HexAddress(templates.CanonicalCREATE2FactoryAddress)
	s := &spec.Spec{Entities: []spec.Entity{
		{
			Kind:     spec.KindContract,
			Template: "create2_factory",
			Address:  &arachnid,
		},
		{
			Kind:     spec.KindContract,
			Template: "create2_deploys",
			Parameters: map[string]any{
				"initcode":   "0xfe",
				"runtime":    "0x00",
				"salt_count": 1,
			},
		},
	}}
	if _, _, err := Build(s, defaultOpts); err != nil {
		t.Fatalf("Build: %v", err)
	}
}

// TestArachnidFactoryUnsatisfiedByNonArachnidAddress pins that a
// create2_factory entity at some OTHER address does NOT satisfy the
// requirement — the spec still needs a factory at Arachnid.
func TestArachnidFactoryUnsatisfiedByNonArachnidAddress(t *testing.T) {
	custom := spec.HexAddress(common.HexToAddress("0x000000000000000000000000000000000000beef"))
	s := &spec.Spec{Entities: []spec.Entity{
		{
			Kind:     spec.KindContract,
			Template: "create2_factory",
			Address:  &custom,
		},
		{
			Kind:     spec.KindContract,
			Template: "create2_deploys",
			Parameters: map[string]any{
				"initcode":   "0xfe",
				"runtime":    "0x00",
				"salt_count": 1,
			},
		},
	}}
	_, _, err := Build(s, defaultOpts)
	if err == nil {
		t.Fatalf("expected error: create2_factory at non-Arachnid address does not satisfy the Arachnid-required invariant")
	}
}

// TestNonArachnidFactoryNotChecked pins the NARROW scope of the
// invariant: a create2_deploys using an explicit non-Arachnid `factory:`
// is NOT checked. The user takes responsibility for declaring (or not
// declaring) a matching create2_factory themselves.
func TestNonArachnidFactoryNotChecked(t *testing.T) {
	s := &spec.Spec{Entities: []spec.Entity{
		{
			Kind:     spec.KindContract,
			Template: "create2_deploys",
			Parameters: map[string]any{
				"initcode":   "0xfe",
				"runtime":    "0x00",
				"salt_count": 1,
				"factory":    "0x000000000000000000000000000000000000beef",
			},
		},
	}}
	if _, _, err := Build(s, defaultOpts); err != nil {
		t.Fatalf("Build: %v (the narrow invariant should not block a non-Arachnid factory)", err)
	}
}

// TestArachnidFactoryRequiredWithExplicitFactoryParam pins the EXPLICIT
// branch of enforceArachnidFactoryRequirement: `factory:` set verbatim
// to the Arachnid address (not defaulted) must still demand a
// create2_factory entity. The pre-existing tests only covered the
// factory:-unset default branch; a regression in the explicit-parse
// branch would let an explicitly-Arachnid spec build chaindata whose
// CREATE2 deploys call into an empty 0x4e59…956C.
func TestArachnidFactoryRequiredWithExplicitFactoryParam(t *testing.T) {
	s := &spec.Spec{Entities: []spec.Entity{
		{
			Kind:     spec.KindContract,
			Template: "create2_deploys",
			Parameters: map[string]any{
				"initcode":   "0xfe",
				"runtime":    "0x00",
				"salt_count": 1,
				"factory":    templates.CanonicalCREATE2FactoryAddress.Hex(),
			},
		},
	}}
	_, _, err := Build(s, defaultOpts)
	if err == nil {
		t.Fatalf("expected error: explicit factory == Arachnid with no create2_factory entity")
	}
	if !strings.Contains(err.Error(), templates.CanonicalCREATE2FactoryAddress.Hex()) {
		t.Errorf("error must mention the Arachnid address; got: %v", err)
	}
}

// TestArachnidFactoryExplicitFactoryParamSatisfied pins the positive
// direction of the explicit branch: same spec plus a defaulted
// create2_factory entity builds cleanly.
func TestArachnidFactoryExplicitFactoryParamSatisfied(t *testing.T) {
	s := &spec.Spec{Entities: []spec.Entity{
		{
			Kind:     spec.KindContract,
			Template: "create2_factory",
		},
		{
			Kind:     spec.KindContract,
			Template: "create2_deploys",
			Parameters: map[string]any{
				"initcode":   "0xfe",
				"runtime":    "0x00",
				"salt_count": 1,
				"factory":    templates.CanonicalCREATE2FactoryAddress.Hex(),
			},
		},
	}}
	if _, _, err := Build(s, defaultOpts); err != nil {
		t.Fatalf("Build: %v", err)
	}
}

// TestArachnidFactoryNoConsumers pins that having a create2_factory
// entity without any create2_deploys consumer is fine — the invariant
// only fires when a consumer exists.
func TestArachnidFactoryNoConsumers(t *testing.T) {
	s := &spec.Spec{Entities: []spec.Entity{
		{
			Kind:     spec.KindContract,
			Template: "create2_factory",
		},
	}}
	if _, _, err := Build(s, defaultOpts); err != nil {
		t.Fatalf("Build: %v (orphan create2_factory should be allowed)", err)
	}
}

// TestNoCREATE2EntitiesAtAll pins that the invariant doesn't fire on
// specs that neither deploy CREATE2 nor declare a factory.
func TestNoCREATE2EntitiesAtAll(t *testing.T) {
	s := &spec.Spec{Entities: []spec.Entity{
		{
			Kind:    spec.KindEOA,
			Balance: nil,
		},
	}}
	if _, _, err := Build(s, defaultOpts); err != nil {
		t.Fatalf("Build: %v (spec with no CREATE2 entities should pass cleanly)", err)
	}
}
