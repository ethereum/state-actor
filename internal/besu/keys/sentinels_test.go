package keys

import "testing"

// TestSentinelKeyLengths pins the byte lengths of all sentinel keys.
// These lengths match the Java source strings exactly. A length mismatch
// means the key was mis-spelled and would never be found by Besu.
func TestSentinelKeyLengths(t *testing.T) {
	cases := []struct {
		name string
		key  []byte
		want int
	}{
		// TRIE_BRANCH_STORAGE sentinels
		{"WorldRootKey", WorldRootKey, 9},
		{"WorldBlockHashKey", WorldBlockHashKey, 14},
		{"WorldBlockNumberKey", WorldBlockNumberKey, 16},
		{"FlatDbStatusKey", FlatDbStatusKey, 12},
		// VARIABLES sentinel
		{"ChainHeadHashKey", ChainHeadHashKey, 13},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if len(c.key) != c.want {
				t.Fatalf("%s: len=%d, want %d (bytes: %x)",
					c.name, len(c.key), c.want, c.key)
			}
		})
	}
}

// TestCFByteIDs pins the single-byte CF identifiers.
// CF names are raw bytes 1, 3..4, 6..18 — NOT ASCII characters like '1'.
func TestCFByteIDs(t *testing.T) {
	cases := []struct {
		name string
		cf   []byte
		want byte
	}{
		{"CFBlockchain", CFBlockchain, 0x01},
		{"CFPrivateTransactions", CFPrivateTransactions, 0x03},
		{"CFPrivateState", CFPrivateState, 0x04},
		{"CFAccountInfoState", CFAccountInfoState, 0x06},
		{"CFCodeStorage", CFCodeStorage, 0x07},
		{"CFAccountStorageStorage", CFAccountStorageStorage, 0x08},
		{"CFTrieBranchStorage", CFTrieBranchStorage, 0x09},
		{"CFTrieLogStorage", CFTrieLogStorage, 0x0a},
		{"CFVariables", CFVariables, 0x0b},
		{"CFGoQuorumPrivateStorage", CFGoQuorumPrivateStorage, 0x0c},
		{"CFBackwardSyncHeaders", CFBackwardSyncHeaders, 0x0d},
		{"CFBackwardSyncBlocks", CFBackwardSyncBlocks, 0x0e},
		{"CFBackwardSyncChain", CFBackwardSyncChain, 0x0f},
		{"CFSnapsyncMissingAccountRange", CFSnapsyncMissingAccountRange, 0x10},
		{"CFSnapsyncAccountToFix", CFSnapsyncAccountToFix, 0x11},
		{"CFChainPrunerState", CFChainPrunerState, 0x12},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if len(c.cf) != 1 {
				t.Fatalf("%s: len=%d, want 1 (bytes: %x)", c.name, len(c.cf), c.cf)
			}
			if c.cf[0] != c.want {
				t.Fatalf("%s: byte=%#x, want %#x", c.name, c.cf[0], c.want)
			}
		})
	}
}

// TestBonsaiCFNames pins the fresh-mainnet-init CF set: count and creation
// order (KeyValueSegmentIdentifier enum order — determines RocksDB CF IDs).
func TestBonsaiCFNames(t *testing.T) {
	want := []string{
		"default",
		"\x01", // BLOCKCHAIN
		"\x03", // PRIVATE_TRANSACTIONS (legacy, retained)
		"\x04", // PRIVATE_STATE (legacy, retained)
		"\x06", // ACCOUNT_INFO_STATE
		"\x07", // CODE_STORAGE
		"\x08", // ACCOUNT_STORAGE_STORAGE
		"\x09", // TRIE_BRANCH_STORAGE
		"\x0a", // TRIE_LOG_STORAGE
		"\x0b", // VARIABLES
		"\x0c", // GOQUORUM_PRIVATE_STORAGE (legacy, retained)
		"\x0d", // BACKWARD_SYNC_HEADERS
		"\x0e", // BACKWARD_SYNC_BLOCKS
		"\x0f", // BACKWARD_SYNC_CHAIN
		"\x10", // SNAPSYNC_MISSING_ACCOUNT_RANGE
		"\x11", // SNAPSYNC_ACCOUNT_TO_FIX
		"\x12", // CHAIN_PRUNER_STATE
	}
	got := BonsaiCFNames()
	if len(got) != len(want) {
		t.Fatalf("BonsaiCFNames: %d CFs, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("BonsaiCFNames[%d] = %x, want %x", i, got[i], want[i])
		}
	}
}

// TestCFDefault pins the "default" CF as UTF-8 string, NOT a single byte.
func TestCFDefault(t *testing.T) {
	want := "default"
	if string(CFDefault) != want {
		t.Fatalf("CFDefault: got %q, want %q", string(CFDefault), want)
	}
}

// TestFlatDbStatusFull pins the FlatDbMode FULL byte and the genesis block
// number sentinel value length.
func TestFlatDbStatusFull(t *testing.T) {
	if len(FlatDbStatusFull) != 1 || FlatDbStatusFull[0] != 0x01 {
		t.Fatalf("FlatDbStatusFull: got %x, want [0x01]", FlatDbStatusFull)
	}
	if len(WorldBlockNumberGenesis) != 8 {
		t.Fatalf("WorldBlockNumberGenesis: len=%d, want 8", len(WorldBlockNumberGenesis))
	}
	for i, b := range WorldBlockNumberGenesis {
		if b != 0 {
			t.Fatalf("WorldBlockNumberGenesis[%d]=%x, want 0x00", i, b)
		}
	}
}
