package nimbus

import "github.com/ethereum/go-ethereum/core/types"

// VertexID is Aristo's vertex identifier (aristo_desc/desc_identifiers.nim).
type VertexID uint64

const (
	// FirstStaticVID is the lowest vertex ID; it is also the account-trie root.
	FirstStaticVID VertexID = 1
	// StateRootVID is the root vertex of the account trie: row (1,1).
	StateRootVID VertexID = FirstStaticVID
	// StaticVIDLevels is how many trie levels below the root get static IDs.
	StaticVIDLevels = 8
	// FirstDynamicVID is the first ID handed out by the dynamic allocator:
	// 1 + sum(16^i, i=0..8) — one past the last static ID.
	FirstDynamicVID VertexID = 0x1_1111_1112

	// MaxVertexBlobSize bounds a vertex record: nimbus reads rows into a fixed
	// 118-byte buffer and fails on anything larger (rdb_get.nim).
	MaxVertexBlobSize = 118
	// MaxStoHint is the largest AccLeaf stoHint nimbus accepts
	// (STATIC_VID_LEVELS + 1; deblobifyLeaf rejects larger values).
	MaxStoHint byte = StaticVIDLevels + 1

	// SavedStateMagic trails the admin record; nimbus also accepts the legacy
	// SavedStateLegacyMagic on read but always writes SavedStateMagic.
	SavedStateMagic       byte = 0x7e
	SavedStateLegacyMagic byte = 0x7f
	// SavedStateLen is the admin record length: vTop(8) + serial(8) + magic(1).
	SavedStateLen = 17
)

// Column-family names of nimbus's single RocksDB instance and the datadir
// layout around it (core_db/backend/rocksdb_desc.nim, aristo/.../rdb_desc.nim,
// kvt/.../rdb_desc.nim).
const (
	CFDefault = "default"
	CFAriVtx  = "AriVtx"  // every trie vertex + the admin record
	CFKvtGen  = "KvtGen"  // chain metadata + contract code
	CFKvtSync = "KvtSync" // syncer scratch; created empty

	// DBFolder is the RocksDB directory under the nimbus --data-dir.
	DBFolder = "ecdb"
	// GenesisFileName is the sidecar client/nimbus writes next to DBFolder for
	// `nimbus executionClient --network=<path>`.
	GenesisFileName = "nimbus-genesis.json"
)

// ColumnFamilies is the full CF set the writer creates, "default" first.
var ColumnFamilies = []string{CFDefault, CFAriVtx, CFKvtGen, CFKvtSync}

// StaticOffset[d] is the first static vertex ID of trie level d
// (1 + sum(16^i, i<d)); StaticOffset[9] == FirstDynamicVID.
var StaticOffset = [StaticVIDLevels + 2]uint64{
	1, 2, 18, 274, 4370, 69906, 1118482, 17895698, 286331154, 4581298450,
}

// EmptyCodeHash and EmptyRootHash are the usual keccak sentinels; an AccLeaf
// omits the code hash when it equals EmptyCodeHash.
var (
	EmptyCodeHash = types.EmptyCodeHash
	EmptyRootHash = types.EmptyRootHash
)
