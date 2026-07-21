// Package rlp wraps go-ethereum's RLP encoder for the subset of types
// state-actor's Nethermind writer emits. It exists so callers (trie builder,
// orchestration, flat writer) interact with a single state-actor-owned
// surface — the encoders here are byte-equivalent to Nethermind's because
// both consume the same standard Ethereum RLP specification.
//
// Citations point at Nethermind release tag 1.39.0
// (commit 14aca2c5202c48a6a46c8d928fe7ed8a898d253e):
//
//   - AccountDecoder: src/Nethermind/Nethermind.Serialization.Rlp/AccountDecoder.cs
//     (full form = the non-slim branch; slim form = AccountDecoder.Slim)
package rlp

import (
	"github.com/ethereum/go-ethereum/core/types"
	gethrlp "github.com/ethereum/go-ethereum/rlp"
)

// EncodeAccount returns the RLP-encoded bytes of an account in Nethermind's
// "full" format: [nonce, balance, storageRoot, codeHash]. This is the form
// stored as the account leaf value inside the state trie, whose nodes are
// persisted in the flat DB's node CFs.
//
// Mirrors AccountDecoder.cs:Encode (the non-slim branch). The byte output
// must match what types.StateAccount serializes via go-ethereum's RLP — both
// follow the standard Ethereum spec. Tests pin the empty-account bytes
// byte-for-byte to surface any drift.
func EncodeAccount(acc *types.StateAccount) ([]byte, error) {
	return gethrlp.EncodeToBytes(acc)
}

// EncodeAccountSlim returns the RLP-encoded bytes of an account in
// Nethermind's "slim" format — the form stored in the flat Account column
// family. It is the 4-item list [nonce, balance, storageRoot?, codeHash?]
// where the storage root is the empty byte string (0x80) when it equals the
// empty-trie root and the code hash is the empty byte string (0x80) when it
// equals the empty-code hash; otherwise the full 32-byte hashes are kept.
//
// This is exactly go-ethereum's types.SlimAccountRLP, which applies the same
// empty-root / empty-codehash substitution Nethermind's AccountDecoder.Slim
// does. Tests pin the byte output against the verified contract.
func EncodeAccountSlim(acc *types.StateAccount) []byte {
	return types.SlimAccountRLP(*acc)
}
