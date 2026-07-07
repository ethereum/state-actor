// STATE-ACTOR ADDITION — NOT vendored from erigon. This file extends the
// vendored engine (see the sibling files' provenance headers) with a bulk,
// from-empty fold driver that feeds followAndUpdate directly from sorted
// per-nibble streams. It replicates the upstream concurrent engine's
// choreography (hex_concurrent_patricia_hashed.go:212-300) — the only
// semantic difference is the feed: a KeyStream instead of an etl collector,
// with the Update passed inline instead of re-fetched via ctx.Account/
// Storage (three inert trace prints are also dropped). No Updates object,
// no Touch dedup map, no etl spill, no chunking.
//
//go:build cgo_erigon_commitment

package hph

import (
	"context"
	"fmt"

	"golang.org/x/sync/errgroup"
)

// TrieContextFactory creates fresh PatriciaContext instances for the
// per-shard fold workers. (Relocated from the deleted warmuper.go — the
// warmup subsystem itself was never enabled.)
type TrieContextFactory func() (PatriciaContext, func())

// KeyStream yields one first-nibble shard's rows in ascending hashedKey
// order. hashedKey is the nibblized keccak key (64 B account / 128 B
// storage — KeyToHexNibbleHash output); plainKey the 20/52-byte plain key;
// u the decoded Update (consumed synchronously by followAndUpdate/updateCell,
// which copy what they keep — the yielded slices and *Update may be reused
// by the stream on the next call). ok=false ends the stream.
type KeyStream interface {
	Next() (hashedKey, plainKey []byte, u *Update, ok bool, err error)
}

// DirectFold builds the trie from empty in ONE concurrent pass: 16 mounted
// subtries, worker n draining streams[n] through followAndUpdate with the
// inline Update, then foldNibble + the serial root fold — exactly
// the upstream concurrent fold sequence. The caller owns the root flush
// (ApplyAndClearInlineDeferredUpdates) and EncodeCurrentState, as with
// the upstream engine.
//
// From-empty invariants this relies on (and which make ctx.Branch reads
// nil-safe): a sorted single pass never re-descends a folded prefix, and
// every branch prefix is folded exactly once.
func DirectFold(ctx context.Context, pph *ConcurrentPatriciaHashed, trieCtxFactory TrieContextFactory, streams *[16]KeyStream) ([]byte, error) {
	if err := pph.unfoldRoot(ctx, trieCtxFactory); err != nil {
		return nil, err
	}

	// Derived context for the errgroup only — the root fold below must not
	// see the spurious context.Canceled that g.Wait() leaves on gctx.
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(16)

	for n := 0; n < len(streams); n++ {
		stream := streams[n]
		phnib := pph.mounts[n]
		ni := n

		g.Go(func() error {
			// close the temporary context provisioned by unfoldRoot before
			// replacing it
			if pph.ctxClosers[ni] != nil {
				pph.ctxClosers[ni]()
				pph.ctxClosers[ni] = nil
			}
			trieCtx, trieCtxClose := trieCtxFactory()
			defer trieCtxClose()
			phnib.ResetContext(trieCtx)

			cnt := 0
			for {
				select {
				case <-gctx.Done():
					return gctx.Err()
				default:
				}
				hashedKey, plainKey, u, ok, err := stream.Next()
				if err != nil {
					return fmt.Errorf("DirectFold stream[%x]: %w", ni, err)
				}
				if !ok {
					break
				}
				cnt++
				if err := phnib.followAndUpdate(hashedKey, plainKey, u); err != nil {
					return fmt.Errorf("followAndUpdate[%x]: %w", ni, err)
				}
			}
			if cnt == 0 {
				return nil
			}
			return pph.foldNibble(gctx, ni)
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	if pph.root.activeRows == 0 {
		pph.root.activeRows = 1
	}
	for pph.root.activeRows > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := pph.root.fold(); err != nil {
			return nil, err
		}
	}
	return pph.root.RootHash()
}
