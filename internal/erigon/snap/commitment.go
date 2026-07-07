package snap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
)

// KeyCommitmentState is the on-disk key whose value carries the HPH
// trie-state header that Erigon's daemon reads on first FCU to anchor
// commitment continuation. Mirrors `commitmentdb.KeyCommitmentState`
// in upstream (`execution/commitment/commitmentdb/commitment_context.go`,
// line 587-589). The exact bytes are ASCII "state".
//
// In the snapshot commitment .kv this record lives alongside the HPH
// branch nodes, and the .kv contract is ascending key order — so
// WriteCommitment merges it into the branch stream at its binary sort
// position (branch keys are compact-encoded nibble paths; "state" is
// 0x73,0x74,0x61,0x74,0x65, typically landing near the end).
var KeyCommitmentState = []byte("state")

// BranchStream yields HPH branch rows (prefix, branchData) in ASCENDING
// prefix order — the commitment package's Result.BranchIterate satisfies it
// directly. Yielded slices may alias the producer's buffers; WriteCommitment
// consumes each row synchronously before requesting the next.
type BranchStream func(yield func(prefix, data []byte) error) error

// WriteCommitment emits the Commitment-domain snapshot file set
// (commitment.<from>-<to>.kv + .kvi + .kvei) for the given step range
// from a sorted stream of HPH branch rows. The KeyCommitmentState record
// is spliced into the stream at its sort position (a 1-key 2-way merge) —
// the caller does NOT pre-insert it. This consumes the branch rows
// STRAIGHT from the commitment walk's live branch store: no intermediate
// re-sort store, which at 100 GB scale elides a full extra Pebble
// write+compaction+read of the ~44 GB branch set.
//
// `keyState` MUST be the output of the KeyCommitmentState value encoder
// (BE u64 txNum + BE u64 blockNum + BE u16 trieStateLen + raw
// EncodeCurrentState bytes; ~683-815 bytes for a genesis HPH). See the
// state-actor commitment package for the encoder.
//
// `branchCount` is the number of rows `branches` will yield.
// WriteCommitment passes `branchCount+1` to WriteDomain so the bloom +
// recsplit sizing accounts for the spliced KeyCommitmentState record.
//
// In the astronomically unlikely event a branch prefix collides with
// KeyCommitmentState ("state" is a valid 5-byte key shape), WriteCommitment
// FAILS LOUDLY. (The previous implementation's last-write-wins streamsort
// Put would have silently dropped the colliding branch row — a corrupt
// commitment snapshot with a desynced keyCount; erroring is strictly
// better for a ~2^-40 event.)
//
// The commitment domain's default AccessorMask
// (AccessorHashMap | AccessorExistence per state_schema.go:261) is used
// — no AccessorBTree. WriteDomain emits .kvi (RecSplit MPHF) and .kvei
// (existence bloom) sidecars.
func WriteCommitment(
	ctx context.Context,
	w *Writer,
	r StepRange,
	keyState []byte,
	branches BranchStream,
	branchCount uint64,
) error {
	if len(keyState) == 0 {
		return fmt.Errorf("snap.WriteCommitment: keyState is empty")
	}
	// Splice the state row into the ascending branch stream. streamErr
	// captures producer-side iterator errors: WriteDomain's push-style
	// yield has no error channel, so a truncated stream would otherwise
	// build a silently-short .kv (the bug class this repo bans).
	var streamErr error
	entries := func(yield func(DomainEntry) bool) {
		// Per-invocation state: WriteDomain drives entries TWICE
		// (CompressFromSource count + encode passes) — declared inside so
		// each pass re-splices the state row and re-arms the ascending
		// assert; hoisting these would silently drop the state row from
		// pass 2.
		stateEmitted := false
		var prevPrefix []byte
		streamErr = branches(func(prefix, data []byte) error {
			// The .kv contract is ascending keys and WriteDomain does not
			// check ("behaviour is undefined") — a mis-ordered producer would
			// otherwise corrupt the snapshot silently.
			if prevPrefix != nil && bytes.Compare(prefix, prevPrefix) <= 0 {
				return fmt.Errorf("branch stream not ascending: %x after %x", prefix, prevPrefix)
			}
			prevPrefix = append(prevPrefix[:0], prefix...)
			if !stateEmitted {
				switch cmp := bytes.Compare(KeyCommitmentState, prefix); {
				case cmp < 0:
					if !yield(DomainEntry{Key: KeyCommitmentState, Value: keyState}) {
						return errStreamingStop
					}
					stateEmitted = true
				case cmp == 0:
					// Collision: refuse to build a snapshot that silently
					// drops a branch row (see doc).
					return fmt.Errorf("branch prefix %x collides with KeyCommitmentState", prefix)
				}
			}
			if !yield(DomainEntry{Key: prefix, Value: data}) {
				return errStreamingStop
			}
			return nil
		})
		if errors.Is(streamErr, errStreamingStop) {
			streamErr = nil // consumer-initiated stop, not a producer error
			return
		}
		if streamErr == nil && !stateEmitted {
			// All branch keys sorted before "state" (or empty stream):
			// state goes last.
			_ = yield(DomainEntry{Key: KeyCommitmentState, Value: keyState})
			stateEmitted = true
		}
	}
	werr := w.WriteDomain(ctx, DomainCommitment, r, branchCount+1, entries)
	// The producer error is the ROOT CAUSE — report it ahead of WriteDomain's
	// downstream symptom (e.g. recsplit key-count mismatch on the truncated
	// stream).
	if streamErr != nil {
		return fmt.Errorf("snap.WriteCommitment: branch stream: %w", streamErr)
	}
	return werr
}
