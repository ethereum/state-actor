//go:build !cgo_nimbus

// Build without the cgo_nimbus tag: no librocksdb, no cgo, no grocksdb
// dependency. runImpl returns the canned errNotImplemented error so
// --client=nimbus fails fast with a clear message pointing at Docker.

package nimbus

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
