package nimbus

import (
	"context"
	"errors"

	"github.com/ethereum/state-actor/generator"
)

// errNotImplemented is returned by the !cgo_nimbus build's runImpl. The
// cgo+grocksdb wiring lives behind the cgo_nimbus build tag and is only
// available inside the Dockerfile.nimbus build context.
//
// Project decision: state-actor's nimbus path is Docker-only, like every
// other RocksDB client. Local Go builds without the tag (the default) return
// this error so users don't mistake --client=nimbus for a working path on a
// machine without librocksdb. Build with `docker build -f Dockerfile.nimbus .`.
var errNotImplemented = errors.New(
	"client/nimbus: requires the cgo_nimbus build tag and librocksdb. " +
		"--client=nimbus is Docker-only — build with `docker build -f Dockerfile.nimbus .`.",
)

// Run is the public entry point dispatched from main.go's `case "nimbus"` arm.
// It delegates to the build-tag-gated runImpl:
//
//   - Built with `-tags cgo_nimbus` (Docker only): runImpl in run_cgo.go opens
//     one grocksdb instance with nimbus's column families, streams the state
//     through internal/nimbus.Builder into Aristo vertex rows, and writes the
//     genesis block + sidecar.
//   - Built without the tag (local default): runImpl in run_stub.go returns
//     errNotImplemented.
func Run(ctx context.Context, cfg generator.Config, opts Options) (*generator.Stats, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return runImpl(ctx, cfg, opts)
}
