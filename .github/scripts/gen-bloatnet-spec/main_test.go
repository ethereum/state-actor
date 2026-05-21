package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/nerolation/state-actor/internal/spec"
	"github.com/nerolation/state-actor/internal/templates"
)

// TestGeneratedSpecParsesAndValidates runs the generator end-to-end
// against a fixed seed, then parses + validates the output with the
// project's spec package. Catches schema drift between the generator
// and the parser before we ship the YAML to the remote machine.
func TestGeneratedSpecParsesAndValidates(t *testing.T) {
	if testing.Short() {
		t.Skip("generator emits a 100+ MB YAML; skipping in -short")
	}
	tmp := t.TempDir()
	out := filepath.Join(tmp, "spec.yaml")

	cmd := exec.Command("go", "run", ".", "-out", out, "-seed", "4242")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("generator: %v", err)
	}

	doc, err := spec.ParseFile(out)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if _, err := doc.Validate(templates.UserVisibleNames()); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	// Expected counts: 1 spamoor + 5 bloated + 7 erc20 showcase + 2 raw
	// + 12 demo + 15000 bulk EOAs + 200000 bulk contracts = 215027.
	const want = 1 + 5 + 7 + 2 + 12 + 15_000 + 200_000
	if got := len(doc.Entities); got != want {
		t.Errorf("entity count = %d, want %d", got, want)
	}

	// Spamoor sender is the first entity at the canonical address.
	if doc.Entities[0].Address == nil || doc.Entities[0].Address.Address().Hex() !=
		"0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf" {
		t.Errorf("entity 0 not the spamoor sender: addr=%v", doc.Entities[0].Address)
	}
}
