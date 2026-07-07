package snap

// Domain identifies one of Erigon's E3 state domains. The on-disk
// file tag for each is given by Tag().
type Domain int

const (
	DomainAccounts Domain = iota
	DomainStorage
	DomainCode
	DomainCommitment
)

// Tag returns the on-disk filename tag for d. Verified against
// erigontech/erigon/db/state/domain.go:1719,1750
// (Domain.FilenameBase switch) and aggregator_test.go:474,484
// (test fixtures emit "commitment"). DomainCommitment's tag is
// "commitment" (singular) — earlier "commitments" plural was wrong
// and produced files Erigon's reader silently ignored, leading to
// "Wrong trie root of block 0" at stage Execution.
func (d Domain) Tag() string {
	switch d {
	case DomainAccounts:
		return "accounts"
	case DomainStorage:
		return "storage"
	case DomainCode:
		return "code"
	case DomainCommitment:
		return "commitment"
	default:
		return "unknown"
	}
}

// StepRange identifies the [From, To) STEP range covered by a snapshot
// file. Erigon's step size is fixed by config3.DefaultStepSize
// (390_625 txnums); a frozen file holds StepsInFrozenFile (256) of
// them ⇒ ~100M txnums ⇒ ~20k blocks at 5k tx/block.
//
// Steps are NOT zero-padded in the file name (unlike block snapshots
// which use E2_FILE_TEMPLATE's `%06d`).
type StepRange struct {
	From uint64
	To   uint64
}

// DomainEntry is one (key, value) pair to write into a domain. Keys
// MUST be sorted ascending before calling WriteDomain — the seg writer
// + btindex accessor both depend on monotonic key order.
type DomainEntry struct {
	Key   []byte
	Value []byte
}

// AccessorMask selects which sidecar accessors to emit for a domain.
// Per Verifier B's correction, this is PER-DOMAIN (not global):
//
//   - AccessorBTree     → .bt   (B+tree; value-domain default)
//   - AccessorHashMap   → .kvi  (RecSplit; commitment-domain only)
//   - AccessorExistence → .kvei (bloom filter; ALL domains)
type AccessorMask uint8

const (
	AccessorBTree AccessorMask = 1 << iota
	AccessorHashMap
	AccessorExistence
)

// Has reports whether m has flag set.
func (m AccessorMask) Has(flag AccessorMask) bool { return m&flag != 0 }

// Settings governs SnapshotWriter behaviour. Step boundaries +
// salt-derivation policy + per-domain accessor mix all live here so
// the orchestrator (client/erigon/run_cgo.go) has a single place to
// override per-spec.
type Settings struct {
	// StepSize matches Erigon's config3.DefaultStepSize (390_625).
	// Override only for tests; production must match.
	StepSize uint64

	// StepsInFrozenFile matches Erigon's config3.DefaultStepsInFrozenFile
	// (256). One frozen file covers StepSize × StepsInFrozenFile txnums.
	StepsInFrozenFile uint64

	// Salt is the 4-byte snapshot salt written to salt-state.txt.
	// If 0, NewWriter derives it deterministically from Seed via
	// DeriveSaltFromSeed (so two runs with the same seed produce
	// byte-identical snapshot files including the salt-dependent
	// existence filters).
	Salt uint32

	// Seed is state-actor's run-wide seed; threads into Salt if Salt==0.
	Seed int64

	// Accessors maps each domain to its accessor mix. Defaults to the
	// production layout if a domain is absent from the map (see
	// DefaultAccessorMask).
	Accessors map[Domain]AccessorMask

	// RecSplitWorkers parallelizes the .kvi recsplit Build (byte-identical
	// at any count; see recsplit.Args.Workers). 0/1 = sequential.
	RecSplitWorkers int

	// SnapshotVersion is the on-disk version prefix in filenames
	// (e.g. "v1.0"). Defaults to the value of
	// erigon.SnapshotFormatVersion if empty.
	SnapshotVersion string
}

// DefaultAccessorMask returns the production accessor mix for d per
// the v3.4.2 schema at
// erigontech/erigon/db/state/statecfg/version_schema_gen.go.
func DefaultAccessorMask(d Domain) AccessorMask {
	switch d {
	case DomainAccounts, DomainStorage, DomainCode:
		return AccessorBTree | AccessorExistence
	case DomainCommitment:
		return AccessorHashMap | AccessorExistence
	default:
		return AccessorExistence
	}
}
