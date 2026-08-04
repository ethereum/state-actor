// Package keys provides column-family byte identifiers, sentinel key byte
// slices, and BLOCKCHAIN CF prefix key constructors for the Besu Bonsai
// RocksDB database.
//
// Column family names: Besu passes segment.getId() directly as the CF
// descriptor name. Each ID is a SINGLE byte (not UTF-8 text). The default
// CF uses the literal UTF-8 string "default" as required by RocksDB.
//
// Citation: KeyValueSegmentIdentifier.java:27-77 (besu tag 26.5.0).
package keys

// Column family byte-slice identifiers.
// These are the exact bytes Besu passes to ColumnFamilyDescriptor.
// A single-byte CF name is NOT the same as the digit character "1" (0x31) —
// it is the raw byte 0x01.
var (
	// CFBlockchain is the BLOCKCHAIN column family. Stores block headers,
	// bodies, receipts, canonical hash index, and total difficulty.
	// BlobDB is ENABLED for this CF.
	// KeyValueSegmentIdentifier.java:29 — getId() returns new byte[]{1}.
	CFBlockchain = []byte{1}

	// CFAccountInfoState is the ACCOUNT_INFO_STATE column family.
	// Stores Bonsai flat account state: key=keccak256(addr), value=RLP account.
	// KeyValueSegmentIdentifier.java:37 — getId() returns new byte[]{6}.
	CFAccountInfoState = []byte{6}

	// CFCodeStorage is the CODE_STORAGE column family.
	// Stores contract bytecode: key=keccak256(code), value=bytecode (default mode).
	// KeyValueSegmentIdentifier.java:38 — getId() returns new byte[]{7}.
	CFCodeStorage = []byte{7}

	// CFAccountStorageStorage is the ACCOUNT_STORAGE_STORAGE column family.
	// Stores Bonsai flat storage: key=keccak256(addr)++keccak256(slot), value=RLP.
	// KeyValueSegmentIdentifier.java:39 — getId() returns new byte[]{8}.
	CFAccountStorageStorage = []byte{8}

	// CFTrieBranchStorage is the TRIE_BRANCH_STORAGE column family.
	// Stores Bonsai path-keyed trie nodes and sentinel values (worldRoot, etc.).
	// KeyValueSegmentIdentifier.java:40 — getId() returns new byte[]{9}.
	CFTrieBranchStorage = []byte{9}

	// CFTrieLogStorage is the TRIE_LOG_STORAGE column family.
	// Must be declared on DB open (Besu rejects open if CF in DB but not declared)
	// but receives NO writes for genesis-only generation.
	// BlobDB is ENABLED for this CF.
	// KeyValueSegmentIdentifier.java:41 — getId() returns new byte[]{10}.
	CFTrieLogStorage = []byte{10}

	// CFVariables is the VARIABLES column family.
	// Stores the chain head pointer: key="chainHeadHash", value=32B genesis hash.
	// KeyValueSegmentIdentifier.java:66 — getId() returns new byte[]{11}.
	CFVariables = []byte{11}

	// CFDefault is the mandatory default RocksDB column family.
	// RocksDB requires "default" to be declared on open; no writes needed.
	// Name is UTF-8 string "default", NOT a single-byte ID.
	CFDefault = []byte("default")

	// The CFs below receive no writes from state-actor. Besu still creates
	// all of them on a fresh mainnet Bonsai init (every segment with BONSAI
	// in its format set), so we create them empty to match a Besu-initialized
	// database layout byte for byte.

	// CFPrivateTransactions — legacy private transactions, retained for DB
	// backwards compatibility. KeyValueSegmentIdentifier.java:33.
	CFPrivateTransactions = []byte{3}

	// CFPrivateState — legacy private state, retained for DB backwards
	// compatibility. KeyValueSegmentIdentifier.java:34.
	CFPrivateState = []byte{4}

	// CFGoQuorumPrivateStorage — legacy GoQuorum private storage, retained
	// for DB backwards compatibility. KeyValueSegmentIdentifier.java:60.
	CFGoQuorumPrivateStorage = []byte{12}

	// CFBackwardSyncHeaders — backward-sync header cache.
	// KeyValueSegmentIdentifier.java:62.
	CFBackwardSyncHeaders = []byte{13}

	// CFBackwardSyncBlocks — backward-sync block cache.
	// KeyValueSegmentIdentifier.java:63.
	CFBackwardSyncBlocks = []byte{14}

	// CFBackwardSyncChain — backward-sync chain cache.
	// KeyValueSegmentIdentifier.java:64.
	CFBackwardSyncChain = []byte{15}

	// CFSnapsyncMissingAccountRange — snap-sync healing bookkeeping.
	// KeyValueSegmentIdentifier.java:65.
	CFSnapsyncMissingAccountRange = []byte{16}

	// CFSnapsyncAccountToFix — snap-sync healing bookkeeping.
	// KeyValueSegmentIdentifier.java:66.
	CFSnapsyncAccountToFix = []byte{17}

	// CFChainPrunerState stores chain-pruner progress. Besu 26.5 defaults to
	// BAL pruning, so this segment is not ignorable on a mainnet-default
	// initialization. KeyValueSegmentIdentifier.java:67.
	CFChainPrunerState = []byte{18}
)

// BonsaiCFNames returns the 17 column family names a fresh mainnet Bonsai
// Besu creates, in KeyValueSegmentIdentifier enum order. Creation order
// determines RocksDB CF IDs, so both the DB writer and any reopener must
// use this exact ordering to mirror a Besu-initialized database.
func BonsaiCFNames() []string {
	return []string{
		string(CFDefault),
		string(CFBlockchain),
		string(CFPrivateTransactions),
		string(CFPrivateState),
		string(CFAccountInfoState),
		string(CFCodeStorage),
		string(CFAccountStorageStorage),
		string(CFTrieBranchStorage),
		string(CFTrieLogStorage),
		string(CFVariables),
		string(CFGoQuorumPrivateStorage),
		string(CFBackwardSyncHeaders),
		string(CFBackwardSyncBlocks),
		string(CFBackwardSyncChain),
		string(CFSnapsyncMissingAccountRange),
		string(CFSnapsyncAccountToFix),
		string(CFChainPrunerState),
	}
}
