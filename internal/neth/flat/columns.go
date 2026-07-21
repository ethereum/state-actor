package flat

// Column identifies a column family in the flat-state RocksDB. Its integer
// value is the index of the CF handle in the slice returned by
// grocksdb.OpenDbColumnFamilies when the DB is opened with ColumnNames, so a
// writer can index handles[col] directly.
type Column int

const (
	ColDefault       Column = iota // RocksDB mandatory CF, unused by the flat layout
	ColMetadata                    // format markers
	ColAccount                     // flat account rows
	ColStorage                     // flat storage rows
	ColStateNodes                  // state-trie nodes, path length 6..15
	ColStateTopNodes               // state-trie nodes, path length 0..5
	ColStorageNodes                // storage-trie nodes, path length 0..15
	ColFallbackNodes               // state/storage-trie nodes, path length 16..64
)

// ColumnNames is the ordered CF-name list to pass to
// grocksdb.OpenDbColumnFamilies. Index i is named ColumnNames[i] and maps to
// the Column constant of the same ordinal. The seven named CFs match the
// member names of Nethermind.State.Flat/FlatDbColumns.cs (CF names come from
// the enum's ToString()); "default" is RocksDB's mandatory CF, which must
// exist even though the flat layout never writes to it.
//
// Only the CF *names* are dictated by Nethermind (that is how it re-opens the
// DB by name); the ordinal ordering here is state-actor's own and only needs
// ColumnNames[Col] == the CF name.
var ColumnNames = []string{
	"default",       // ColDefault
	"Metadata",      // ColMetadata
	"Account",       // ColAccount
	"Storage",       // ColStorage
	"StateNodes",    // ColStateNodes
	"StateTopNodes", // ColStateTopNodes
	"StorageNodes",  // ColStorageNodes
	"FallbackNodes", // ColFallbackNodes
}
