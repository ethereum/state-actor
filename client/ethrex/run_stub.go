//go:build !cgo_ethrex

// Build without the cgo_ethrex tag: no librocksdb, no cgo, no grocksdb
// dependency. runImpl returns the canned errNotImplemented error so
// --client=ethrex fails fast with a clear message pointing at Docker.
//
// This file is the only one in client/ethrex/ that compiles without the tag;
// everything else under client/ethrex/ that touches grocksdb is gated behind
// //go:build cgo_ethrex and excluded from the build entirely.

package ethrex

import (
	"context"

	"github.com/nerolation/state-actor/generator"
)

func runImpl(ctx context.Context, cfg generator.Config, opts Options) (*generator.Stats, error) {
	_ = ctx
	_ = cfg
	_ = opts
	return nil, errNotImplemented
}
