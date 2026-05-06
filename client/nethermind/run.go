package nethermind

import (
	"context"
	"errors"

	"github.com/nerolation/state-actor/generator"
)

// errNotImplemented is returned by the !cgo_neth build's runImpl. The
// stage-2 cgo+grocksdb wiring lives behind the cgo_neth build tag and
// is only available inside the Dockerfile.nethermind build context.
//
// Project decision: state-actor's Nethermind path is **Docker-only**.
// Local Go builds without the tag (the default) return this error so
// users don't accidentally think `--client=nethermind` works on their
// macOS / Linux dev machine without librocksdb installed. Build with
// `docker build -f Dockerfile.nethermind .` to use it.
var errNotImplemented = errors.New(
	"client/nethermind: requires the cgo_neth build tag and librocksdb. " +
		"--client=nethermind is Docker-only — build with `docker build -f Dockerfile.nethermind .`. " +
		"See client/nethermind/testdata/README.md for the reproducer (or `make smoke-nethermind`).",
)

// Run is the public entry point dispatched from main.go's
// `case "nethermind"` arm. It delegates to the build-tag-gated runImpl:
//
//   - Built with `-tags cgo_neth` (Docker only): runImpl in run_cgo.go opens
//     7 grocksdb instances, drives entitygen → trie.Builder → grocksdb writes,
//     assembles the genesis block tree.
//   - Built without the tag (local default): runImpl in run_stub.go returns
//     errNotImplemented.
//
// The split keeps macOS/Linux dev builds free of cgo and librocksdb while
// the Docker image carries the real writer.
func Run(ctx context.Context, cfg generator.Config, opts Options) (*generator.Stats, error) {
	return runImpl(ctx, cfg, opts)
}

