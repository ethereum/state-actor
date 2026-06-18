package ethrex

import (
	"context"
	"errors"

	"github.com/ethereum/state-actor/generator"
)

// errNotImplemented is returned by the !cgo_ethrex build's runImpl. The
// cgo+grocksdb wiring lives behind the cgo_ethrex build tag and is only
// available inside the Dockerfile.ethrex build context.
//
// Project decision: state-actor's ethrex path is Docker-only. Local Go builds
// without the tag (the default) return this error so users don't accidentally
// think --client=ethrex works on a dev machine without librocksdb installed.
// Build with `docker build -f Dockerfile.ethrex .` to use it.
var errNotImplemented = errors.New(
	"client/ethrex: requires the cgo_ethrex build tag and librocksdb. " +
		"--client=ethrex is Docker-only — build with `docker build -f Dockerfile.ethrex .`.",
)

// Run is the public entry point dispatched from main.go's `case "ethrex"` arm.
// It delegates to the build-tag-gated runImpl:
//
//   - Built with `-tags cgo_ethrex` (Docker only): runImpl in run_cgo.go opens
//     one grocksdb instance with 20 column families, drives entitygen →
//     ethrex.Builder → grocksdb writes, assembles the genesis block.
//   - Built without the tag (local default): runImpl in run_stub.go returns
//     errNotImplemented.
//
// The split keeps macOS/Linux dev builds free of cgo and librocksdb while
// the Docker image carries the real writer.
func Run(ctx context.Context, cfg generator.Config, opts Options) (*generator.Stats, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return runImpl(ctx, cfg, opts)
}
