package reth

// Pinned versions of the upstream reth artifacts we mirror. Bumping any of
// these requires regenerating testdata/fixtures.json (see testdata/README.md)
// and, once it lands, running the differential oracle that will live at
// client/reth/oracle_test.go (Slice F scope).
const (
	// PinnedRethCommit is the exact reth source SHA whose schema and codec
	// this package mirrors.
	PinnedRethCommit = "6fa48a497a"

	// PinnedCodecsVer is the reth-codecs crate.io version whose Compact
	// encoding this package reproduces.
	PinnedCodecsVer = "0.3.1"

	// PinnedAlloyTrieVer is the alloy-trie crate.io version whose
	// BranchNodeCompact format this package reproduces.
	PinnedAlloyTrieVer = "0.9.5"

	// PinnedRethRelease is the Docker image tag the differential oracle
	// + e2e suite boot against in CI.
	//
	// Pinned by content digest (sha256) appended to the release tag. The
	// digest is the multi-arch index digest that Docker resolves; the tag
	// is a human-readable hint. A tag can move, so the digest pin keeps
	// CI reproducible.
	//
	// v2.7.0 is the reth release that schedules the Sepolia Amsterdam
	// fork (paradigmxyz/reth#27350, alloy-hardforks 0.4.9
	// SEPOLIA_AMSTERDAM_TIMESTAMP = 1_791_294_816). It reads the
	// Amsterdam HeaderExt fields (block_access_list_hash, slot_number)
	// and boots both the Osaka and the Amsterdam genesis. It also
	// contains `--debug.skip-genesis-validation` (paradigmxyz/reth#23919,
	// merged 2026-05-06). State-actor's writer-direct datadir requires
	// that flag — without it reth recomputes the genesis hash from the
	// chainspec's alloc and rejects the DB.
	//
	// The previous pin was nightly@sha256:e528857e5e9e...d206e0, which
	// does not know the Sepolia Amsterdam spec.
	PinnedRethRelease = "v2.7.0@sha256:a7f059d470878a5a59f3c575f3fc81b536d0d2d56d7a1cbed73e717a4f766336"

	// PinnedRethImage is the fully-qualified image reference (registry + name)
	// without the tag. Reth is published to GHCR. Migrated from CPerezz/reth
	// (fork-only `skip-genesis-validation` branch) once the upstream PR
	// landed in main.
	PinnedRethImage = "ghcr.io/paradigmxyz/reth"

	// PinnedMdbxGoVer is the github.com/erigontech/mdbx-go module version
	// that the cgo writer links against. Pinned because libmdbx C ABI is
	// version-sensitive.
	PinnedMdbxGoVer = "v0.38.4"

	// DBVersion is the value written to <datadir>/db/database.version. Reth
	// boot validates this exact value; mismatch fails fast.
	DBVersion = 2
)
