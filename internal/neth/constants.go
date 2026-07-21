// Package neth and its sub-packages mirror the byte-exact subset of
// Nethermind's wire formats that state-actor's Nethermind writer emits.
//
// Sub-packages:
//   - neth/rlp  — RLP encoders for header, block, account, BlockInfo,
//     ChainLevelInfo, receipts.
//   - neth/trie — trie Builder that wraps go-ethereum's StackTrie and routes
//     its node output to a layout sink.
//   - neth/flat — flat-state column-DB key/marker encoders (the layout the
//     Nethermind writer emits and Nethermind ≥ 1.39.0 serves).
//
// The package is **dependency-free at the runtime layer**: no I/O, no
// goroutines, no database handles. That makes encoders unit-testable in
// isolation against literal byte fixtures, which is the load-bearing
// strategy for catching wire-format drift before it reaches a Nethermind
// boot test.
//
// Citations in this package and neth/flat point at Nethermind release tag
// 1.39.0 (commit 14aca2c5202c48a6a46c8d928fe7ed8a898d253e). Bumping
// PinnedNethermindVersion means diffing the mirrored upstream files against the
// new tag — the flat key/marker/CF encoders (neth/flat), the block-tree +
// account RLP (neth/rlp), and the flat detection policy/log strings — then
// re-running the differential oracle and flat e2e against the new image.
package neth

import "github.com/ethereum/go-ethereum/common"

// PinnedNethermindVersion is the Nethermind release whose on-disk byte formats
// this package (and neth/flat) are verified against. The Docker image
// nethermind/nethermind:<PinnedNethermindVersion> booted by every e2e /
// oracle / smoke / bench harness MUST match this string. A consistency test
// (neth/flat) asserts the harness pin sites agree with it.
const PinnedNethermindVersion = "1.39.0"

// EmptyTreeHash is keccak256(RLP_empty_string) = keccak256([0x80]).
//
// Nethermind defines this in src/Nethermind/Nethermind.Core/Crypto/Keccak.cs
// as `EmptyTreeHash = InternalCompute([128])`. State trie roots for empty
// allocs MUST equal this constant — Nethermind short-circuits reads at this
// hash without dereferencing the State DB.
var EmptyTreeHash = common.HexToHash("0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421")

// OfAnEmptyString is keccak256(empty bytes) = keccak256([]).
//
// This is the codeHash field of an account that has no code. Nethermind
// uses it as the "no code" sentinel.
var OfAnEmptyString = common.HexToHash("0xc5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470")

// OfAnEmptySequenceRlp is keccak256(RLP_empty_list) = keccak256([0xc0]).
//
// This is the txRoot / receiptsRoot field of a block with no transactions
// or receipts (the genesis block, by definition).
var OfAnEmptySequenceRlp = common.HexToHash("0x1dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347")
