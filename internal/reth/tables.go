package reth

// TableSpec describes one MDBX named DB: its reth table name and whether the
// table uses DupSort semantics. Names match crates/storage/db-api/src/tables/mod.rs
// at PinnedRethCommit exactly — drift = silent boot failure.
type TableSpec struct {
	Name    string
	DupSort bool
}

// Tables is the canonical registry of every MDBX named DB reth declares.
// Order is the same as the tables! macro in mod.rs:308-540.
var Tables = []TableSpec{
	{Name: "CanonicalHeaders"},
	{Name: "HeaderTerminalDifficulties"},
	{Name: "HeaderNumbers"},
	{Name: "Headers"},
	{Name: "BlockBodyIndices"},
	{Name: "BlockOmmers"},
	{Name: "BlockWithdrawals"},
	// The two block access list (EIP-7928) tables come from reth v2.7.0
	// (PinnedRethRelease), not from PinnedRethCommit. state-actor writes
	// no rows in them. A reth node creates missing tables when it opens
	// the DB read-write, but read-only tools (`reth db stats`) open every
	// table in Tables::ALL and fail with "no matching key/data pair found"
	// if one is missing.
	{Name: "BlockAccessLists"},
	{Name: "BlockAccessListBlockNumbers"},
	{Name: "Transactions"},
	{Name: "TransactionHashNumbers"},
	{Name: "TransactionBlocks"},
	{Name: "Receipts"},
	{Name: "Bytecodes"},
	{Name: "PlainAccountState"},
	{Name: "PlainStorageState", DupSort: true},
	{Name: "AccountsHistory"},
	{Name: "StoragesHistory"},
	{Name: "AccountChangeSets", DupSort: true},
	{Name: "StorageChangeSets", DupSort: true},
	{Name: "HashedAccounts"},
	{Name: "HashedStorages", DupSort: true},
	{Name: "AccountsTrie"},
	{Name: "StoragesTrie", DupSort: true},
	{Name: "TransactionSenders"},
	{Name: "StageCheckpoints"},
	{Name: "StageCheckpointProgresses"},
	{Name: "PruneCheckpoints"},
	{Name: "VersionHistory"},
	{Name: "ChainState"},
	{Name: "Metadata"},
}
