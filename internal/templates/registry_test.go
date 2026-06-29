package templates

import "testing"

// TestRegistryHasExpectedTemplates pins the registered template set.
// New templates land here when they ship.
func TestRegistryHasExpectedTemplates(t *testing.T) {
	want := []string{
		"create2_deploys",
		"create2_factory",
		"create_preimage_deploys",
		"eoa",
		"erc20",
		"raw",
		"sequential_eoas",
		"sequential_pkey_delegations",
		"sequential_pkey_eoas",
		"storage_pattern",
	} // sorted
	got := Names()
	if len(got) != len(want) {
		t.Fatalf("registry has %d templates %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Names()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLookupHit(t *testing.T) {
	for _, name := range []string{
		"create2_deploys", "create2_factory", "create_preimage_deploys",
		"eoa", "erc20", "raw",
		"sequential_eoas", "sequential_pkey_delegations", "sequential_pkey_eoas", "storage_pattern",
	} {
		if _, ok := Lookup(name); !ok {
			t.Errorf("Lookup(%q) = false, want true", name)
		}
	}
}

func TestLookupMiss(t *testing.T) {
	if _, ok := Lookup("does-not-exist"); ok {
		t.Error("Lookup of nonexistent template returned true")
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on duplicate Register, got none")
		}
	}()
	Register(&rawTemplate{}) // already registered in init
}
