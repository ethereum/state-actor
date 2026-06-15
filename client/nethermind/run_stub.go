//go:build !cgo_neth

// Build without the cgo_neth tag: no librocksdb, no cgo, no grocksdb
// dependency. runImpl returns the canned errNotImplemented error so
// `--client=nethermind` fails fast with a clear message.
//
// This file is the only one that compiles without the tag; everything
// else under client/nethermind/ that touches grocksdb is gated behind
// `//go:build cgo_neth` and excluded from the build entirely.

package nethermind

import (
	"context"

	"github.com/ethereum/state-actor/generator"
)

func runImpl(ctx context.Context, cfg generator.Config, opts Options) (*generator.Stats, error) {
	_ = ctx
	_ = cfg
	_ = opts
	return nil, errNotImplemented
}
