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
	// Pinned by content digest (sha256) appended to the `nightly` tag.
	// The digest is what Docker resolves to; the `nightly` prefix is a
	// human-readable hint that this snapshot was a nightly build.
	//
	// Why a digest and not just `nightly`: the `nightly` tag in the
	// upstream registry is overwritten daily, so a fresh CI run can pull
	// a different image than yesterday and break unrelated PRs. The
	// digest pin makes builds reproducible.
	//
	// The pinned snapshot contains `--debug.skip-genesis-validation`
	// (paradigmxyz/reth#23919, merged 2026-05-06). State-actor's
	// writer-direct datadir requires that flag — without it reth
	// recomputes the genesis hash from the chainspec's alloc and rejects
	// the DB.
	//
	// Bump to a specific `vX.Y.Z` release tag once one is cut after
	// 2026-05-06 — the oldest release containing #23919 will be the
	// long-term stable pin.
	PinnedRethRelease = "nightly@sha256:e528857e5e9ebc2c6cb99f28436e70ded38ca905629f00afc98d186e27d206e0"

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
