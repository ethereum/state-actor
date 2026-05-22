// Package autofill translates a target on-disk byte budget into a
// deterministic recipe for emitting synthetic Ethereum-shaped state in
// mainnet proportions. It replaces the older --accounts / --contracts
// CLI flags that silently collided with --spec entities.
//
// The split is 20 / 10 / 70 across account-trie / bytecode / contract
// storage (constants in internal/sizecal). The split applies to the
// top-up portion only — target_size minus the projected cost of any
// loaded --spec entities. With no spec, top-up = target_size.
//
// All entity emission goes through Plan.DrawEOA and Plan.DrawContract,
// which compose the canonical entitygen primitives. The contract draw
// order is fixed across all 5 client emission sites (geth-MPT,
// geth-bintrie, reth, besu, nethermind) so a single (spec, target_size,
// seed) tuple yields byte-identical state roots cross-client — the
// load-bearing invariant of the project.
package autofill
