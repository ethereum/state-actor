//go:build cgo_erigon

// runImpl writes a bootable Erigon v3 chaindata directory in two tiers:
// `erigon init` lays down a minimal MDBX (genesis header + chain
// config), then writeSnapshots streams the bloat state (accounts /
// storage / code / commitment) into Erigon's flat snapshot .kv files and
// patches block 0's stateRoot to the snapshot-tier root.
//
// The cgo_erigon build imports github.com/erigontech/erigon only for the
// commitment-root computation (internal/erigon/commitment, behind
// cgo_erigon_commitment); the erigon daemon itself is invoked as a CLI
// from the Docker image built by Dockerfile.erigon.

package erigon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/ethereum/state-actor/generator"
)

// erigonBinary is the path to the erigon CLI inside the Docker image
// (see Dockerfile.erigon stage 3). Override via Options.ErigonBin for
// tests or non-Docker invocations.
const erigonBinary = "/usr/local/bin/erigon"

// erigonRootLogRegex matches the "Successfully wrote genesis state" log
// line emitted by `erigon init` after CommitGenesisBlock returns. The
// `root` field is the genesis state-root we surface as Stats.StateRoot.
var erigonRootLogRegex = regexp.MustCompile(`Successfully wrote genesis state\b.*\broot=(0x[0-9a-fA-F]+)`)

func runImpl(ctx context.Context, cfg generator.Config, opts Options) (*generator.Stats, error) {
	startedAt := time.Now()

	if cfg.DBPath == "" {
		return nil, fmt.Errorf("client/erigon: cfg.DBPath is required")
	}
	if cfg.Genesis == nil {
		return nil, fmt.Errorf("client/erigon: cfg.Genesis is required (set via genesis.BuildSynthetic in main.go)")
	}

	// 0. Create the data dir (chaindata + snapshots subdirs both
	// auto-created by erigon init / `datadir.New`).
	if err := os.MkdirAll(cfg.DBPath, 0o755); err != nil {
		return nil, fmt.Errorf("client/erigon: mkdir dbPath %q: %w", cfg.DBPath, err)
	}

	// 1. Materialise the spec foundational alloc (PreAlloc +
	// GenesisAccounts). The AutoFill draw loop runs inside
	// writeSnapshots (snapshot_cgo.go) where it streams directly into
	// the snapshot streamsorts — never aggregating 30 M+ EOAs in
	// memory. NONE of this is written to genesis.json — every
	// state-actor-generated entry ends up in the snapshot tier only.
	foundational, stats, err := buildAllocMap(cfg)
	if err != nil {
		return nil, fmt.Errorf("client/erigon: build alloc: %w", err)
	}

	// 2. Serialize genesis.json with an EMPTY alloc. This makes
	// erigon init's single-value mdbx_put into kv.ConfigTable["genesis"]
	// trivially small (no MDBX_BAD_VALSIZE risk at any --target-size),
	// and keeps MDBX free of state-actor data. Erigon's WriteGenesisBlock
	// auto-adds the per-fork system contracts based on ChainConfig
	// (EIP-4788 BeaconRoot etc.); those are the only writes erigon init
	// produces. Every state-actor entry — including PreAlloc /
	// GenesisAccounts / AutoFill — flows into snapshot files instead.
	genesisPath := filepath.Join(cfg.DBPath, "genesis.json")
	if err := writeGenesisJSON(cfg.Genesis, genesisPath, nil); err != nil {
		return nil, fmt.Errorf("client/erigon: write genesis.json: %w", err)
	}

	// 3. Exec `erigon init <genesis.json> --datadir <dbPath>`.
	bin := erigonBinary
	// Arg order matters: erigon's urfave/cli parser binds --datadir to
	// the `init` subcommand's flag table ONLY if the flag comes BEFORE
	// the <genesisPath> positional. With the flag AFTER the path the
	// parser silently falls back to the default DataDir
	// (`/root/.local/share/erigon`) and writes chaindata there.
	//
	// We deliberately do NOT pass --chain to init. The default is
	// "mainnet"; the chain ID in genesis.json (1337) is what actually
	// goes into MDBX. The downstream `erigon` daemon must also NOT
	// pass --chain dev: v3.4.2's daemon rejects --chain dev against an
	// existing-chaindata boot with "Fatal: chain name is not recognized:
	// dev" (a chain-name lookup that bypasses Dev's short-circuit on
	// the daemon path; init has its own validator that doesn't trip).
	// Block production at boot is driven by --dev.period + miner flags
	// instead. Found via the v1 bench-iteration loop.
	cmd := exec.CommandContext(ctx, bin, "init", "--datadir", cfg.DBPath, genesisPath)
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		return nil, fmt.Errorf("client/erigon: erigon init failed: %w\nOutput:\n%s", runErr, string(out))
	}

	// 4. Parse the genesis state-root from erigon's log output.
	matches := erigonRootLogRegex.FindStringSubmatch(string(out))
	if len(matches) >= 2 {
		var root common.Hash
		b, decErr := hexutil.Decode(matches[1])
		if decErr == nil && len(b) == 32 {
			copy(root[:], b)
			stats.StateRoot = root
		}
	}

	// 5. Write a chainspec.json sidecar (informational — Erigon reads
	// the chain config from MDBX's ConfigTable after init). Some
	// external tooling (oracle tests, bench scripts) reads
	// <dbPath>/chainspec.json, so emit it for compatibility with the
	// reth/besu/nethermind precedent. Empty alloc here too — sidecar
	// is for chain config inspection; state lives in snapshots.
	chainspecPath := filepath.Join(cfg.DBPath, "chainspec.json")
	if err := writeGenesisJSON(cfg.Genesis, chainspecPath, nil); err != nil {
		// Non-fatal: erigon init already succeeded.
		_ = err
	}

	// 5b. Write SyncStage progress markers so erigon's daemon-boot
	// AllSegmentsDownloadComplete gate clears (>0 OtterSync). Without
	// this, engine_forkchoiceUpdated returns SYNCING forever and no
	// blocks are produced. See sync_stages_cgo.go for the mechanism.
	// (If snapshot-tier presence alone ever satisfies the gate, this
	// call could become unnecessary.)
	if err := writeSyncStageMarkers(cfg.DBPath); err != nil {
		return nil, fmt.Errorf("client/erigon: writeSyncStageMarkers: %w", err)
	}

	// 6. Make the datadir world-readable + group-writable. The state-actor
	// container runs as root, so erigon init writes chaindata as root.
	// The downstream `erigontech/erigon:v3.4.2` container runs as a
	// non-root user (uid 1001 "erigon") and would fail with "permission
	// denied" trying to read /data/nodekey / chaindata/. A blanket chmod
	// is safe in this test-only flow because the datadir lives in a bind
	// mount the user controls.
	if err := chmodRecursive(cfg.DBPath, 0o777); err != nil {
		// Non-fatal, but log it: a silent chmod failure resurfaces later
		// as an opaque "permission denied" when the non-root erigon
		// daemon container reads the datadir.
		fmt.Fprintf(os.Stderr, "client/erigon: warning: chmod datadir %q: %v\n", cfg.DBPath, err)
	}

	// 7. Streaming snapshot orchestrator: write the
	// accounts/storage/code/commitment .kv snapshot files + the FS
	// preconditions (salt-state.txt, erigondb.toml), compute the HPH
	// commitment root over the post-bloat state, and patch block 0's
	// header.stateRoot so the daemon's first FCU validates.
	root, err := writeSnapshots(ctx, cfg, foundational, stats)
	if err != nil {
		return nil, fmt.Errorf("client/erigon: writeSnapshots: %w", err)
	}
	if err := patchGenesisHeaderStateRoot(cfg.DBPath, root); err != nil {
		return nil, fmt.Errorf("client/erigon: patchGenesisHeaderStateRoot: %w", err)
	}
	stats.StateRoot = root

	stats.GenerationTime = time.Since(startedAt)
	return stats, nil
}

// chmodRecursive walks root and sets mode on every entry. Stops on first
// error.
func chmodRecursive(root string, mode os.FileMode) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chmod(path, mode)
	})
}

// FoundationalAlloc holds the spec-foundational entries
// (cfg.PreAlloc + cfg.GenesisAccounts) that buildAllocMap materializes
// in memory. Bounded by `|cfg.PreAlloc| + |cfg.GenesisAccounts|` —
// a few thousand entries at any `--target-size`. Retained for two
// purposes:
//
//  1. genesisAddrs dedup set for the autofill RNG redraw loop in
//     writeSnapshots (must match geth's byte-for-byte). geth dedups
//     autofill draws against the foundational set only, so this set
//     stays small.
//  2. Range-0 (deepest) snapshot routing: the streaming orchestrator
//     writes every Spec entry into rangeIdx=0 of the snapshot
//     streamsorts before starting the autofill loop.
type FoundationalAlloc struct {
	Spec map[common.Address]*allocAccount
}

// buildAllocMap materializes cfg.PreAlloc + cfg.GenesisAccounts into
// the spec map. AutoFill processing (the 30 M+ EOA / 512 K contract
// draw loop) is NOT done here — it streams directly into the
// snapshot streamsorts from writeSnapshots (snapshot_cgo.go) to keep
// memory bounded.
//
// Returns:
//   - foundational — spec map (small; retained for genesisAddrs +
//     range-0 snapshot writes)
//   - stats        — counts + byte tallies for the spec portion only;
//     writeSnapshots adds autofill numbers
//
// State-actor writes ZERO state-actor data to genesis.json
// (writeGenesisJSON receives nil — see runImpl). The only thing
// erigon init writes to MDBX is the per-fork system contracts that
// Erigon's WriteGenesisBlock auto-adds, plus chain-config tables.
// patchGenesisHeaderStateRoot overwrites block 0's stateRoot with
// the HPH-over-(foundational+autofill) value once writeSnapshots
// completes.
func buildAllocMap(cfg generator.Config) (*FoundationalAlloc, *generator.Stats, error) {
	foundational := &FoundationalAlloc{Spec: make(map[common.Address]*allocAccount)}
	stats := &generator.Stats{}

	for addr, sa := range cfg.GenesisAccounts {
		entry := &allocAccount{
			Nonce: sa.Nonce,
		}
		if sa.Balance != nil {
			entry.Balance = sa.Balance.ToBig()
		}
		if code := cfg.GenesisCode[addr]; len(code) > 0 {
			entry.Code = code
			stats.CodeBytes += uint64(len(code))
		} else if lazy := cfg.GenesisLazyCode[addr]; lazy != nil {
			// Made per account on the encode workers, not held here.
			entry.lazyCode = lazy
			stats.CodeBytes += uint64(lazy.Size)
		}
		if stor := cfg.GenesisStorage[addr]; len(stor) > 0 {
			entry.Storage = stor
			stats.StorageSlotsCreated += len(stor)
			stats.StorageBytes += uint64(len(stor)) * 64
		}
		foundational.Spec[addr] = entry
		if entry.hasCode() {
			stats.ContractsCreated++
		} else {
			stats.AccountsCreated++
		}
	}

	stats.TotalBytes = stats.StorageBytes + stats.CodeBytes
	return foundational, stats, nil
}
