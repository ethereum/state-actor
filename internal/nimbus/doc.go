// Package nimbus is the pure-Go codec for nimbus-eth1's on-disk state
// database (the "Aristo" MPT plus the "Kvt" key-value store), used by
// client/nimbus to write a datadir the Nimbus execution client boots
// without recomputing any state.
//
// # Pin
//
// Byte layouts mirror status-im/nimbus-eth1 at commit 2f0ae87cd ("Storage
// trie static vids (#4797)") — the build published as
// statusim/nimbus-eth1:master-2f0ae87. That commit changed the format (static
// storage-trie vertex IDs, the AccLeaf stoHint byte, the 0x7e SavedState
// trailer), so no tagged release reads what this package writes. The golden
// fixture in testdata/ and client/nimbus/e2e_test.go carry the same pin; move
// them together. Source files of record (paths under execution_chain/db/):
// aristo/aristo_blobify.nim (records), aristo/aristo_vid.nim +
// aristo/aristo_constants.nim (vertex IDs), aristo/aristo_fetch.nim
// (retrieveStatic — what the reader assumes), storage_types.nim + fcu_db.nim
// (Kvt keys).
//
// # Aristo model
//
// One RocksDB column family (AriVtx) holds every trie vertex of every trie:
// the account trie rooted at vertex ID 1 and one storage trie per contract,
// rooted at a dynamically allocated ID (the account leaf's stoID). Rows are
// keyed by RootedVertexID{root, vid} (see AppendKey) and hold one vertex
// record (see vertex.go): Branch, ExtBranch (an MPT extension and the branch
// under it, merged into one vertex carrying the extension path as pfx),
// AccLeaf or StoLeaf. Branch and ExtBranch records optionally carry the
// 32-byte Merkle key of the node they represent; leaves never do. A stored
// key is TRUSTED by nimbus, and computeStateRoot(skipLayers=true) at boot
// returns as soon as the root vertex has one, so the writer stores a key on
// every branch whose referenced RLP is >= 32 bytes (always true for a root).
//
// # Vertex IDs
//
// A branch does not list its children: child n lives at startVid+n when bit n
// of used is set. Positions 1..8 (nibbles consumed from the root) use STATIC
// IDs computed from the path (StaticVid / StaticStartVid), so the reader can
// probe a leaf directly at StaticVid(path, level) without walking from the
// root; deeper positions and storage roots use DYNAMIC IDs from one global
// counter starting at FirstDynamicVID (VidAllocator). The admin record (empty
// key, EncodeSavedState) persists the counter's high-water mark as vTop.
//
// # Kvt
//
// Chain metadata and contract code live in the KvtGen column family under the
// keys in kvt.go. Nothing in this package depends on RocksDB; client/nimbus
// supplies the Sink that turns (RootedVertexID, record) into a row.
//
// # Dependency on internal/ethrex
//
// MPT node RLP (EncodeLeaf / EncodeExtension / EncodeBranch), the account
// value RLP and the nibble helpers are consensus-defined and identical for
// every MPT client, so they are imported from internal/ethrex rather than
// duplicated. Nothing ethrex-specific (its path-keyed rows, flat-KV layer,
// column families) is used here.
package nimbus
