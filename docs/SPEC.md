# `--spec` YAML schema

> [!IMPORTANT]
> **This file is the schema reference** — parser rules, validation errors,
> the address-resolution algorithm, `approximate_size_bytes` semantics.
>
> **The syntax reference** — every feature shown in practice — is
> [`../examples/full-matrix-spec-feature.yaml`](../examples/full-matrix-spec-feature.yaml).
> Read it alongside this file. The fixture is CI-pinned by
> `TestBuildFullMatrix`, every per-client `TestE2ESuite`, and the
> cross-client genesis-root invariant job, so it stays correct
> automatically. [`docs/SKILL.md`](SKILL.md#canonical-spec-reference) has an
> intent → entity-# index for navigating it.

State-actor's `--spec` flag accepts a YAML file declaring concrete entities
(EOAs + contracts) the writer must include in generated genesis state.
Spec entities are written first; if `--target-size` is also set, the
mainnet-shaped auto-fill (20 % account-trie / 10 % bytecode / 70 %
storage) fills the headroom on top.

## Quick start

```bash
state-actor --client=reth --db=/tmp/mychain --spec=examples/spec-erc20-mixed-sizes.yaml --target-size=20GB
```

Once the DB is written, boot the client per [`RUNBOOK.md`](RUNBOOK.md). For
example specs covering each capability, see [`examples/README.md`](../examples/README.md).

## Schema

```yaml
entities:
  - kind: contract | eoa     # required
    name: string             # optional; used for pretty-print + name-derived address
    address: 0x...           # optional; explicit 20-byte address
    balance: "1000000000000000000"  # optional; wei, MUST be a quoted string
    nonce: 0                 # optional; default 0
    code: "0x..."            # optional; required iff template is absent on contracts
    template: erc20          # optional; for kind=contract only
    parameters: { ... }      # optional; template-specific (only with `template`)
    approximate_size_bytes: 1_000_000   # optional; synthesizes storage slots
```

### `kind`

One of:

- `contract` — a smart contract with deployed bytecode. **Must** set
  exactly one of `template:` or `code:`. May set `approximate_size_bytes:`
  to populate synthetic storage.
- `eoa` — an externally-owned account. May set `code:` (e.g. an EIP-7702
  23-byte `0xef0100<addr>` delegation marker). May set
  `approximate_size_bytes:` for delegated-storage bloat. MUST NOT set
  `template:` or `parameters:`.

### Address resolution (three deterministic modes)

1. **Explicit**: `address: 0xABC...` is set — used verbatim.
2. **Name-derived**: `address:` omitted, `name:` set —
   `keccak256(seed || name)[12:]`. Same `name + --seed` always produces
   the same address (good for cross-run determinism).
3. **Position-derived**: both omitted —
   `keccak256(seed || "anon-N")[12:]` where `N` is the entity's index.
   Reordering entities in the YAML changes their derived addresses;
   explicit/named entities are stable across reorderings.

### `balance`

Wei, **must be a quoted string**. Unquoted balances are rejected because
YAML's scalar resolution would silently lose precision for values
larger than 2^53. Decimal and `0x`-prefix hex are both accepted:

```yaml
balance: "1000000000000000000"        # 1 ETH decimal
balance: "0xde0b6b3a7640000"          # 1 ETH hex
```

### `approximate_size_bytes`

Target on-disk byte budget for this entity's storage. Resolved to a
synthetic slot count via the single global trie-only `bytesPerSlot`
constant in [`internal/sizecal/factors.go`](../internal/sizecal/factors.go)
(identical across clients by design — required by the cross-client
genesis-root invariance gate). Slots are populated with deterministic
`(key, value)` pairs derived from `(seed, address)`.

- **RAM**: spec storage flows through a per-entity streaming pipeline
  (`internal/streamingtrie` + `internal/streamsort`). Total writer RAM
  stays at ~2 GB peak (a tuned Pebble MemTable per active entity)
  regardless of slot count.
- **Disk**: per-entity bound is the temp-sort working set (`slot_count
  × 96 B` in Pebble) colocated with the output datadir; freed when the
  entity finishes writing.
- **Accuracy**: ±25% versus the realised on-disk size, set by the
  global `bytesPerSlot` constant.

### Per-template precedence

When a template defines its own sizing parameter (e.g. `erc20`'s
`total_owners`, `storage_pattern`'s `final`, `create_preimage_deploys`'s
`count`), the **explicit template parameter always wins** over
`approximate_size_bytes`. `approximate_size_bytes` is a fallback that
applies only when none of the template-specific sizing parameters are
set. This matches the principle that explicit user input always takes
precedence over implicit byte budgets.

For `erc20` specifically: if neither `total_owners` nor
`total_allowances` is set, `approximate_size_bytes` derives the random
owner count (one slot per holder, minus up to three metadata slots:
`_name` and `_symbol` are always written; `_totalSupply` only when at
least one holder has a non-zero balance).

## Templates

| Template | Required parameters | Optional | Notes |
|---|---|---|---|
| `erc20`  | `symbol`, `name`, `decimals` | `owners`, `allowances`, `total_owners`, `total_allowances` | Vendored OpenZeppelin v5.6.1 ERC20 deployed runtime bytecode (`internal/templates/erc20_oz_v5.hex`, regenerate via `scripts/regen-erc20-bytecode.sh`). `decimals` must equal 18 (OZ v5 base default); use the `raw` template for other decimals. |
| `sequential_eoas` | `count` | `balance` | One entity → `count` plain EOAs at `[address, address+count)`. Anchor address comes from the entity's resolved address. `balance` defaults to `1` wei when omitted; explicit `balance: "0"` is rejected (zero-balance plain EOAs are pruned by EIP-161, leaving the planted addresses empty). Backs `SequentialAddressLayout` in bloatnet benchmarks. |
| `sequential_pkey_eoas` | `start_pkey`, `count` | `balance` | One entity → `count` plain EOAs whose addresses are derived from the secp256k1 scalar sequence `[start_pkey, start_pkey+count)`. Unlike `sequential_eoas` (which plants at sequential *addresses* the caller doesn't have keys for), this template plants at sequential *private keys* so the caller can sign txns FROM the planted accounts. Entity-level `address:` is IGNORED. `start_pkey` must be a 32-byte hex string representing a valid secp256k1 scalar (non-zero, below the curve order). `balance` defaults to `1` wei; explicit `balance: "0"` rejected (EIP-161 pruning). Backs `yield_distinct_sender()` in bloatnet `test_transaction_types`, where senders are `EOA(key=SENDER_BASE_KEY + i)`. |
| `storage_pattern` | `final` | — | Plants `slot 0 = final + 1` (next-free pointer) plus `slot k = k` for `k in 1..final`. Anchor address = entity's resolved address. Entity-level `nonce:` is honored; defaults to 1 (forced ≥ 1 so EIP-161 empty-account pruning doesn't wipe the entry). Backs `test_sload_bloated` / `test_sstore_bloated` `existing_slots=True`. |
| `create2_factory` | — | — | Plants the 69-byte Arachnid deterministic-deployment proxy runtime. The deployed address defaults to the canonical Arachnid factory address `0x4e59b44847b379578588920cA78FbF26c0B4956C` when neither `address:` nor `name:` is set; explicit `address:` (or name-derived) values are honored verbatim. The runtime is always the canonical Arachnid bytecode regardless of address. **Invariant**: if any `create2_deploys` entity uses the Arachnid factory (its `factory:` is unset or explicitly set to the Arachnid address), specbuild rejects the spec unless at least one `create2_factory` entity resolves to that address. The check is narrow to Arachnid — custom factories at non-Arachnid addresses are the user's responsibility. |
| `create2_deploys` | `salt_count`, plus `initcode`+`runtime` OR `code_pattern` | `salt_start`, `factory`, `storage_init` | For each salt in `[salt_start, salt_start+salt_count)`, derives the CREATE2 address and plants the resolved runtime there. Either supply literal `initcode:` + `runtime:` (shared across all derived addresses), OR `code_pattern:` to opt into a named generator (see below). `factory` defaults to the canonical Arachnid address. `storage_init` is an optional map of slot → value applied identically to every derived contract. |
| `create_preimage_deploys` | `sender`, `count`, plus `runtime` OR `code_pattern` | `start_nonce`, `storage_init` | For each nonce in `[start_nonce, start_nonce+count)`, derives `keccak256(rlp([sender, nonce]))[12:]` and plants the resolved runtime there. Either supply literal `runtime:` (shared) or `code_pattern:` (named generator). `storage_init` is an optional map of slot → value applied identically to every derived contract. Backs `CreatePreimageLayout` in bloatnet benchmarks (EXISTING_CONTRACT mode of `test_account_access`). |

### `code_pattern` — named per-address runtime generators

Both `create2_deploys` and `create_preimage_deploys` accept an
optional `code_pattern:` parameter that opts into a named generator
producing per-derived-address byte-unique runtime. `code_pattern:` is
**mutually exclusive** with literal `runtime:` (and, on
`create2_deploys`, also with `initcode:`) — the pattern owns both.

| Pattern name | Description |
|---|---|
| `unique_jumpdest_pre_amsterdam` | 24 KiB-per-contract runtime matching execution-specs `build_unique_contract_initcode` (Fusaka-era, EIP-170 limit = 0x6000). Entry: `PUSH2 0x5FFF; JUMP`; bytes 0x2C..0x40 hold the derived contract's own 20-byte address; remainder is `JUMPDEST` filler with a valid `JUMPDEST` byte at 0x5FFF where the entry JUMP lands. Backs the `diff_to_unique_code_jumpdest_contract` case in `test_transaction_types.py`. |

Amsterdam (EIP-7907) raises `MAX_CODE_SIZE`, at which point a sibling
`unique_jumpdest_amsterdam` pattern will be added. Existing
pre-Amsterdam fixtures stay valid by their explicit name suffix.

Symmetry note: `create2_deploys` and `create_preimage_deploys` are
twin templates — their only meaningful difference is the
address-derivation algorithm (CREATE2 vs CREATE). Every other
parameter (`runtime`, `storage_init`, `code_pattern`) behaves
identically. If you need per-derived-address custom code (e.g.
embedding each contract's own address in its runtime), use a named
`code_pattern:` (see above) — the built-in
`unique_jumpdest_pre_amsterdam` covers the bloatnet unique-code
layout, and new address-aware shapes are added as named patterns
alongside it. Arbitrary address-aware initcode (EVM simulation) stays
out of scope until a benchmark needs it.

### `erc20` parameters in detail

```yaml
- kind: contract
  template: erc20
  parameters:
    symbol: USDC                                  # required, ≤31 bytes
    name: USD Coin                                # required, ≤31 bytes
    decimals: 18                                  # required; must equal 18

    # Optional: granular per-owner balances. Each entry plants
    # _balances[address] = balance. Duplicate addresses are rejected.
    owners:
      - { address: "0x1111111111111111111111111111111111111111", balance: "1000000000000000000" }
      - { address: "0x2222222222222222222222222222222222222222", balance: "500000000000000000" }

    # Optional: bulk-fill target. total_owners - len(owners) additional
    # random holders are synthesized with deterministic varied balances
    # in [1, 10^18] wei. Must satisfy total_owners >= len(owners).
    total_owners: 20000000

    # Optional: granular per-pair allowances. Each entry plants
    # _allowances[owner][spender] = allowance. Duplicate (owner, spender)
    # pairs are rejected. Allowance owner doesn't need a balance entry —
    # ERC-20 allows approving from zero balance.
    allowances:
      - { owner: "0x1111111111111111111111111111111111111111", spender: "0x3333333333333333333333333333333333333333", allowance: "100" }

    # Optional: bulk-fill target for the allowances mapping. Same pattern
    # as total_owners.
    total_allowances: 5000000
```

`approximate_size_bytes` (set at the entity level, not inside
`parameters:`) works as a fallback: when neither `total_owners` nor
`total_allowances` is set, the slot budget is converted to a random
holder count (one slot per holder, minus up to three metadata slots —
`_name`/`_symbol` always, `_totalSupply` when supply > 0).
The example below produces ~71.4M random `_balances` entries at the
calibrated 140 B/slot cost:

```yaml
- kind: contract
  template: erc20
  approximate_size_bytes: 10_000_000_000      # ~10 GB trie → ~71.4M slots
  parameters:
    symbol: BIG
    name: BigToken
    decimals: 18
```

If `total_owners` (or `total_allowances`) is also set, the explicit
value wins and `approximate_size_bytes` is ignored.

`_totalSupply` is auto-summed from every planted balance (explicit +
random). Users cannot override it — the ERC-20 conservation invariant
is preserved by construction.

Type rules inside `parameters`: addresses, balances, and allowances
**must be quoted strings**, because yaml.v3 decodes nested maps via
`map[string]any` and our custom hex/uint256 hooks only apply at the
top-level entity fields.

### Repricing-benchmark templates in detail

These six templates were added to drive prestate for the bloatnet
benchmarks under `execution-specs/tests/benchmark/stateful/`. All have
`UserVisible() == true` and dispatch via the YAML `template:` field
under `kind: contract`.

```yaml
# Sequential EOAs — one entity expands to `count` plain EOAs at
# [address, address+count). Anchor address = entity's resolved address.
- kind: contract
  template: sequential_eoas
  address: 0x0000000000000000000000000000000000001000
  parameters:
    count: 1000000              # required; uint64
    balance: "1000000000000000000"  # optional; wei, defaults to 1.
                                    # Must be > 0 — a zero-balance plain
                                    # EOA (no code, nonce 0) would be
                                    # pruned by EIP-161, leaving no
                                    # account at the planted address.

# Storage pattern — slot 0 = final + 1, slot k = k for k in 1..final.
# Anchor address = entity's resolved address. Entity-level nonce/balance
# are honored; nonce defaults to 1 (forced >= 1).
- kind: contract
  template: storage_pattern
  address: 0x3f8074692982594c1936bd27433a8b6e5d77e0f0
  nonce: 1
  parameters:
    final: 50000000             # required; uint64

# CREATE2 factory — plants the canonical Arachnid factory runtime.
# `address:` defaults to 0x4e59b44847b379578588920cA78FbF26c0B4956C
# (the canonical Arachnid address) when neither `address:` nor `name:`
# is set. Explicit non-Arachnid addresses are accepted; the planted
# runtime is always the canonical Arachnid bytecode.
- kind: contract
  template: create2_factory
  # address: 0x4e59b44847b379578588920cA78FbF26c0B4956C  # optional

# CREATE2 deploys — one entity expands to N CREATE2-derived contracts.
# Constructor is never executed; `runtime` is what lands at every
# derived address. Use the same initcode the chain actually uses so the
# derivation matches; `runtime` may differ from initcode's intended
# return value if you want a synthetic body. `storage_init` (optional)
# applies the same slot→value map at every derived address.
- kind: contract
  template: create2_deploys
  parameters:
    initcode: "0x6080..."       # required; hex bytes (drives CREATE2 derivation)
    runtime: "0x6080..."        # required; hex bytes (planted at every derived addr)
    salt_count: 1000            # required; uint64
    salt_start: 0               # optional; uint64, defaults to 0
    factory: "0x4e59...956c"    # optional; defaults to canonical Arachnid
    storage_init:               # optional; same map planted on every derived contract
      "0x0": "0x...padded-32-bytes..."

# CREATE-preimage deploys — Bittrex-style chain. For each nonce in
# [start_nonce, start_nonce+count), derive crypto.CreateAddress(sender,
# nonce) and plant `runtime` there. The sender is supplied as a
# parameter (NOT the entity's address), so the user is free to also
# declare a separate entity (`template: raw`, etc.) at the sender's
# address. `storage_init` (optional) applies the same slot→value map
# at every derived address — e.g. set slot 0 = controller address on
# every Bittrex child.
- kind: contract
  template: create_preimage_deploys
  parameters:
    sender: "0xA3C1E324CA1CE40DB73ED6026C4A177F099B5770"  # required; the CREATE deployer
    count: 1500000              # required; uint64
    runtime: "0x6080..."        # required; hex bytes, planted at every derived addr
    start_nonce: 2              # optional; uint64, defaults to 0
    storage_init:               # optional; same map planted on every derived contract
      "0x0": "0xa3c1e324ca1ce40db73ed6026c4a177f099b5770"
```

Type rules inside `parameters:` mirror the ERC-20 ones — addresses and
hex-byte fields **must be quoted strings**.

The `storage_init` map deserves its own note: both slot keys and
values are treated as 32-byte hashes. Shorter hex strings are
left-padded with zero bytes (so `"0x0"` is slot 0 and `"0xa3c1…5770"`
becomes a 20-byte address left-padded to a 32-byte slot value). Two
YAML keys that decode to the same canonical slot (e.g. `"0x0"` and
`"0x00"`) are rejected as a duplicate at parse time.

Built-in non-template handlers (no `template:` field needed):

- `raw` — `kind: contract` with explicit `code:`. Whatever bytecode you
  supply, with synthesized storage filling `approximate_size_bytes`.
- `eoa` — `kind: eoa`. Plain EOA when `code:` is empty; 7702-delegating
  EOA when `code:` is `0xef0100<addr>`; storage-bloated EOA when
  `approximate_size_bytes:` is set.

## Composability with `--target-size`

- `--target-size`: an upper bound on the projected trie footprint of
  the whole generated database — spec entities AND auto-fill both count
  toward it. When set alongside `--spec`, the auto-fill emits
  mainnet-shaped synthetic state (20 % account-trie / 10 % bytecode /
  70 % storage) up to the headroom (`target_size` minus the spec's
  projected cost). If the spec alone exceeds the budget,
  `internal/specbuild` truncates the entity list to the longest prefix
  that fits, emits a `--target-size … truncated spec at entity[N]`
  warning on stderr, and no auto-fill runs. To generate a spec verbatim
  with no synthetic fill, omit `--target-size`.
- `--seed`: drives both the spec's deterministic address derivation
  AND the auto-fill RNG. Same `--seed + --spec + --target-size` always
  produces the same on-disk state on a given client.

## Determinism guarantees

Same YAML + same `--seed` produces:
- Identical entity addresses (all three modes). Pinned at unit level by
  `internal/specbuild/derive_test.go:TestResolveAddressDeterministicAcrossRuns`.
- Identical synthesized storage slot keys + values. Pinned by
  `internal/templates/sizing_test.go:TestSynthesizeSlotsDeterministic`.
- Identical end-to-end `PreAlloc` slice after parse → validate → build.
  Pinned by `internal/specbuild/build_test.go:TestBuildDeterminismEndToEnd`.

**CI coverage**: every per-client end-to-end suite — geth, besu,
nethermind, reth — drives its `Config.PreAlloc` from
`examples/full-matrix-spec-feature.yaml` via the shared helper
`internal/e2e_testing.LoadCISpecPreAlloc`. The same YAML on all four
clients produces identical state via `sizecal.NewFixed(64)`
(neutralizing per-client calibration divergence). The existing
`cross-client-genesis-root` aggregator job thus *automatically*
verifies the spec-driven invariant: same YAML + same `--seed` →
identical state root on all four MPT clients. No new CI job needed.

Every entity in the spec fixture is RPC-verified post-boot:
`CheckInjections` (`internal/e2e_testing/check_entities.go`) walks
`cfg.GenesisAccounts` for balances and `cfg.GenesisCode` for bytecode,
asserting RPC-returned values match the spec's intent.

## Examples

- `examples/spec-erc20-mixed-sizes.yaml` — three ERC-20s of different
  sizes + five 7702 EOAs.
- `examples/spec-eoa-bloat.yaml` — three EIP-7702 EOAs with bloated
  storage (2 GB / 5 GB / 10 GB target).
- `examples/full-matrix-spec-feature.yaml` — canonical CI fixture
  exercising every schema feature. Loaded by each per-client
  `TestE2ESuite` and validated by the `cross-client-genesis-root`
  aggregator.
- The six repricing-benchmark templates are exercised at tiny size by
  the full-matrix fixture above (entities 23-29) and validated by
  `internal/specbuild/build_repricing_test.go`. The production-scale
  repricing prestates (300 M-gas minimum, etc.) live in the repricing
  benchmark repo, not here.
- `examples/spec-repricing-10gb.yaml` — ~10 GB rehearsal prestate
  (full ERC-20, 1 GB bloated EOA, 1.5 M-nonce Bittrex chain) for
  end-to-end runs against execution-specs' `fill-stateful` workflow.
