// Package clientpolicy centralizes per-(flag, client) compatibility rules.
//
// The previous main.go shape carried a recognition switch + three
// per-client `if *client == "X"` validation blocks (~50 lines of
// scattered if/log.Fatalf). Each addition of a new client meant
// remembering to add a fourth/fifth block and updating the human-readable
// error strings in three places. This package collapses that into one
// table-driven function so the rules live next to each other and
// surfacing them in tests is trivial.
package clientpolicy

import (
	"fmt"

	"github.com/nerolation/state-actor/genesis"
)

// FlagValues is the subset of CLI flags ValidateForClient needs to make a
// per-client compatibility decision. The rest of the flags either apply
// uniformly to every client (--accounts, --contracts, --seed, ...) or are
// validated by the flag parser itself (--db required, --extra-data hex
// well-formed, ...).
type FlagValues struct {
	BinaryTrie bool   // --binary-trie
	TargetSize string // --target-size (non-empty means active)
	Fork       string // --fork (canonical fork name; "" means "auto")
}

// ValidateForClient returns nil when every flag in fv is compatible with
// the chosen client. Otherwise it returns a human-readable error matching
// the strings the legacy if-blocks in main.go used to emit, so existing
// CI and documentation that grep for these strings keep working.
//
// Two layers:
//  1. Client recognition: erigon → not-yet-implemented; unknown → reject.
//  2. Per-client compatibility: --binary-trie is geth-only (the others
//     lack EIP-7864 support); --target-size is incompatible with reth;
//     --fork is clamped at each client's writer ceiling (see
//     genesis.MaxForkForClient).
func ValidateForClient(client string, fv FlagValues) error {
	switch client {
	case "geth", "nethermind", "besu", "reth":
		// recognized; per-flag checks below
	case "erigon":
		return fmt.Errorf("--client=%s is not yet implemented (planned in a follow-up PR); use --client=geth, --client=nethermind, --client=besu, or --client=reth", client)
	default:
		return fmt.Errorf("--client=%s is not recognized; valid values: geth, nethermind, besu, reth", client)
	}

	// EIP-7864 binary trie — geth-only.
	if fv.BinaryTrie && client != "geth" {
		switch client {
		case "nethermind":
			return fmt.Errorf("--binary-trie is not supported with --client=nethermind (Nethermind does not implement EIP-7864)")
		case "besu":
			return fmt.Errorf("--binary-trie is not supported with --client=besu (Besu does not implement EIP-7864)")
		case "reth":
			return fmt.Errorf("--binary-trie is not supported with --client=reth (Reth does not implement EIP-7864)")
		}
	}

	// Reth has no Phase-1 stop hook (its streaming Phase 4 sorter would
	// need a per-batch projected-size check that nobody's wired up).
	if fv.TargetSize != "" && client == "reth" {
		return fmt.Errorf("--target-size is not yet supported with --client=reth; set --accounts / --contracts explicitly")
	}

	// Per-client fork ceiling. Empty means "auto"; the caller resolves
	// to MaxForkForClient(client) so this branch never fires for the
	// auto path.
	if fv.Fork != "" {
		ceiling := genesis.MaxForkForClient(client)
		if !genesis.ForkAtLeast(ceiling, fv.Fork) {
			return fmt.Errorf("--fork=%s is past --client=%s's writer ceiling (%s); pass --fork=%s or earlier, or use a different --client",
				fv.Fork, client, ceiling, ceiling)
		}
	}

	return nil
}
