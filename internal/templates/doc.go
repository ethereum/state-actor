// Package templates hosts the registry of named state-actor spec templates
// (`erc20`, etc.) and exports the PreAllocEntity record type that every
// writer consumes.
//
// The package is structured around a single Template interface and a
// process-level registry populated via init() functions. Adding a new
// template is one new file in this package: implement Template, call
// Register(yourTemplate) in init().
//
// What lives here:
//
//   - template.go         — the Template interface, Context, and PreAllocEntity.
//   - registry.go         — Register / Lookup / Names / UserVisibleNames.
//   - params.go           — neutral parameter parsers shared across templates.
//   - raw.go              — kind: contract, code: ... (no template field).
//   - eoa.go              — kind: eoa (with optional 7702 code, storage bloat).
//   - erc20.go            — kind: contract, template: erc20.
//   - sequential_eoas.go  — kind: contract, template: sequential_eoas. One
//     entity expands to N plain EOAs at sequential addresses; backs
//     SequentialAddressLayout in execution-specs bloatnet benchmarks.
//   - sequential_pkey_eoas.go — kind: contract, template:
//     sequential_pkey_eoas. N pre-funded EOAs at addresses derived from
//     sequential private keys; backs the EEST sender-pool layout.
//   - storage_pattern.go  — kind: contract, template: storage_pattern. Plants
//     slot 0 = final+1 plus slot k = k for k in 1..final; backs the
//     existing_slots=True path of test_sload_bloated / test_sstore_bloated.
//   - create2_factory.go  — kind: contract, template: create2_factory. Plants
//     the canonical Arachnid deterministic-deployment proxy runtime.
//   - create2_deploys.go  — kind: contract, template: create2_deploys. Derives
//     N CREATE2 addresses (factory + initcode + salt range) and plants
//     the runtime (literal `runtime:` or `code_pattern:`-generated) at each.
//   - create_preimage_deploys.go — kind: contract, template:
//     create_preimage_deploys. Derives N CREATE addresses
//     (keccak256(rlp([sender, nonce]))[12:]) and plants `runtime` (or
//     `code_pattern:`-generated code) at each; backs CreatePreimageLayout
//     (Bittrex-style chains).
//   - code_pattern.go     — named per-derived-address runtime generators
//     (`unique_jumpdest_pre_amsterdam`) shared by the two deploy templates.
//   - sizing.go           — shared streaming storage-slot synthesizer.
//
// What does NOT live here:
//   - The YAML schema and parser (internal/spec/).
//   - The Spec→entities translator (internal/specbuild/).
//   - Per-client byte-budget calibration (internal/sizecal/).
//
// Determinism contract: every Template.Expand call must be byte-identical
// across runs for the same input. Address derivation, slot synthesis, and
// storage values are all pure functions of the spec entity + the seed.
package templates
