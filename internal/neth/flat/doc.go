// Package flat mirrors the byte-exact on-disk layout of Nethermind's
// "Flat DB" state backend, so state-actor's Nethermind writer can populate a
// flat-state RocksDB directly (bypassing Nethermind's own genesis/import
// path) that Nethermind detects and serves as its `flat` backend.
//
// # Scope
//
// The flat layout is one RocksDB *column* database at <BaseDbPath>/flat with
// seven column families (plus RocksDB's mandatory, unused "default"):
//
//	Metadata       — format markers (CurrentState / Layout / SlotEncoding)
//	Account        — flat account rows: key keccak(addr)[0:20], value slim RLP
//	Storage        — flat storage rows: 52-byte key, value RLP(trimmed slot)
//	StateNodes     — state-trie nodes, path length 6..15  (8-byte key)
//	StateTopNodes  — state-trie nodes, path length 0..5   (3-byte key)
//	StorageNodes   — storage-trie nodes, path length 0..15 (28-byte key)
//	FallbackNodes  — state/storage-trie nodes, path length 16..64 (34/54-byte key)
//
// In flat mode the Merkle-Patricia trie is NOT eliminated — it is relocated
// out of the legacy `state` DB and into the four node column families above.
// A flat writer therefore emits BOTH the trie nodes (from the same StackTrie
// pass that computes the state root) AND the flat Account/Storage leaf rows,
// then stamps the Metadata markers.
//
// This package is dependency-free at the runtime layer (pure byte encoders,
// no I/O, no cgo, no database handles), so every key/value form is unit
// testable against literal fixtures.
//
// # Pin
//
// All byte layouts mirror Nethermind release tag 1.39.0
// (commit 14aca2c5202c48a6a46c8d928fe7ed8a898d253e). The Docker image booted
// by the e2e / oracle / smoke / bench harnesses MUST match
// neth.PinnedNethermindVersion. The flat layout changed across 1.37→1.38→1.39
// (Layout marker added in 1.38.0, RLP-wrapped slot values + SlotEncoding
// marker in 1.39.0); a DB emitted by this package is 1.39.0-or-newer only.
//
// Source anchors (paths under src/Nethermind at tag 1.39.0):
//
//	Nethermind.State.Flat/FlatDbColumns.cs                       — CF set
//	Nethermind.State.Flat/Persistence/BasePersistence.cs         — markers
//	Nethermind.State.Flat/Persistence/BaseFlatPersistence.cs     — Account/Storage keys+values
//	Nethermind.State.Flat/Persistence/BaseTriePersistence.cs     — node keys + CF routing
//	Nethermind.Init/FlatStateActivationPolicy.cs                 — backend detection
package flat
