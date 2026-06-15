// verify-trie-consistency walks reth's AccountsTrie and verifies that every
// tree_mask bit points to a child BNC row that actually exists at the right
// variable-length key. Catches the wire-format / propagation bug that
// triggered the v8 reth tokio-rt SIGSEGV.
//
// Usage:
//
//	go build -tags=cgo_reth -buildvcs=false -o /tmp/verify-trie ./scripts/verify-trie-consistency
//	docker run --rm -v <datadir>:/data -v /tmp/verify-trie:/v:ro debian:trixie-slim /v -datadir /data
//
// Expected on a correct DB: "dangling = 0 / orphan = 0". Non-zero counts mean
// the writer's tree_mask propagation or key encoding is broken.

//go:build cgo_reth

package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"path/filepath"

	"github.com/erigontech/mdbx-go/mdbx"

	iReth "github.com/ethereum/state-actor/internal/reth"
)

func main() {
	datadir := flag.String("datadir", "", "reth datadir (contains db/mdbx.dat)")
	maxReport := flag.Int("max-report", 5, "maximum dangling/orphan rows to print")
	flag.Parse()
	if *datadir == "" {
		log.Fatal("-datadir required")
	}

	env, err := mdbx.NewEnv()
	if err != nil {
		log.Fatalf("NewEnv: %v", err)
	}
	defer env.Close()
	if err := env.SetOption(mdbx.OptMaxDB, 64); err != nil {
		log.Fatalf("SetOption maxdb: %v", err)
	}
	if err := env.Open(filepath.Join(*datadir, "db"), mdbx.Readonly|mdbx.NoSubdir, 0o644); err != nil {
		log.Fatalf("Open: %v", err)
	}

	if err := env.View(func(txn *mdbx.Txn) error {
		dbi, err := txn.OpenDBISimple("AccountsTrie", 0)
		if err != nil {
			return fmt.Errorf("open AccountsTrie: %w", err)
		}

		// Pass 1: collect every row's key as a set, plus map parent_path → BNC.
		// AccountsTrie keys are variable-length nibble bytes (0..=64).
		rowKeys := make(map[string]iReth.BranchNodeCompact)
		cur, err := txn.OpenCursor(dbi)
		if err != nil {
			return fmt.Errorf("open cursor: %w", err)
		}
		k, v, err := cur.Get(nil, nil, mdbx.First)
		for ; err == nil; k, v, err = cur.Get(nil, nil, mdbx.Next) {
			if len(k) > 64 {
				return fmt.Errorf("AccountsTrie row has key length %d > 64 (wire-format bug suspected): %s",
					len(k), hex.EncodeToString(k))
			}
			var node iReth.BranchNodeCompact
			node.DecodeCompact(v, len(v))
			rowKeys[string(k)] = node
		}
		cur.Close()
		fmt.Printf("AccountsTrie rows scanned: %d\n", len(rowKeys))

		// Pass 2: for each row's tree_mask bits, check child row exists.
		danglingTree := 0
		danglingHash := 0
		orphan := 0
		examples := 0
		for keyStr, node := range rowKeys {
			pathNibbles := []byte(keyStr)
			for slot := uint16(0); slot < 16; slot++ {
				bit := uint16(1) << slot
				if node.TreeMask&bit != 0 {
					childKey := make([]byte, len(pathNibbles)+1)
					copy(childKey, pathNibbles)
					childKey[len(pathNibbles)] = byte(slot)
					if _, ok := rowKeys[string(childKey)]; !ok {
						danglingTree++
						if examples < *maxReport {
							fmt.Printf("  DANGLING tree_mask: parent=%s slot=%x → expected child key=%s (not found)\n",
								hex.EncodeToString(pathNibbles), slot, hex.EncodeToString(childKey))
							examples++
						}
					}
				}
				// hash_mask without state_mask is a separate invariant
				// (the alloy_trie BranchNodeCompact::new assertion), but we
				// also count it just in case.
				if node.HashMask&bit != 0 && node.StateMask&bit == 0 {
					danglingHash++
				}
			}
		}

		// Pass 3: for each non-root row, check the parent exists and has
		// state_mask bit set for this slot.
		orphanExamples := 0
		for keyStr := range rowKeys {
			pathNibbles := []byte(keyStr)
			if len(pathNibbles) == 0 {
				continue
			}
			parentNibbles := pathNibbles[:len(pathNibbles)-1]
			slot := pathNibbles[len(pathNibbles)-1]
			parent, ok := rowKeys[string(parentNibbles)]
			if !ok {
				orphan++
				if orphanExamples < *maxReport {
					fmt.Printf("  ORPHAN: child=%s exists but parent=%s missing\n",
						hex.EncodeToString(pathNibbles), hex.EncodeToString(parentNibbles))
					orphanExamples++
				}
				continue
			}
			bit := uint16(1) << uint16(slot)
			if parent.StateMask&bit == 0 {
				orphan++
				if orphanExamples < *maxReport {
					fmt.Printf("  ORPHAN: child=%s parent=%s state_mask=0x%04x slot=%x not set\n",
						hex.EncodeToString(pathNibbles), hex.EncodeToString(parentNibbles),
						parent.StateMask, slot)
					orphanExamples++
				}
			}
		}

		fmt.Printf("\nResults:\n")
		fmt.Printf("  dangling_tree_mask  : %d (tree_mask bit set without child row on disk)\n", danglingTree)
		fmt.Printf("  dangling_hash_only  : %d (hash_mask bit set without matching state_mask bit)\n", danglingHash)
		fmt.Printf("  orphan_rows         : %d (row exists but parent missing OR parent's state_mask bit clear)\n", orphan)
		if danglingTree == 0 && danglingHash == 0 && orphan == 0 {
			fmt.Printf("  status              : OK — AccountsTrie is internally consistent\n")
		} else {
			fmt.Printf("  status              : FAIL — writer has a bug\n")
		}
		return nil
	}); err != nil {
		log.Fatalf("view: %v", err)
	}
}
