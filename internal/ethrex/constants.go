package ethrex

// Column-family names for ethrex's single RocksDB instance.
// Sourced from ethrex crates/storage/api/tables.rs at v23.0.0.
const (
	CFChainData            = "chain_data"
	CFAccountCodes         = "account_codes"
	CFAccountCodeMetadata  = "account_code_metadata"
	CFBodies               = "bodies"
	CFBlockNumbers         = "block_numbers"
	CFCanonicalBlockHashes = "canonical_block_hashes"
	CFHeaders              = "headers"
	CFPendingBlocks        = "pending_blocks"
	CFTransactionLocations = "transaction_locations"
	CFReceiptsV2           = "receipts_v2"
	CFSnapState            = "snap_state"
	CFInvalidAncestors     = "invalid_ancestors"
	CFAccountTrieNodes     = "account_trie_nodes"
	CFStorageTrieNodes     = "storage_trie_nodes"
	CFFullsyncHeaders      = "fullsync_headers"
	CFAccountFlatKeyValue  = "account_flatkeyvalue"
	CFStorageFlatKeyValue  = "storage_flatkeyvalue"
	CFMiscValues           = "misc_values"
	CFExecutionWitnesses   = "execution_witnesses"
	CFBlockAccessLists     = "block_access_lists"
	CFBadBlocks            = "bad_blocks"
)

// Tables is the ordered list of all 21 ethrex column families.
// Order matches ethrex's TABLES array for deterministic CF creation.
//
// state-actor writes rows into a subset of these; the rest are created empty so
// the on-disk CF set matches what ethrex itself would produce. ethrex drops any
// column family it finds that is absent from its own TABLES (drop_obsolete_cfs),
// so this list must not carry entries from an ethrex version newer than the
// boot pin in client/ethrex/e2e_test.go.
var Tables = []string{
	CFChainData,
	CFAccountCodes,
	CFAccountCodeMetadata,
	CFBodies,
	CFBlockNumbers,
	CFCanonicalBlockHashes,
	CFHeaders,
	CFPendingBlocks,
	CFTransactionLocations,
	CFReceiptsV2,
	CFSnapState,
	CFInvalidAncestors,
	CFAccountTrieNodes,
	CFStorageTrieNodes,
	CFFullsyncHeaders,
	CFAccountFlatKeyValue,
	CFStorageFlatKeyValue,
	CFMiscValues,
	CFExecutionWitnesses,
	CFBlockAccessLists,
	CFBadBlocks,
}

// chain_data index keys (RLP of the index byte).
// ChainConfig index = 0; RLP(0) = 0x80 (RLP empty string, not 0x00).
// EarliestBlockNumber index = 1; RLP(1) = 0x01.
// LatestBlockNumber index = 4; RLP(4) = 0x04.
var (
	ChainDataKeyChainConfig         = []byte{0x80}
	ChainDataKeyEarliestBlockNumber = []byte{0x01}
	ChainDataKeyLatestBlockNumber   = []byte{0x04}
)

// EmptyCodeHash is keccak256(nil) — the code hash for accounts with no code.
// ethrex writes an entry in account_codes + account_code_metadata for every
// account including empty-code EOAs.
const EmptyCodeHashHex = "0xc5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470"

// EmptyTrieHashHex is the Keccak256 of the RLP-encoded empty string (0x80),
// i.e. the root of an empty MPT — used as the default storageRoot.
const EmptyTrieHashHex = "0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421"

// StoreSchemaVersion is the value written to metadata.json. It must equal
// ethrex's STORE_SCHEMA_VERSION (crates/storage/lib.rs): a lower value sends
// every boot through Store::new_with_config's migration branch, which runs the
// pending migrations over the whole DB before serving a single request.
const StoreSchemaVersion = 3

// misc_values keys/values controlling ethrex's flat-key-value (FKV) generator.
// On boot the generator reads misc_values["last_written"]: empty = "not
// generated" (clear + rebuild the FKV CFs from the trie); 0xff = "already
// generated, skip". state-actor pre-populates the FKV CFs and writes
// FKVLastWrittenComplete so the generator skips the rebuild.
// Source: ethrex store.rs flatkeyvalue_generator + Store::from_backend.
var (
	MiscValuesLastWrittenKey = []byte("last_written")
	FKVLastWrittenComplete   = []byte{0xff}
)
