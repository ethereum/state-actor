package geth

import (
	"bytes"
	"context"
	"math/big"
	"os"
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

// TestSpecIntrospect mirrors main.go's --spec pipeline against
// examples/spec-ci-comprehensive.yaml + seed=0 + --accounts=0
// --contracts=0, then opens the produced Pebble DB read-only and asserts
// for every PreAllocEntity:
//
//  1. an account snapshot row exists at SnapshotAccountPrefix +
//     keccak256(addr).
//  2. the snapshot row's CodeHash matches keccak256(pe.Code) (or
//     EmptyCodeHash when pe.Code is empty).
//  3. the Pebble row CodePrefix + CodeHash returns pe.Code byte-equal.
//
// On the parity-CI failure where named-token (name-derived addr) reads
// back empty via eth_getCode, exactly one of (1)/(2)/(3) will report a
// row miss — that diff pins the bug to Phase 1's sorter put, Phase 2's
// blob encode/decode, or the account-trie / snapshot write.
func TestSpecIntrospect(t *testing.T) {
	// Default cleanup unless caller wants to inspect manually.
	var dir string
	if keep := os.Getenv("KEEP_DATADIR"); keep != "" {
		dir = keep
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		t.Logf("KEEP_DATADIR=%s (boot geth: geth --dev --datadir=%s --db.engine=pebble --networkid=1337 --http)", dir, dir)
	} else {
		dir = t.TempDir()
	}
	dbPath := filepath.Join(dir, "geth", "chaindata")

	// --- 1. Mirror main.go's --spec pipeline -----------------------------
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
		Sizer:      sizecal.NewFixed(64),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Use BuildSynthetic to mirror main.go's chainspec defaults.
	g, err := stategenesis.BuildSynthetic("osaka", big.NewInt(1337), 60_000_000, 0, nil)
	if err != nil {
		t.Fatalf("BuildSynthetic: %v", err)
	}

	cfg := generator.Config{
		DBPath:         dbPath,
		PreAlloc:       pre,
		NumAccounts:    0,
		NumContracts:   0,
		Seed:           0,
		TrieMode:       generator.TrieModeMPT,
		WriteTrieNodes: true,
		Genesis:        g,
	}
	syscontracts.AddCanonicalSystemContracts(&cfg)
	if _, err := Populate(context.Background(), cfg, Options{}); err != nil {
		t.Fatalf("Populate: %v", err)
	}

	// --- 2. Open Pebble read-only ---------------------------------------
	db, err := pebble.New(dbPath, 16, 16, "geth-introspect", true)
	if err != nil {
		t.Fatalf("pebble.New: %v", err)
	}
	defer db.Close()

	// --- 3. Per-entity assertions ---------------------------------------
	// Deterministic iteration order — sort by hex address for stable logs.
	indices := make([]int, len(pre))
	for i := range indices {
		indices[i] = i
	}
	sort.Slice(indices, func(i, j int) bool {
		return bytes.Compare(pre[indices[i]].Address[:], pre[indices[j]].Address[:]) < 0
	})

	var failures int
	for _, i := range indices {
		pe := pre[i]
		addrHash := crypto.Keccak256Hash(pe.Address[:])

		// (1) Snapshot row at "a" + addrHash.
		snapKey := append([]byte("a"), addrHash.Bytes()...)
		snapRow, _ := db.Get(snapKey)

		// Decode SlimAccountRLP → StateAccount (handles the omit-when-empty
		// Root/CodeHash fields). types.FullAccount panics on invalid input,
		// but at this point we only call it when snapRow is non-empty.
		var snapHash common.Hash
		if len(snapRow) > 0 {
			acc, err := types.FullAccount(snapRow)
			if err != nil {
				t.Errorf("entity[%d] addr=%s SlimAccountRLP decode: %v", i, pe.Address.Hex(), err)
				continue
			}
			snapHash = common.BytesToHash(acc.CodeHash)
		}

		// (2) Expected CodeHash from the spec's PreAllocEntity.
		var wantCodeHash common.Hash
		if len(pe.Code) > 0 {
			wantCodeHash = crypto.Keccak256Hash(pe.Code)
		} else {
			wantCodeHash = types.EmptyCodeHash
		}

		// (3) Code-DB row at "c" + wantCodeHash.
		var gotCode []byte
		if len(pe.Code) > 0 {
			gotCode, _ = db.Get(append([]byte("c"), wantCodeHash.Bytes()...))
		}

		// Diagnostic log per entity.
		t.Logf("entity[%2d] addr=%s pe.Code=%4dB snap=%dB snap.CH=%s want.CH=%s code-bytes=%dB",
			i, pe.Address.Hex(), len(pe.Code), len(snapRow),
			snapHash.Hex()[:10], wantCodeHash.Hex()[:10], len(gotCode))

		// Assertions — only fire on entities the spec says should have code.
		if len(pe.Code) > 0 {
			if len(snapRow) == 0 {
				t.Errorf("BRANCH A — entity[%d] %s: snapshot row missing", i, pe.Address.Hex())
				failures++
				continue
			}
			if !bytes.Equal(snapHash[:], wantCodeHash[:]) {
				t.Errorf("BRANCH B — entity[%d] %s: snapshot CodeHash %s != keccak256(pe.Code) %s",
					i, pe.Address.Hex(), snapHash.Hex(), wantCodeHash.Hex())
				failures++
				continue
			}
			if !bytes.Equal(gotCode, pe.Code) {
				t.Errorf("BRANCH C — entity[%d] %s: code-DB row %dB != pe.Code %dB",
					i, pe.Address.Hex(), len(gotCode), len(pe.Code))
				failures++
			}
		}
	}
	if failures > 0 {
		t.Logf("INTROSPECT: %d/%d entities failed their writer-side invariant", failures, len(pre))
	}
}
