//go:build !cgo_besu

// Build without the cgo_besu tag: no librocksdb, no cgo, no grocksdb
// dependency. runImpl returns the canned errNotImplemented error so
// `--client=besu` fails fast with a clear message pointing at Docker.
//
// This file is the only one in client/besu/ that compiles without the tag;
// everything else under client/besu/ that touches grocksdb is gated behind
// `//go:build cgo_besu` and excluded from the build entirely.

package besu

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
