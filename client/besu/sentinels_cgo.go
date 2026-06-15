//go:build cgo_besu

package besu

import (
	"github.com/ethereum/state-actor/internal/besu/keys"
)

// writeAdvisorySentinels writes the two TRIE_BRANCH_STORAGE sentinels that
// are NOT covered by NodeSink.SaveWorldState:
//
//   - flatDbStatus    = 0x01 (FULL)
//   - worldBlockNumber = 8 zero bytes (genesis)
//
// SaveWorldState handles the other two sentinels (worldRoot, worldBlockHash)
// plus the root-RLP write at key=Bytes.EMPTY.
//
// flatDbStatus is the load-bearing one: missing it would cause Besu's
// FlatDbStrategyProvider.deriveFlatDbStrategy to fall back to PARTIAL mode
// (BonsaiFlatDbStrategyProvider.java:41-48), which makes every account read
// fall through to the trie. With trie nodes present, that "works" but is
// silently slower. Writing 0x01 explicitly puts Besu in FULL mode.
//
// worldBlockNumber is advisory at boot but Besu's own writers always set
// it (PathBasedWorldState.java:219-225). Omitting it doesn't break boot
// but leaves the DB in an unusual state. We write it at genesis (block 0)
// = Bytes.ofUnsignedLong(0) = 8 zero bytes.
//
// These are written once after the trie/flat-state pipeline completes,
// before the genesis block writes. They go through the besuDB.put fast
// path (no batching needed — only 2 writes).
func (b *besuDB) writeAdvisorySentinels() error {
	if err := b.put(cfIdxTrieBranchStorage, keys.FlatDbStatusKey, keys.FlatDbStatusFull); err != nil {
		return err
	}
	if err := b.put(cfIdxTrieBranchStorage, keys.WorldBlockNumberKey, keys.WorldBlockNumberGenesis); err != nil {
		return err
	}
	return nil
}
