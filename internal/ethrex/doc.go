// Package ethrex implements the trie codec primitives used by ethrex
// (pinned release v15.0.0, lambdaclass/ethrex). The golden fixture is
// byte-identical from v13.0.0 through v15.0.0.
//
// # Two-rows-per-leaf model
//
// Each leaf in both the account trie and storage trie is stored as two rows:
//
//  1. (pathToNode, EncodeLeaf(remainingPath, value)) — the node RLP row,
//     keyed by the path from the root to the branch/leaf node.
//  2. (pathToNode ++ remainingNibbles ++ [16], value) — the full leaf-path
//     row (65 nibbles for a 64-nibble account keyspace, trailing 16 = leaf flag),
//     whose value is the raw leaf value without any RLP wrapping.
//
// Branch and extension nodes produce only one row each (the node RLP row).
//
// # Raw-nibble path keys
//
// Both account_trie_nodes and storage_trie_nodes are keyed by the trie node
// path as raw nibbles — one nibble per byte, values 0x00–0x0f. There is no
// hex-packing or nibble compaction in the DB keys.
//
// # Storage prefix
//
// Storage trie keys are prefixed by the address owner's trie path:
// 64 address nibbles ++ [0x10] ++ [0x11] ++ storage node path.
// The Builder itself emits raw (unprefixed) paths; the caller prepends the
// 66-nibble storage prefix via a sink wrapper.
//
// # Import constraints
//
// go-ethereum/trie is forbidden in production .go files in this package.
// Allowed production imports: go-ethereum/common, go-ethereum/crypto,
// go-ethereum/rlp, holiman/uint256.
package ethrex
