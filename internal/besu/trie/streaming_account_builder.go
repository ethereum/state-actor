package trie

import "github.com/ethereum/go-ethereum/common"

// StreamingAccountBuilder builds the Bonsai account-state trie in O(trie depth)
// memory, the account-trie counterpart to StreamingStorageBuilder. It is a thin
// wrapper over the shared streamingBuilder engine with an account emitter
// (un-prefixed node keys via PutAccountStateTrieNode).
//
// It replaces the non-streaming Builder for bulk generation: Builder keeps the
// entire account MPT resident until Commit (O(accounts) memory), which OOMs on
// large datadirs; this builder emits nodes as the right-spine collapses, so peak
// memory is O(depth) regardless of account count — matching how geth/reth/neth
// build their account trie via StackTrie / HashBuilder.
//
// Input contract: AddAccount/AddLeaf calls MUST arrive in strictly ascending
// keccak256(address) (addrHash) order; the Phase 2 sorted-Pebble iteration
// provides this.
type StreamingAccountBuilder struct {
	c streamingBuilder
}

// NewStreamingAccountBuilder constructs a streaming account-state trie builder
// that emits trie nodes through the caller-supplied sink.
func NewStreamingAccountBuilder(sink NodeSink) *StreamingAccountBuilder {
	return &StreamingAccountBuilder{c: streamingBuilder{sink: sink, em: accountTrieEmitter{}}}
}

// AddAccount inserts (addrHash → accountRLP) into the account-state trie.
// addrHash must be keccak256(address); accountRLP is the flat account RLP.
// addrHash MUST be strictly greater than the previous call's; out-of-order input
// returns ErrSlotsOutOfOrder.
func (ab *StreamingAccountBuilder) AddAccount(addrHash common.Hash, accountRLP []byte) error {
	return ab.c.addLeaf(addrHash, accountRLP)
}

// AddLeaf is an alias for AddAccount so *StreamingAccountBuilder satisfies
// streamingtrie.HashBuilder.
func (ab *StreamingAccountBuilder) AddLeaf(keyHash common.Hash, valueRLP []byte) error {
	return ab.c.addLeaf(keyHash, valueRLP)
}

// Commit finalises the account trie and returns (rootHash, rootRLP). The caller
// passes rootRLP to NodeSink.SaveWorldState. An empty trie returns
// (EmptyTrieNodeHash, [0x80]) with zero sink calls.
func (ab *StreamingAccountBuilder) Commit() (common.Hash, []byte, error) {
	return ab.c.finalize()
}

// Root finalises the account trie and returns just the root hash. Implements
// the streamingtrie.HashBuilder contract (alongside AddLeaf).
func (ab *StreamingAccountBuilder) Root() (common.Hash, error) {
	h, _, err := ab.c.finalize()
	return h, err
}
