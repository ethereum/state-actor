# Changelog

## Unreleased

### Added
- **Nethermind flat-DB state generation (closes #111).** `--client=nethermind`
  now always writes Nethermind's flat-state layout — an eighth RocksDB
  (`<db>/flat`, a column DB holding Account/Storage leaf rows plus the relocated
  Merkle-Patricia trie in four node column families, and the
  `Layout`/`SlotEncoding`/`CurrentState` metadata markers) — so Nethermind
  ≥ 1.39.0 detects and serves the DB as its **flat** backend instead of
  **patricia**, matching how real networks run it. The byte-exact layout lives
  in the new dependency-free `internal/neth/flat` package, pinned to release tag
  1.39.0. The state root is unchanged (the trie is relocated into the flat DB,
  not removed). Boot a flat datadir with `--FlatDb.Enabled=true` (the bloatnet
  bench and the e2e suite pass it). The legacy patricia (`state`-DB) layout is
  no longer written.

### Changed
- **Nethermind writer runs Nethermind's own RocksDB configuration.** All
  8 databases (and every column family) now parse the verbatim DbConfig
  option strings through RocksDB's own parser instead of hand-tuned
  grocksdb defaults — bloom/ribbon filter policies, index types, block
  sizes, compression now match what Nethermind itself writes. Transient
  bulk-import tuning is layered on top and leaves no trace in the SSTs.
  A new `TestNethOptionsPersisted` locks the composed options via each
  DB's OPTIONS-* file.
- **Geth writer mirrors geth's per-level Pebble options** (10-bit bloom
  filters on L0–L5, none on L6, 2→128 MiB file-size ladder), locked to
  the pinned go-ethereum by a new OPTIONS-file parity test. Effect is
  config parity + upstream-drift detection: the shipped datadir's bulk
  data still lands filter-less in L6 via the Close-time compaction,
  matching a real synced geth node. The final compaction's error is no
  longer swallowed on the success path.
- **Pinned Nethermind image bumped 1.37.0 → 1.39.0.** The flat-DB backend and
  its startup `State backend: flat/patricia` detection log only exist in
  Nethermind ≥ 1.39.0. A new `neth.PinnedNethermindVersion` constant plus a
  consistency test keep the image pin aligned across the Makefile, bench script,
  and `validate-big-db.sh`.

### Documentation
- **RUNBOOK and `client/besu/doc.go` document Besu's p2p requirement for the Engine
  API.** Besu accepts `engine_forkchoiceUpdated` on an isolated snapshot node only with
  p2p enabled — its synchronizer must register the post-merge head as in-sync, otherwise
  Besu answers `SYNCING`.

### Added
- **`--client=ethrex` reports its memory split every 30s.** New
  `internal/memstat` samples process RSS against the Go runtime's own total —
  their difference being the cgo memory no `GOMEMLIMIT` can govern — plus
  host-wide `MemAvailable`/`Dirty`/`Writeback`, which distinguish "this process
  exhausted memory" from "unreclaimable page cache did". Alongside it the
  ethrex writer logs RocksDB's own accounting (memtables, table readers,
  block-cache usage, L0 file count), and `Close()` logs per-CF compaction
  duration and memory — the phases a SIGKILL would otherwise leave unrecorded.

### Fixed
- **Erigon datadirs survive `FCU(head=genesis)` on erigon ≥ 2e41aa8308
  (PR erigontech/erigon#22344; on `main` only, no release tag as of
  v3.5.4).** That change rebuilds `MaxTxNum[0]` from the genesis body's
  `TxCount` during the bootstrap FCU, clobbering the fat-genesis
  `StepSize-1` and failing every FCU with `seems broken TxNums index not
  filled`. `patchGenesisHeaderStateRoot` now fattens the genesis
  `BodyForStorage.TxCount` to `StepSize` and bumps `Sequence[EthTx]` to
  match, so any body-derived rebuild reproduces the fat value. Inert on
  pre-22344 daemons. Erigon datadirs must be regenerated.
- **`--client=ethrex` no longer OOM-kills on large `--target-size` runs
  (closes #116).** A 350 GB fill died at 51.3 GiB anon RSS on a 62 GiB host
  (kernel-confirmed OOM; 16 GiB of it transparent huge pages). Fixed on two
  fronts. **Allocator**: the runtime image now runs RocksDB on jemalloc via
  `LD_PRELOAD` — RocksDB-on-glibc is the documented pathological pairing
  behind unbounded allocator retention, and jemalloc is what ethrex itself
  ships as its default global allocator (`MALLOC_ARENA_MAX=2` stays as the
  fallback for a failed preload). **Structure**, aligning the writer with the
  besu/nethermind/geth/erigon disciplines: Phase-2 workers capped at 8 (was
  uncapped `NumCPU()`); state-CF memtables 256 MiB × 4 (was 512 MiB × 6 with
  min-merge) under a 4 GiB `db_write_buffer_size` backstop; index and
  bloom-filter blocks routed through the shared block cache (512 MiB — as a
  4 GiB bystander they sat in per-SST table readers outside every budget)
  with `max_open_files` bounded; `Close()`-time CompactRange serialized
  (concurrent manual compactions of never-compacted L0 stacked readahead at
  the run's peak); Phase-1 sort spill moved from `/tmp` to the datadir
  volume. The new `internal/memlimit` derives a `GOMEMLIMIT` from the host's
  real ceiling (cgroup v2 → cgroup v1 → `/proc/meminfo`) minus the writer's
  declared off-heap reserve; an explicit `GOMEMLIMIT` wins, and a limit too
  small to help is declined and logged — one below the live heap trades an
  OOM kill for an unbounded GC stall. The reserve is measured, not guessed:
  a 40 GB same-seed A/B put the jemalloc off-heap plateau at ~4.4 GiB
  (flat; the glibc baseline climbed to 6.2 GiB on identical work), so the
  reserve is 8 GiB — budgeted caps plus a 1.5 GiB allocator slack — and the
  heap takes half the post-reserve remainder. These are process-runtime knobs only:
  the produced database is **logically identical** — same KV content, same
  state root, pinned by `TestEthrexGoldenStateRoot` and
  `TestGenesisDumpGolden` (physical SST packing legitimately varies with
  flush cadence).

- **`erc20` template now honors `approximate_size_bytes`.** Previously
  the universal entity-level sizing knob was silently ignored on the
  `erc20` template (only `raw` and `eoa` consumed it), even though
  [`docs/SPEC.md`](docs/SPEC.md) described it as the cross-template
  storage-budget control. Now the slot budget falls back to deriving
  `total_owners` (one slot per random holder, minus up to three metadata
  slots — `_name`/`_symbol` always, `_totalSupply` only when supply > 0).
  Explicit
  `total_owners` / `total_allowances` continue to win — precedence is
  "explicit > implicit", matching the existing `owners` + `total_owners`
  composition pattern. Adds four regression tests in
  `internal/templates/erc20_test.go` pinning the new derivation,
  equivalence with explicit `total_owners`, explicit-precedence, and
  the floor against shrinking explicit owners.

### Breaking
- **Removed `--accounts`, `--contracts`, `--max-slots`, `--min-slots`,
  `--distribution`, `--code-size` flags.** Synthetic state generation is
  now driven by a single `--target-size` flag plus the new
  `internal/autofill` Plan, which emits mainnet-shaped 20 / 10 / 70
  proportions (account-trie / bytecode / contract storage) up to the
  budget. Per-contract code is a truncated normal in `[1 KiB, 24 KiB]`
  centered at 5 KiB; per-contract storage size is a truncated normal in
  `[1 KiB, 100 MiB]` with budget-derived mean. EOAs randomize balance
  (90 % non-zero), nonce (always non-zero), and EIP-7702 delegation
  (30 %) independently. **Closes
  [ethereum/state-actor#82](https://github.com/ethereum/state-actor/issues/82).**
- **`--target-size` is now required when `--spec` is not set.** Replaces
  the previous default-1100-entity silent behavior. Existing invocations
  that combined `--spec` with default `--accounts=1000 --contracts=100`
  (and worked around the collision with `--accounts=0 --contracts=0`)
  become simply `--spec=...`.
- **Golden state-root hashes regenerated.**
  `entitygen.CanonicalOsakaMPTRoot` and the binary-trie golden in
  `generator/generator_test.go` are pinned to the new auto-fill output.

### Added
- **`--spec <file>.yaml` flag** — declarative state customization via YAML.
  Users can specify concrete EOAs + contracts (with optional ERC-20
  template, raw bytecode, EIP-7702 delegation marker, balance/nonce, and
  `approximate_size_bytes` storage bloat). Spec entities are written
  first; the synthetic-fill loop then runs on top until `--target-size`
  is reached. See [`docs/SPEC.md`](docs/SPEC.md) for the schema. Closes
  the customizable-state-generation feature plan.
- **Deterministic invariant**: same `--spec` + same `--seed` produces
  byte-identical state on the same client. Pinned at unit level
  (address derivation, storage synthesis, end-to-end PreAlloc) and at
  CI level via `client/geth/e2e_test.go:TestE2ESuiteSpec` (geth boot +
  spamoor + golden-hash). Cross-client byte-equal state root across
  geth/besu/nethermind/reth lands in v1.5 (see limitations).
- New packages: `internal/spec/` (YAML parser + schema), `internal/templates/`
  (template registry: `erc20`, `raw`, `eoa`), `internal/specbuild/`
  (Spec→entities translator with 3-mode address resolution),
  `internal/sizecal/` (per-client storage-slot calibration table).

### Changed
- `generator.Config` gained a `PreAlloc []templates.PreAllocEntity` field
  populated by `--spec`. `Config.Validate()` materializes PreAlloc into
  the legacy `GenesisAccounts/Code/Storage` maps so existing client
  writer code paths handle spec entities unchanged.
- Nethermind synthetic-accounts writer (`client/nethermind/`) now
  threads alloc storage through the storage-trie path — **closes
  https://github.com/ethereum/state-actor/issues/22**. Specs combining
  storage-bearing entities with synthetic auto-fill (`--target-size`)
  work on nethermind for the first time.

### Removed
- **`--inject-accounts` flag AND `Config.InjectAddresses` Go field** —
  fully superseded by `--spec`. Migration: an EOA that was previously
  written as `--inject-accounts=0xABC...` is now declared in YAML as:
  ```yaml
  entities:
    - kind: eoa
      address: 0xABC...
      balance: "999999999000000000000000000"
  ```
  Programmatic Go callers that wired `cfg.InjectAddresses = [...]` must
  migrate to `cfg.PreAlloc = [...]templates.PreAllocEntity{...}`. All
  in-tree CI/test code already migrated. The per-client writer code
  paths that consumed `cfg.InjectAddresses` have been deleted in geth
  / besu / nethermind / reth.

### Tested
- Per-package unit tests (`internal/spec/`, `internal/templates/`,
  `internal/specbuild/`, `internal/sizecal/`, `generator/prealloc_test.go`).
- `TestMainSpecFlagSmoke` (in default CI job): builds state-actor, runs
  `--spec` end-to-end against the geth writer, asserts the db dir is
  non-empty. Pins the wiring CLI → parser → templates → specbuild →
  Config.PreAlloc → writer.
- `TestMainInjectAccountsFlagRemoved`: confirms the removed flag exits
  non-zero — prevents an accidental re-add.
- **Every per-client `TestE2ESuite` now drives `Config.PreAlloc` from
  `examples/spec-ci-baseline.yaml`** via the shared
  `internal/e2e_testing.LoadCISpecPreAlloc` helper. Same YAML on all
  four clients (geth/besu/nethermind/reth) → identical state root
  (via `sizecal.NewFixed(64)`) → the existing
  `cross-client-genesis-root` aggregator job automatically becomes the
  cross-client spec invariant. This is the v1 CI guarantee: `--spec`
  drives writer → boot → spamoor → RPC re-query → golden-hash on
  every client.
- **`CheckInjections` extended** to walk `cfg.GenesisAccounts` for
  balance verification AND `cfg.GenesisStorage` (with deterministic
  sampling — see below) for storage-slot verification. All spec
  entities are now RPC-verified at Phase 4: balances, code, AND
  storage slots get the round-trip eth_getBalance / eth_getCode /
  eth_getStorageAt treatment.
- **Storage-slot sampling**: `sampleStorageSlots` deterministically
  picks up to 5 keys per entity (first/last/middle-spaced) from a
  sorted view. Same input → same sampled keys → reproducible across
  CI runs and clients. Bounds RPC roundtrips at
  O(addresses × 5) regardless of fixture size; ~30 calls for the
  CI baseline. Catches the bug class "spec entity injected into the
  writer but vanished by RPC time" — ERC-20 holder balances, 7702 EOA
  storage-bloat slots, raw-contract synthesized storage.
- `examples/spec-ci-baseline.yaml`: rewritten as the rich CI fixture
  (~12 entities) including the spamoor sender at
  `oracle.SpamoorSenderAddr`, replacing the legacy
  `cfg.InjectAddresses: [SpamoorSenderAddr]` mechanism. Exercises every
  schema variant: explicit/name-derived/position-derived addresses,
  ERC-20 with `holders`, ERC-20 with explicit nonce, raw bytecode, 7702
  delegation, 7702 + storage bloat, plain EOAs.
- **`internal/e2e_testing/spec_setup_test.go:TestCISpecMatchesSpamoorSender`**
  asserts the YAML's spamoor entity address matches
  `oracle.SpamoorSenderAddr` — catches future drift.
- Audit-driven coverage additions:
  - `TestValidateRejectsEIP170OversizeCode` + `TestValidateAcceptsExactlyMaxCodeSize`
  - `TestValidateCaseSensitiveKind` + `TestValidateCaseSensitiveTemplate`
  - `TestParseBalanceRejections` (8 sub-cases: underscored, scientific,
    negative, float, bool, alpha-no-prefix, empty, unquoted-int)
  - `TestParseBalanceMaxUint256` + `TestParseBalanceOverflowUint256`
  - `TestParseAddressEdgeCases` (zero, max, too-long, prefix-only, unquoted-hex)
  - `TestParseCodeEdgeCases` (empty, prefix-only, single-byte, 23-byte 7702 marker, odd-length, non-hex)
  - `TestERC20BalancesSlotComputationManyHolders` (extends single-holder
    Solidity-equivalence to 25 holders)
  - `TestERC20NonceHonorsUserValue` (3 sub-cases pinning the EIP-161 floor + user override)
  - `TestValidateRejectsSpecExceedingTargetSize` + `TestValidateAcceptsSpecUnderTargetSize`
  - `TestBuildDeterminismEndToEnd`

### Limitations (tracked for v1.5)
- `--spec` materializes `approximate_size_bytes` storage into a Go map
  before writers consume it; per-entity practical limit is ~1 GB on
  16 GB RAM. Multi-GB per-entity workloads (Story 1's "10 GB ERC-20")
  will gain a streaming writer integration in a follow-up that doesn't
  change the schema.
- `erc20` template ships with a stub runtime bytecode in v1 — storage
  layout is correct (OZ v5: `_balances` mapping at slot 0,
  `_totalSupply` at slot 2, short-string `_name`/`_symbol` at slots
  3/4) but `eth_call balanceOf()` returns zero. Audited OZ v5 runtime
  bytecode lands as a one-file v1.5 swap.
- `erc721` and `uniswapv2` templates are deferred to v1.5 (the registry
  pattern makes adding them a single-file change).
- **Cross-client spec-state-root invariant: in CI for all 4 clients
  from v1.** Every existing `TestE2ESuite` now loads
  `examples/spec-ci-baseline.yaml` via the shared helper and runs
  through the same boot + spamoor + golden-hash pipeline. The existing
  `cross-client-genesis-root` aggregator job pins byte-equal state root
  across geth/besu/nethermind/reth. The `sizecal.NewFixed(64)`
  override in the helper neutralizes per-client calibration divergence
  so the invariant is robust.
- **ERC-20 template hardcodes nonce-floor at 1.** Per EIP-161, contracts
  on Spurious-Dragon+ forks have nonce ≥ 1. Users who explicitly set
  `nonce: 0` on a `template: erc20` entity get nonce=1 silently.
  Override by setting `nonce: 1` (or higher) explicitly. v1.5 may grow
  a `*uint64` Entity.Nonce to distinguish "unset" from "explicit 0".
