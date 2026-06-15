//go:build !cgo_reth

package reth

import (
	"context"
	"errors"

	"github.com/ethereum/state-actor/generator"
)

// runCgoNotAvailableError documents to callers (and to TestRunCgoStubBuildPath)
// why RunCgo is unavailable. The cgo_reth build sets this to nil (run_cgo.go).
var runCgoNotAvailableError = errors.New(
	"client/reth: RunCgo requires the cgo_reth build tag and libmdbx. " +
		"--client=reth via direct-write is Docker-only — build with " +
		"`docker build -f Dockerfile.reth .`. Without cgo_reth, the JSONL " +
		"path via Populate() is still available.",
)

// RunCgo is the cgo-only entry point for direct-write reth datadirs. The
// stub returns runCgoNotAvailableError; the real implementation is in
// run_cgo.go (//go:build cgo_reth).
func RunCgo(ctx context.Context, cfg generator.Config, opts Options) (*generator.Stats, error) {
	return nil, runCgoNotAvailableError
}
