//go:build cgo_reth

package reth

import (
	"bytes"
	"encoding/json"
	"math/big"
	"testing"

	"github.com/erigontech/mdbx-go/mdbx"
	"github.com/ethereum/go-ethereum/core/types"

	iReth "github.com/ethereum/state-actor/internal/reth"
)

func TestWriteMetadataAllTables(t *testing.T) {
	// Run in both archive and non-archive modes — most tables are identical
	// between them; the PruneCheckpoints rows are the only difference.
	for _, tc := range []struct {
		name    string
		archive bool
	}{
		{"archive=true", true},
		{"archive=false", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			envs, err := OpenEnvs(tmp, true)
			if err != nil {
				t.Fatalf("OpenEnvs: %v", err)
			}
			defer envs.Close()

			header := &types.Header{
				Number: big.NewInt(0),
			}

			if err := WriteMetadata(envs, header, tc.archive); err != nil {
				t.Fatalf("WriteMetadata: %v", err)
			}

			// Verify Metadata.storage_settings — key matches reth's
			// STORAGE_SETTINGS const; value is SCALE-prefixed JSON.
			// 0x4C = (19 << 2) is the SCALE single-byte compact-length
			// prefix for the 19-byte JSON payload {"storage_v2":true}.
			// See writeStorageSettings doc comment for the full wire-
			// format rationale.
			if err := envs.Mdbx.View(func(txn *mdbx.Txn) error {
				val, err := txn.Get(envs.MdbxDBIs["Metadata"], []byte("storage_settings"))
				if err != nil {
					return err
				}
				want := append([]byte{0x4C}, []byte(`{"storage_v2":true}`)...)
				if !bytes.Equal(val, want) {
					t.Errorf("Metadata[storage_settings] = %x, want %x", val, want)
				}
				return nil
			}); err != nil {
				t.Errorf("verify Metadata: %v", err)
			}

			// Verify all 15 StageCheckpoints at block 0
			if err := envs.Mdbx.View(func(txn *mdbx.Txn) error {
				for _, stage := range iReth.StageIDsAll {
					val, err := txn.Get(envs.MdbxDBIs["StageCheckpoints"], []byte(stage))
					if err != nil {
						return err
					}
					var sc iReth.StageCheckpoint
					sc.DecodeCompact(val, len(val))
					if sc.BlockNumber != 0 {
						t.Errorf("StageCheckpoints[%s] block_number = %d, want 0", stage, sc.BlockNumber)
					}
				}
				return nil
			}); err != nil {
				t.Errorf("verify StageCheckpoints: %v", err)
			}

			// Verify HeaderNumbers
			expectedHash := header.Hash()
			if err := envs.Mdbx.View(func(txn *mdbx.Txn) error {
				val, err := txn.Get(envs.MdbxDBIs["HeaderNumbers"], expectedHash[:])
				if err != nil {
					return err
				}
				if len(val) != 8 {
					t.Errorf("HeaderNumbers value len = %d, want 8", len(val))
				}
				// All-zero BE u64 = block 0
				for i, b := range val {
					if b != 0 {
						t.Errorf("HeaderNumbers value byte %d = %#x, want 0", i, b)
					}
				}
				return nil
			}); err != nil {
				t.Errorf("verify HeaderNumbers: %v", err)
			}

			// Verify BlockBodyIndices
			if err := envs.Mdbx.View(func(txn *mdbx.Txn) error {
				key := []byte{0, 0, 0, 0, 0, 0, 0, 0} // BE u64 of 0
				val, err := txn.Get(envs.MdbxDBIs["BlockBodyIndices"], key)
				if err != nil {
					return err
				}
				var bbi iReth.StoredBlockBodyIndices
				bbi.DecodeCompact(val, len(val))
				if bbi.FirstTxNum != 0 || bbi.TxCount != 0 {
					t.Errorf("BlockBodyIndices = %+v, want {0, 0}", bbi)
				}
				return nil
			}); err != nil {
				t.Errorf("verify BlockBodyIndices: %v", err)
			}

			// Verify PruneCheckpoints: present iff !archive.
			if err := envs.Mdbx.View(func(txn *mdbx.Txn) error {
				for _, segment := range []uint8{
					iReth.PruneSegmentAccountHistory,
					iReth.PruneSegmentStorageHistory,
				} {
					key := iReth.EncodePruneSegmentKey(segment)
					val, err := txn.Get(envs.MdbxDBIs["PruneCheckpoints"], key)
					if tc.archive {
						// Archive mode: row MUST be absent. mdbx-go returns
						// mdbx.NotFound (errno NotFound) for a missing key.
						if err == nil {
							t.Errorf("PruneCheckpoints[%d] = %x, want NotFound in archive mode", segment, val)
						}
						continue
					}
					// Non-archive: row MUST be present and match the expected
					// Compact-encoded PruneCheckpoint{Some(0), None, Before(1)} = 01 00 02 01.
					if err != nil {
						return err
					}
					want := []byte{0x01, 0x00, 0x02, 0x01}
					if !bytes.Equal(val, want) {
						t.Errorf("PruneCheckpoints[%d] = %x, want %x", segment, val, want)
					}
				}
				return nil
			}); err != nil {
				t.Errorf("verify PruneCheckpoints: %v", err)
			}

			// NOTE: VersionHistory is intentionally NOT written by WriteMetadata.
		})
	}
}

// TestWriteMetadata_StorageSettingsSCALEWrappedJSON pins the byte-level
// wire format reth actually reads: SCALE compact-length-prefixed JSON
// (NOT raw Compact bitflag, NOT raw JSON).
//
// Byte 0 is (inner_len << 2) for inner_len ∈ [0, 63] (parity_scale_codec
// single-byte compact mode). Bytes 1..end are the serde JSON serialization
// of reth's StorageSettings struct. If reth bumps the wire format this
// test fails without needing to spin docker.
func TestWriteMetadata_StorageSettingsSCALEWrappedJSON(t *testing.T) {
	tmp := t.TempDir()
	envs, err := OpenEnvs(tmp, true)
	if err != nil {
		t.Fatalf("OpenEnvs: %v", err)
	}
	defer envs.Close()

	header := &types.Header{Number: big.NewInt(0)}
	if err := WriteMetadata(envs, header, true); err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}

	if err := envs.Mdbx.View(func(txn *mdbx.Txn) error {
		val, err := txn.Get(envs.MdbxDBIs["Metadata"], []byte("storage_settings"))
		if err != nil {
			return err
		}
		if len(val) < 2 {
			t.Fatalf("Metadata[storage_settings] = %x (%d bytes); want >= 2 (SCALE prefix + JSON)", val, len(val))
		}
		// SCALE single-byte mode: low 2 bits == 0, upper 6 bits = length.
		if val[0]&0b11 != 0 {
			t.Errorf("SCALE prefix %#x: low 2 bits = %#b, want 0b00 (single-byte mode)", val[0], val[0]&0b11)
		}
		innerLen := int(val[0] >> 2)
		if innerLen != len(val)-1 {
			t.Errorf("SCALE prefix says inner_len=%d, actual remaining bytes = %d", innerLen, len(val)-1)
		}
		// Decode inner JSON via encoding/json — matches what reth's
		// serde_json::from_slice does after SCALE-decoding the outer Vec<u8>.
		var got struct {
			StorageV2 bool `json:"storage_v2"`
		}
		if err := json.Unmarshal(val[1:], &got); err != nil {
			t.Errorf("inner JSON parse failed: %v (bytes = %s)", err, val[1:])
		}
		if !got.StorageV2 {
			t.Errorf("decoded storage_v2 = false, want true")
		}
		return nil
	}); err != nil {
		t.Errorf("read Metadata[storage_settings]: %v", err)
	}
}
