# ethrex integration — Phase 0 spike findings

Empirically captured from a real ethrex genesis store at pinned commit
`318ec2888944ad38fee66c2ae8b9dccbd8df939f` (lambdaclass/ethrex). The harness
(`testdata/gen/sa_dump.rs`) builds a store via the real
`Store::add_initial_state` path and dumps every column family; the golden dump
is `testdata/genesis_dump.json`. Input genesis: `testdata/gen/genesis.json`
(chainId 1337; one EOA, one storage contract forcing a storage-trie branch, one
JUMPDEST/PUSH contract).

Genesis result: `state_root = 0x7acb3fa2fa378c411135fef796a61c4c681c14e9c844e4af1d779a6f6a7006fb`,
`block_hash = 0xb9351fb93aa55de48457a067ecdf1d4d25ef2c2204ed7cbe6f72ab009db07b63`.

## CF population at genesis (20 tables)

Written: `account_trie_nodes` (9), `storage_trie_nodes` (7), `account_codes` (3),
`account_code_metadata` (3), `chain_data` (3), `headers` (1), `bodies` (1),
`block_numbers` (1), `canonical_block_hashes` (1). Everything else EMPTY —
including `account_flatkeyvalue`, `storage_flatkeyvalue`, `misc_values`.

## Trie node tables — key = raw nibbles (one nibble per byte)

Both `account_trie_nodes` and `storage_trie_nodes` are keyed by the node PATH as
raw nibbles (NOT hex-packed, NOT node hash). Confirmed against
`store.rs:setup_genesis_state_trie` + `layering.rs:apply_prefix`.

**Two rows per leaf (load-bearing):** each leaf emits BOTH:
1. `(path_to_node, node_rlp)` — node_rlp = `RLP([compact(remaining_path), value])`.
2. `(path_to_node ++ remaining_nibbles ++ [0x10], raw_value)` — the full leaf
   path (65 nibbles for accounts, 131 for storage; trailing `0x10` = leaf flag),
   value = the raw leaf value WITHOUT the node-RLP wrapper.

So an account leaf produces a 65-nibble key whose value is the bare
`AccountState` RLP; a storage leaf produces a 131-nibble key whose value is the
bare storage-value RLP. Branch/extension nodes produce only row (1).

- Account node key = `nibbles(keccak(addr))`-derived path, no prefix.
- Storage node key = `applyPrefix` = `nibbles(keccak(addr)) ++ [0x10] ++ [0x11] ++ path`.
  i.e. 64 address nibbles, then leaf-flag nibble `16`, then separator nibble
  `17`, then the storage node path. The storage-trie ROOT key is therefore 66
  nibbles (64 + 16 + 17). (layering.rs:300-307; `fromBytes` appends 16, then
  `append_new(17)`.)
- Account-trie ROOT key = empty (`0x`).
- Empty-trie sentinel `([] -> 0x80)` only fires when a trie is completely empty
  (no leaves). Accounts with no storage emit ZERO storage rows and just set
  `storage_root = EMPTY_TRIE_HASH` in their AccountState.

Account leaf value = `RLP([nonce, balance, storage_root, code_hash])` (full, not
slim). Storage leaf value = minimal-big-endian RLP of the nonzero U256 (zero
slots skipped).

## account_codes — key = RAW 32-byte code hash (NOT RLP)

Value = `encode_code` = `RLP(bytecode) || RLP(jump_targets: Vec<u32>)`
concatenated. Confirmed:
- code `0x60015b00` -> `0x84·60015b00` + `0xc1·02` (jump_targets `[2]`: JUMPDEST
  at offset 2; the PUSH1 at offset 0 skips its 1-byte immediate).
- code `0x600160015500` -> `0x86·600160015500` + `0xc0` (empty jump_targets).
- empty code -> `0x80` + `0xc0`.

`jump_targets` scan: `0x5B` -> push offset; `0x60..=0x7F` (PUSH1..PUSH32) ->
`i += opcode - 0x5F`; else `i += 1` (account.rs:58-79).

An entry is written for EVERY account, including empty-code EOAs (the empty-code
hash `0xc5d2…a470` is present).

## account_code_metadata — key = RAW 32-byte code hash

Value = `u64::to_be_bytes(len(bytecode))` (8 bytes big-endian). Empty code -> 0.

## chain_data — key = RLP(index as u8)

**ChainConfig (index 0) key = `0x80`** (RLP of integer 0 is the empty string,
NOT `0x00`). EarliestBlockNumber (1) key = `0x01`; LatestBlockNumber (4) key =
`0x04`. Block-number values = `u64::to_le_bytes` (LE!).

ChainConfig value = `serde_json::to_string(chain_config)` — ethrex's OWN
serialization (camelCase), which fills in fields the input omitted (osaka/bpo
blob schedules, `enableVerkleAtGenesis:false`, all unset forks as `null`).
NOTE: `set_chain_config` runs at the TOP of `add_initial_state` (store.rs:2207),
BEFORE the genesis-hash short-circuit, so ethrex REWRITES this value from
`--network <genesis.json>` on every boot. state-actor should still write a valid
ChainConfig JSON for self-consistency, but byte-exactness here is not boot-
critical.

## Block metadata

- `headers`: key = `RLP(block_hash)` = `0xa0 || 32B` (33 bytes); value =
  ethrex `BlockHeaderRLP`. Decoded fields (Prague set): parentHash, ommersHash
  (=EmptyUncleHash), coinbase, stateRoot, txRoot/receiptsRoot (=EmptyTrieHash),
  logsBloom(256B 0), difficulty=0, number=0, gasLimit, gasUsed=0, timestamp,
  extraData, mixHash=0, nonce(8B 0), **baseFeePerGas = 0x3b9aca00 (1 gwei
  default)**, withdrawalsRoot(=EmptyTrieHash), blobGasUsed=0, excessBlobGas=0,
  parentBeaconBlockRoot=0, requestsHash(=sha256(empty)=EmptyRequestsHash).
- `bodies`: key = `RLP(block_hash)`; empty genesis body value = `0xc3c0c0c0`
  (RLP list of three empty lists: transactions[], ommers[], withdrawals[]).
- `block_numbers`: key = `RLP(block_hash)`; value = `u64::to_le_bytes(0)`.
- `canonical_block_hashes`: key = `u64::to_le_bytes(0)` (raw 8B); value =
  `RLP(block_hash)`. This is the boot gate read by `load_block_header(0)`.

## metadata.json — MUST be written by state-actor (corrects reviewer CRITICAL-3)

`Store::new` (store.rs:1523-1534): if the datadir is non-empty but has NO
`metadata.json`, it returns `NotFoundDBVersion` and REFUSES to boot. ethrex only
auto-creates `metadata.json` for an EMPTY dir. Since state-actor fills the dir
with SST files, **state-actor MUST write `datadir/metadata.json` containing
`{"schema_version": 2}`** (STORE_SCHEMA_VERSION = 2). `read_store_schema_version`
just `serde_json::from_str`s it, so any valid JSON with `schema_version: 2`
works. This is the OPPOSITE of "do not write metadata.json".

## Boot path

ethrex always calls `add_initial_state(--network genesis)`. It compares the
stored `load_block_header(0)` hash against `genesis.get_block().hash()`.

CORRECTION (verified against v15.0.0, supersedes the original spike claim):
`genesis.get_block()` sets `state_root: self.compute_state_root()`, which builds
the trie from the sidecar `alloc` on EVERY boot. With state-actor's empty alloc
that yields `EMPTY_TRIE_HASH`, which does NOT match the real stored root, so the
match falls to the `Some(_)` arm and returns `IncompatibleChainConfig` — ethrex
refuses to boot. There is no "no trie recompute" short-circuit; the recompute is
unconditional.

state-actor therefore boots ethrex with `--skip-genesis-validation`
(lambdaclass/ethrex#6783), which makes ethrex trust the stored root instead of
recomputing it. Boot requirements: a stored genesis header + canonical row +
schema metadata file, AND the skip-validation flag (or a flag-bearing image).

## Open questions still deferred to e2e (Phase 4)

- Exact ethrex boot subcommand + flags (datadir, `--network`, engine-API
  JWT/authrpc) and whether a CL handshake is needed to advance past block 0.
- The prebuilt `/home/edgar/dev/ethrex/target/release/ethrex` is a STALE commit
  (`097bab04`); e2e must build/boot ethrex at the pinned commit.
