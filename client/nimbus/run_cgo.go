//go:build cgo_nimbus

package nimbus

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/ethereum/go-ethereum/common"

	"github.com/ethereum/state-actor/generator"
	"github.com/ethereum/state-actor/genesis"
	"github.com/ethereum/state-actor/internal/genesisheader"
	"github.com/ethereum/state-actor/internal/memlimit"
	nimbusinternal "github.com/ethereum/state-actor/internal/nimbus"
)

// runImpl is the cgo_nimbus orchestrator. Write order is the load-bearing
// invariant:
//  1. DB open at <db>/ecdb (fresh-dir guard).
//  2. writeState → every AriVtx vertex + KvtGen code row, then the admin
//     record (sync); stateRoot obtained.
//  3. writeGenesisBlock → header / score / number→hash / fcuHead, then the
//     canonical-head row LAST with sync (nimbus's "initialised" gate).
//  4. WriteGenesisSidecar → <db>/nimbus-genesis.json for --network.
//  5. Close (KForce CompactRange + release).
func runImpl(ctx context.Context, cfg generator.Config, opts Options) (*generator.Stats, error) {
	_ = opts
	if cfg.DBPath == "" {
		return nil, errors.New("nimbus: --db is required")
	}
	if cfg.TrieMode == generator.TrieModeBinary {
		return nil, errors.New("nimbus: binary trie (EIP-7864) is not supported by Nimbus")
	}
	if cfg.Archive {
		return nil, errors.New("nimbus: --archive is not supported by the nimbus writer")
	}

	log.Printf("nimbus: %s", memlimit.Set(nimbusOffHeapReserveBytes))

	g := cfg.Genesis
	if g == nil {
		// Only reached by tests that omit a genesis; main.go always supplies one.
		var err error
		g, err = genesis.BuildSynthetic("osaka", nil, 0, 0, nil)
		if err != nil {
			return nil, fmt.Errorf("nimbus: build default genesis: %w", err)
		}
	}
	if g.Config == nil {
		return nil, errors.New("nimbus: cfg.Genesis must have Config set (use genesis.BuildSynthetic)")
	}

	if err := os.MkdirAll(cfg.DBPath, 0o755); err != nil {
		return nil, fmt.Errorf("nimbus: create datadir %s: %w", cfg.DBPath, err)
	}
	db, err := openNimbusDB(StoreDir(cfg.DBPath))
	if err != nil {
		return nil, err
	}
	defer db.Close()

	// LIFO: the sampler stops BEFORE db.Close() (it reads DB properties).
	stopMemorySampler := startMemorySampler(ctx, db)
	defer stopMemorySampler()

	vertexSink := newBatchSink(db, cfIdxAriVtx)
	defer vertexSink.Close()
	codeSink := newBatchSink(db, cfIdxKvtGen)
	defer codeSink.Close()

	alloc := nimbusinternal.NewVidAllocator()
	stateRoot, stats, err := writeState(ctx, cfg, db, alloc, vertexSink, codeSink)
	if err != nil {
		return nil, fmt.Errorf("nimbus: writeState: %w", err)
	}

	header := genesisheader.Build(g, 0, common.Hash{}, stateRoot)
	if err := writeGenesisBlock(db, header); err != nil {
		return nil, err
	}
	if err := WriteGenesisSidecar(cfg.DBPath, g); err != nil {
		return nil, err
	}

	stats.StateRoot = stateRoot
	return stats, nil
}
