// Package oracle provides shared helpers for the per-client oracle/boot/e2e
// tests that boot a real Ethereum node against a state-actor-generated
// datadir and assert via JSON-RPC.
//
// The first export is Reproduce: re-derives the exact (EOAs, contracts)
// stream a client adapter wrote during Phase 1, by replaying autofill's
// canonical RNG draw order. Lets the oracle side know the expected
// balances / code / storage without exposing them through the writer
// API. Lives here (not in internal/autofill) so it can depend on the
// per-client generator.Config wiring without forcing autofill itself
// to import generator.
package oracle

import (
	mrand "math/rand"

	"github.com/nerolation/state-actor/internal/autofill"
	"github.com/nerolation/state-actor/internal/entitygen"
)

// ReproduceCfg controls the entity stream Reproduce regenerates. Mirrors
// the writer-side knobs in generator.Config that affect entitygen draws:
// the seed and the resolved auto-fill Plan. Anything else (DB path,
// batch size, workers) doesn't affect the entity stream and isn't
// replicated here.
type ReproduceCfg struct {
	Seed     int64
	AutoFill *autofill.Plan
}

// Reproduce returns the (EOAs, contracts) pair a state-actor writer would
// have produced during Phase 1 for the supplied config. The draw order
// — N EOAs via Plan.DrawEOA, then M contracts via Plan.DrawContract — is
// the canonical "single source of truth" RNG sequence every writer
// follows.
//
// Identical inputs → identical outputs across calls. Use this from
// oracle / boot / e2e tests to compute expected balances / code /
// storage values to compare against eth_get* RPC results.
//
// Caveat: writers may advance the RNG further on a draw collision with
// genesis/system contracts; Reproduce assumes the canonical-MPT
// invariant configuration (no pre-existing collisions).
//
// nil-Plan semantics: when cfg.AutoFill == nil, returns (nil, nil). This
// is semantically distinct from a Plan that legitimately requested zero
// entities (NumEOAs=0 AND NumContracts=0), which also returns empty
// slices — but via the populated-Plan code path. Callers who care about
// the distinction (e.g. tests verifying that auto-fill DID run) MUST
// supply a non-nil Plan; the function intentionally does NOT error on
// nil-AutoFill because spec-only runs legitimately have no auto-fill.
func Reproduce(cfg ReproduceCfg) (eoas, contracts []*entitygen.Account) {
	if cfg.AutoFill == nil {
		return nil, nil
	}
	rng := mrand.New(mrand.NewSource(cfg.Seed))
	plan := cfg.AutoFill
	eoas = make([]*entitygen.Account, plan.NumEOAs)
	for i := 0; i < plan.NumEOAs; i++ {
		eoas[i] = plan.DrawEOA(rng)
	}
	contracts = make([]*entitygen.Account, plan.NumContracts)
	for i := 0; i < plan.NumContracts; i++ {
		contracts[i] = plan.DrawContract(rng)
	}
	return eoas, contracts
}
