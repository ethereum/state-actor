//go:build cgo_reth

package reth

import (
	"bytes"
	"fmt"

	"github.com/erigontech/mdbx-go/mdbx"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	iReth "github.com/nerolation/state-actor/internal/reth"
)

// WriteMetadata populates the minimum-boot MDBX metadata into envs in a
// single atomic transaction. header must be the genesis header (block 0);
// archive controls whether to write PruneCheckpoint markers.
//
// Tables written:
//   - Metadata.storage_settings = SCALE-prefixed JSON `{"storage_v2":true}`
//     (selects reth's v2 reader/writer surface).
//   - StageCheckpoints: one entry per stage in iReth.StageIDsAll (15 entries).
//   - HeaderNumbers: header.Hash() → BE u64(0).
//   - BlockBodyIndices: BE u64(0) → Compact StoredBlockBodyIndices{0, 0}.
//   - PruneCheckpoints (NON-ARCHIVE ONLY): AccountHistory + StorageHistory
//     rows with block_number=Some(0), prune_mode=Before(1). Tells reth's
//     read path "history pruned before block 1" so historical-tag queries
//     route through HashedAccounts/HashedStorages instead of returning
//     NotYetWritten when the RocksDB history CFs are empty.
//
// VersionHistory is intentionally NOT written — reth's init_db writes its
// own ClientVersion on every boot. ChainState is left empty; reth populates
// it lazily on finality.
func WriteMetadata(envs *Envs, header *types.Header, archive bool) error {
	if header.Number.Sign() != 0 {
		return fmt.Errorf("WriteMetadata: header must be block 0, got %s", header.Number)
	}
	return envs.Mdbx.Update(func(txn *mdbx.Txn) error {
		if err := writeStorageSettings(txn, envs.MdbxDBIs["Metadata"]); err != nil {
			return fmt.Errorf("Metadata.storage_settings: %w", err)
		}
		if err := writeStageCheckpoints(txn, envs.MdbxDBIs["StageCheckpoints"], 0); err != nil {
			return fmt.Errorf("StageCheckpoints: %w", err)
		}
		if err := writeHeaderNumber(txn, envs.MdbxDBIs["HeaderNumbers"], header.Hash(), 0); err != nil {
			return fmt.Errorf("HeaderNumbers: %w", err)
		}
		if err := writeBlockBodyIndices(txn, envs.MdbxDBIs["BlockBodyIndices"], 0); err != nil {
			return fmt.Errorf("BlockBodyIndices: %w", err)
		}
		if !archive {
			if err := writePruneCheckpoints(txn, envs.MdbxDBIs["PruneCheckpoints"]); err != nil {
				return fmt.Errorf("PruneCheckpoints: %w", err)
			}
		}
		return nil
	})
}

// writePruneCheckpoints writes non-archive-mode PruneCheckpoint rows for
// AccountHistory + StorageHistory. Both rows use the same value:
// {block_number: Some(0), tx_number: None, prune_mode: Before(1)}. The
// block_number-is-Some bit tells reth's read path "history pruned before
// block 1" so historical-tag queries route through Hashed* state instead of
// returning NotYetWritten.
func writePruneCheckpoints(txn *mdbx.Txn, dbi mdbx.DBI) error {
	ckpt := iReth.PruneCheckpoint{
		BlockNumber: iReth.U64Ptr(0),
		TxNumber:    nil,
		PruneMode:   iReth.PruneMode{Kind: iReth.PruneModeBefore, Value: 1},
	}
	var valBuf bytes.Buffer
	ckpt.EncodeCompact(&valBuf)
	value := valBuf.Bytes()

	for _, segment := range []uint8{iReth.PruneSegmentAccountHistory, iReth.PruneSegmentStorageHistory} {
		key := iReth.EncodePruneSegmentKey(segment)
		if err := txn.Put(dbi, key, value, 0); err != nil {
			return fmt.Errorf("segment %d: %w", segment, err)
		}
	}
	return nil
}

// writeStorageSettings puts a SCALE-wrapped JSON storage_settings row into
// the Metadata table.
//
// Wire: `outer_len_prefix[1 byte] || inner_json[19 bytes]`.
//
// Reth's Metadata table value type is Vec<u8> with a SCALE Decompress impl
// (reth-codecs-0.3.1/src/compress/scale.rs). For inner lengths 0-63 the
// SCALE compact-length prefix is a single byte = (len << 2). Reth's trait
// reader (storage-api/src/metadata.rs) then serde-decodes the inner
// bytes as StorageSettings. Our payload is 19 bytes (`{"storage_v2":true}`)
// so the prefix is 0x4C.
func writeStorageSettings(txn *mdbx.Txn, dbi mdbx.DBI) error {
	inner := []byte(`{"storage_v2":true}`)
	if len(inner) > 63 {
		// Defensive: switch to 2-byte SCALE prefix if the JSON ever grows.
		// Today's payload is 19 bytes — well under the single-byte cap.
		return fmt.Errorf("storage_settings JSON length %d > 63; needs 2-byte SCALE prefix encoder", len(inner))
	}
	value := make([]byte, 0, 1+len(inner))
	value = append(value, byte(len(inner)<<2)) // SCALE single-byte compact-length prefix.
	value = append(value, inner...)
	return txn.Put(dbi, []byte("storage_settings"), value, 0)
}

// writeStageCheckpoints writes one StageCheckpoint{BlockNumber: blockNum}
// per stage in iReth.StageIDsAll, Compact-encoded, into the StageCheckpoints
// table.
func writeStageCheckpoints(txn *mdbx.Txn, dbi mdbx.DBI, blockNum uint64) error {
	for _, stage := range iReth.StageIDsAll {
		sc := iReth.StageCheckpoint{BlockNumber: blockNum}
		var buf bytes.Buffer
		sc.EncodeCompact(&buf)
		if err := txn.Put(dbi, []byte(stage), buf.Bytes(), 0); err != nil {
			return fmt.Errorf("stage %q: %w", stage, err)
		}
	}
	return nil
}

// writeHeaderNumber puts hash → BE u64(num) into HeaderNumbers.
func writeHeaderNumber(txn *mdbx.Txn, dbi mdbx.DBI, hash common.Hash, num uint64) error {
	val := beU64(num)
	return txn.Put(dbi, hash[:], val[:], 0)
}

// writeBlockBodyIndices puts BE_u64(blockNum) → Compact(StoredBlockBodyIndices{0, 0})
// into BlockBodyIndices.
func writeBlockBodyIndices(txn *mdbx.Txn, dbi mdbx.DBI, blockNum uint64) error {
	bbi := iReth.StoredBlockBodyIndices{FirstTxNum: 0, TxCount: 0}
	var buf bytes.Buffer
	bbi.EncodeCompact(&buf)
	key := beU64(blockNum)
	return txn.Put(dbi, key[:], buf.Bytes(), 0)
}

// beU64 encodes v as 8 big-endian bytes.
func beU64(v uint64) [8]byte {
	return [8]byte{
		byte(v >> 56), byte(v >> 48), byte(v >> 40), byte(v >> 32),
		byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v),
	}
}
