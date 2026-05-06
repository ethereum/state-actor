package besu

import (
	"context"
	"errors"

	"github.com/nerolation/state-actor/generator"
)

// errNotImplemented is returned by the !cgo_besu build's runImpl. The
// stage-2 cgo+grocksdb wiring lives behind the cgo_besu build tag and is
// only available inside the Dockerfile.besu build context.
//
// Project decision: state-actor's Besu path is **Docker-only**. Local Go
// builds without the tag (the default) return this error so users don't
// accidentally think `--client=besu` works on their macOS / Linux dev
// machine without librocksdb installed. Build with
// `docker build -f Dockerfile.besu .` to use it.
var errNotImplemented = errors.New(
	"client/besu: requires the cgo_besu build tag and librocksdb. " +
		"--client=besu is Docker-only — build with `docker build -f Dockerfile.besu .`. " +
		"See client/besu/testdata/README.md for the reproducer (or `make smoke-besu`).",
)

// Run is the public entry point dispatched from main.go's `case "besu"` arm.
// It delegates to the build-tag-gated runImpl:
//
//   - Built with `-tags cgo_besu` (Docker only): runImpl in run_cgo.go opens
//     one grocksdb instance with 8 column families, drives entitygen →
//     trie.Builder → grocksdb writes, assembles the genesis block.
//   - Built without the tag (local default): runImpl in run_stub.go returns
//     errNotImplemented.
//
// The split keeps macOS/Linux dev builds free of cgo and librocksdb while
// the Docker image carries the real writer.
func Run(ctx context.Context, cfg generator.Config, opts Options) (*generator.Stats, error) {
	return runImpl(ctx, cfg, opts)
}

