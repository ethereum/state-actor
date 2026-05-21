package geth

import (
	"bytes"
	"context"
	"math/big"
	"path/filepath"
	"sort"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb/pebble"

	"github.com/nerolation/state-actor/generator"
	stategenesis "github.com/nerolation/state-actor/genesis"
	"github.com/nerolation/state-actor/internal/sizecal"
	"github.com/nerolation/state-actor/internal/spec"
	"github.com/nerolation/state-actor/internal/specbuild"
	"github.com/nerolation/state-actor/internal/syscontracts"
	"github.com/nerolation/state-actor/internal/templates"
)

// TestSpecIntrospect runs the --spec pipeline against
// examples/spec-ci-comprehensive.yaml in-process and asserts, for every
// resolved PreAllocEntity, that the on-disk Pebble store contains:
//
//   1. an account snapshot row at "a" + keccak256(addr);
//   2. that account's CodeHash equals keccak256(pe.Code) when pe.Code != "";
//   3. a code-DB row at "c" + CodeHash that returns pe.Code byte-equal.
//
// Regression gate for the PR #83 named-token failure: while debugging
// that bug, an introspection run against a binary-built datadir showed
// every name- and position-derived entity missing its snapshot row. The
// in-process path (this test) was always byte-correct; the binary path
// silently randomized derived addresses via main.go's `--seed=0` trap.
// CI now passes --seed=42 to avoid that trap; this test pins the writer
// invariant so a future regression in materializePreAlloc / Phase 1 /
// Phase 2 fails here BEFORE the CI integration job spins up four
// clients + four Docker containers.
func TestSpecIntrospect(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "geth", "chaindata")

	s, err := spec.ParseFile("../../examples/spec-ci-comprehensive.yaml")
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if _, err := s.Validate(templates.UserVisibleNames()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	pre, _, err := specbuild.Build(s, specbuild.BuildOptions{
		Seed:       0,
		ClientName: "geth",
		Sizer:      sizecal.Default(),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	g, err := stategenesis.BuildSynthetic("osaka", big.NewInt(1337), 60_000_000, 0, nil)
	if err != nil {
		t.Fatalf("BuildSynthetic: %v", err)
	}
	cfg := generator.Config{
		DBPath:         dbPath,
		Distribution:   generator.PowerLaw,
		Seed:           0,
		TrieMode:       generator.TrieModeMPT,
		CommitInterval: 500_000,
		WriteTrieNodes: true,
		PreAlloc:       pre,
		Genesis:        g,
	}
	syscontracts.AddCanonicalSystemContracts(&cfg)
	if _, err := Populate(context.Background(), cfg, Options{}); err != nil {
		t.Fatalf("Populate: %v", err)
	}

	db, err := pebble.New(dbPath, 16, 16, "geth-introspect", true)
	if err != nil {
		t.Fatalf("pebble.New(%s): %v", dbPath, err)
	}
	defer db.Close()

	indices := make([]int, len(pre))
	for i := range indices {
		indices[i] = i
	}
	sort.Slice(indices, func(i, j int) bool {
		return bytes.Compare(pre[indices[i]].Address[:], pre[indices[j]].Address[:]) < 0
	})

	for _, i := range indices {
		pe := pre[i]
		addrHash := crypto.Keccak256Hash(pe.Address[:])

		snapRow, _ := db.Get(append([]byte("a"), addrHash.Bytes()...))
		if len(snapRow) == 0 {
			t.Errorf("entity[%d] %s: snapshot row missing", i, pe.Address.Hex())
			continue
		}
		acc, err := types.FullAccount(snapRow)
		if err != nil {
			t.Errorf("entity[%d] %s: SlimAccountRLP decode: %v", i, pe.Address.Hex(), err)
			continue
		}

		if len(pe.Code) == 0 {
			continue
		}
		wantCodeHash := crypto.Keccak256Hash(pe.Code)
		gotSnapHash := common.BytesToHash(acc.CodeHash)
		if !bytes.Equal(gotSnapHash[:], wantCodeHash[:]) {
			t.Errorf("entity[%d] %s: snapshot CodeHash %s != keccak256(pe.Code) %s",
				i, pe.Address.Hex(), gotSnapHash.Hex(), wantCodeHash.Hex())
			continue
		}
		gotCode, _ := db.Get(append([]byte("c"), wantCodeHash.Bytes()...))
		if !bytes.Equal(gotCode, pe.Code) {
			t.Errorf("entity[%d] %s: code-DB row %dB != pe.Code %dB",
				i, pe.Address.Hex(), len(gotCode), len(pe.Code))
		}
	}
}
