// Package main provides a tool for generating realistic Ethereum state
// in a Pebble database compatible with go-ethereum.
//
// The tool writes state directly to the snapshot layer, which is the
// authoritative source for state in modern geth. The trie can be
// regenerated from snapshots on node startup.
//
// When a genesis file is provided, the tool:
// 1. Includes genesis alloc accounts in state generation
// 2. Computes the combined state root
// 3. Writes the genesis block with the correct state root
// 4. Produces a database ready to use without `geth init`
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/nerolation/state-actor/client/besu"
	"github.com/nerolation/state-actor/client/geth"
	"github.com/nerolation/state-actor/client/nethermind"
	"github.com/nerolation/state-actor/client/reth"
	"github.com/nerolation/state-actor/generator"
	"github.com/nerolation/state-actor/genesis"
)

var (
	dbPath         = flag.String("db", "", "Path to the database directory (required)")
	accounts       = flag.Int("accounts", 1000, "Number of EOA accounts to create")
	contracts      = flag.Int("contracts", 100, "Number of contracts to create")
	maxSlots       = flag.Int("max-slots", 10000, "Maximum storage slots per contract")
	minSlots       = flag.Int("min-slots", 1, "Minimum storage slots per contract")
	distribution   = flag.String("distribution", "power-law", "Storage distribution: 'power-law', 'uniform', or 'exponential'")
	seed           = flag.Int64("seed", 0, "Random seed (0 = use current time)")
	batchSize      = flag.Int("batch-size", 100000, "Database batch size. For --client=reth: per-batch generation size in the streaming Phase 4 (each batch is generated, written to MDBX, RLP-keyed by AddrHash into a temp Pebble sorter, then dropped — Phase 4 RAM stays bounded by one batch + Pebble's 64 MiB buffer regardless of total N).")
	workers        = flag.Int("workers", 0, "Number of parallel workers (0 = NumCPU)")
	codeSize       = flag.Int("code-size", 1024, "Average contract code size in bytes")
	verbose        = flag.Bool("verbose", false, "Verbose output")
	benchmark      = flag.Bool("benchmark", false, "Run in benchmark mode (print detailed stats)")
	binaryTrie     = flag.Bool("binary-trie", false, "Generate state for binary trie mode (EIP-7864)")
	commitInterval = flag.Int("commit-interval", 500000, "Binary trie: commit to disk every N trie insertions (default 500K, 0 = all in-memory)")

	// Target size
	targetSize = flag.String("target-size", "", "Target total DB size on disk (e.g. '5GB', '500MB'). Stops generating when estimated size is reached.")

	// Genesis integration
	genesisPath    = flag.String("genesis", "", "Path to genesis.json file (optional)")
	injectAccounts = flag.String("inject-accounts", "", "Comma-separated hex addresses to inject with 999999999 ETH (e.g. 0xf39F...2266)")
	chainID        = flag.Int64("chain-id", 0, "Override genesis chainId (0 = use value from genesis.json)")

	// Binary trie group depth
	groupDepth      = flag.Int("group-depth", 8, "Binary trie group depth (1-8, default 8). Controls serialization unit size.")
	pebbleBlockSize = flag.Int("pebble-block-size", 4096, "PebbleDB SSTable block size in bytes (default 4096)")

	// Stats server
	statsPort = flag.Int("stats-port", 0, "Port for live stats HTTP server (0 = disabled)")

	// Client selection (multi-client support). Each client uses its own
	// self-contained machinery inside client/<name>/; only the CLI is shared.
	client = flag.String("client", "geth", "Target Ethereum client: 'geth' (default), 'nethermind', 'besu', or 'reth'. Other clients (erigon) are planned in follow-up PRs.")
)

func main() {
	flag.Parse()

	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "Error: -db flag is required")
		flag.Usage()
		os.Exit(1)
	}

	if *workers == 0 {
		*workers = runtime.NumCPU()
	}

	if *seed == 0 {
		*seed = time.Now().UnixNano()
	}

	// Validate --client value and its compatibility with other flags. Doing
	// this at CLI parse time (before any generation work) means misconfigured
	// runs fail fast instead of burning minutes producing a wrong output.
	switch *client {
	case "geth", "nethermind", "besu", "reth":
		// supported
	case "erigon":
		log.Fatalf("--client=%s is not yet implemented (planned in a follow-up PR); use --client=geth, --client=nethermind, --client=besu, or --client=reth", *client)
	default:
		log.Fatalf("--client=%s is not recognized; valid values: geth, nethermind, besu, reth", *client)
	}
	if *client == "nethermind" {
		// Nethermind doesn't implement EIP-7864 (binary trie) and the
		// commit-interval / group-depth / pebble-block-size flags are all
		// geth/Pebble-specific. Reject up front.
		if *binaryTrie {
			log.Fatalf("--binary-trie is not supported with --client=nethermind (Nethermind does not implement EIP-7864)")
		}
	}
	if *client == "besu" {
		// Besu doesn't implement EIP-7864 (binary trie). Reject up front.
		// --chain-id is warn-and-ignored inside client/besu/run_cgo.go
		// (Besu reads chainId from --genesis-file at boot, not from the DB).
		if *binaryTrie {
			log.Fatalf("--binary-trie is not supported with --client=besu (Besu does not implement EIP-7864)")
		}
	}
	if *client == "reth" {
		// Reth doesn't implement EIP-7864; surface the mismatch here rather
		// than letting reth init-state fail opaquely later.
		if *binaryTrie {
			log.Fatalf("--binary-trie is not supported with --client=reth (Reth does not implement EIP-7864)")
		}
		if *targetSize != "" {
			log.Fatalf("--target-size is not yet supported with --client=reth; set --accounts / --contracts explicitly")
		}
	}

	trieMode := generator.TrieModeMPT
	if *binaryTrie {
		trieMode = generator.TrieModeBinary
	}

	// Parse --inject-accounts
	var injectAddrs []common.Address
	if *injectAccounts != "" {
		for _, s := range strings.Split(*injectAccounts, ",") {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			if !common.IsHexAddress(s) {
				log.Fatalf("Invalid address in --inject-accounts: %q", s)
			}
			injectAddrs = append(injectAddrs, common.HexToAddress(s))
		}
	}

	// Parse --target-size
	var parsedTargetSize uint64
	if *targetSize != "" {
		var err error
		parsedTargetSize, err = parseSize(*targetSize)
		if err != nil {
			log.Fatalf("Invalid --target-size: %v", err)
		}
	}

	// Start stats server if requested
	var statsServer *generator.StatsServer
	var liveStats *generator.LiveStats
	if *statsPort > 0 {
		statsServer = generator.NewStatsServer(*statsPort)
		liveStats = statsServer.Stats()
		liveStats.SetConfig(*accounts, *contracts, *distribution, *seed)
		if err := statsServer.Start(); err != nil {
			log.Fatalf("Failed to start stats server: %v", err)
		}
		log.Printf("Stats server running on http://localhost:%d", *statsPort)
		defer statsServer.Stop()
	}

	// When --target-size is set, auto-scale ALL parameters holistically
	// using a 70/20/10 entry ratio (storage / accounts / code).
	// Two independent inputs:
	//   - 70/20/10 ratio: macro-level split of entry types
	//   - min/max-slots + distribution: micro-level per-contract shape
	// Contracts is the derived "solver" variable that reconciles both.
	// Explicit flags always override auto-computed values.
	if parsedTargetSize > 0 {
		// bytesPerEntry is used only by auto-scaling to shape
		// --accounts/--contracts/--slots defaults; the actual stop
		// condition is driven by the Phase 1/2 SizeTracker or dirSize check,
		// so this constant does not need to be exact — 130 matches the
		// observed GB-scale bintrie density (22.4M entries → 2.9 GB).
		const bytesPerEntry uint64 = 130
		totalEntries := parsedTargetSize / bytesPerEntry
		chunksPerContract := uint64(((*codeSize) + 30) / 31)

		// --- Auto-scale accounts (20% of entries) ---
		if !isFlagSet("accounts") {
			autoAccounts := int(totalEntries * 20 / 100)
			if autoAccounts > *accounts {
				*accounts = autoAccounts
			}
		}

		// --- Compute contracts as solver variable ---
		// Contracts must satisfy two constraints:
		//   1. Code budget:    contracts × chunksPerContract ≈ 10% of entries
		//   2. Storage budget: contracts × avgSlots ≈ 70% of entries
		// Take the larger to satisfy both; then adjust minSlots if needed.
		storageEntries := totalEntries * 70 / 100

		// Compute avgSlots from current min/max-slots and distribution.
		// Power-law (alpha=1.5) mean ≈ 3×min; uniform mean = (min+max)/2.
		avgSlots := uint64(*minSlots) * 3 // power-law default
		if isFlagSet("distribution") {
			switch *distribution {
			case "uniform":
				avgSlots = uint64(*minSlots+*maxSlots) / 2
			case "exponential":
				avgSlots = uint64(*maxSlots) / 4
			}
		}
		if avgSlots == 0 {
			avgSlots = 1
		}

		contractsFromCode := totalEntries * 10 / 100 / max(chunksPerContract, 1)
		contractsFromStorage := storageEntries / avgSlots

		userContracts := int(max(contractsFromCode, contractsFromStorage))
		if userContracts < 100 {
			userContracts = 100
		}

		// If user explicitly set --contracts, respect it but warn if dangerous.
		if isFlagSet("contracts") {
			userContracts = *contracts
			if !isFlagSet("min-slots") {
				autoMin := int(storageEntries / uint64(userContracts) / 3)
				if autoMin > 500_000 {
					log.Printf("WARNING: %d contracts with %s target → ~%dM min-slots/contract (~%.1fGB RAM each).",
						userContracts, *targetSize, autoMin/1_000_000,
						float64(autoMin)*64/1e9)
					log.Printf("  Consider: --contracts %d or let auto-scaling choose.",
						max(int(storageEntries/500_000/3), 1000))
				}
			}
		}

		// --- Auto-scale min-slots from storage budget ---
		if !isFlagSet("min-slots") {
			actualAvgSlots := storageEntries / uint64(userContracts)
			autoMin := int(actualAvgSlots / 3) // power-law mean ≈ 3×min
			if isFlagSet("distribution") {
				switch *distribution {
				case "uniform":
					autoMin = int(actualAvgSlots)
				case "exponential":
					autoMin = int(actualAvgSlots / 4)
				}
			}
			if autoMin > *minSlots {
				*minSlots = autoMin
			}
		}

		// --- Auto-scale max-slots ---
		if !isFlagSet("max-slots") && *maxSlots < *minSlots*10 {
			*maxSlots = *minSlots * 10
		}

		// Log computed parameters.
		log.Printf("Auto-scaled for %s target (70/20/10 ratio):", *targetSize)
		log.Printf("  accounts:     %d", *accounts)
		log.Printf("  contracts:    %d (soft cap, stops earlier at target)", userContracts)
		log.Printf("  min-slots:    %d", *minSlots)
		log.Printf("  max-slots:    %d", *maxSlots)
		log.Printf("  code-size:    %d", *codeSize)
		log.Printf("  distribution: %s", *distribution)

		// Phase 1 safety cap: the SizeTracker / dirSize check is the
		// real stop condition, but a generous finite cap protects against
		// the stop check misbehaving (e.g. Pebble silently not growing
		// the mainDB). autoScaleEstimate × 5 gives plenty of headroom for
		// workloads where compression or overhead happens to push the
		// actual stop point later than the estimate; if the cap is ever
		// hit in practice, a warning at end-of-generation flags it.
		*contracts = userContracts * 5
	}

	config := generator.Config{
		DBPath:          *dbPath,
		NumAccounts:     *accounts,
		NumContracts:    *contracts,
		MaxSlots:        *maxSlots,
		MinSlots:        *minSlots,
		Distribution:    generator.ParseDistribution(*distribution),
		Seed:            *seed,
		BatchSize:       *batchSize,
		Workers:         *workers,
		CodeSize:        *codeSize,
		Verbose:         *verbose,
		TrieMode:        trieMode,
		CommitInterval:  *commitInterval,
		WriteTrieNodes:  true, // Always write trie nodes — DB is unusable without them
		InjectAddresses: injectAddrs,
		TargetSize:      parsedTargetSize,
		LiveStats:       liveStats,
		GroupDepth:      *groupDepth,
		PebbleBlockSize: *pebbleBlockSize,
	}

	// Load genesis if provided
	var genesisConfig *genesis.Genesis
	if *genesisPath != "" {
		var err error
		genesisConfig, err = genesis.LoadGenesis(*genesisPath)
		if err != nil {
			log.Fatalf("Failed to load genesis: %v", err)
		}

		// Extract accounts from genesis alloc
		config.GenesisAccounts = genesisConfig.ToStateAccounts()
		config.GenesisStorage = genesisConfig.GetAllocStorage()
		config.GenesisCode = genesisConfig.GetAllocCode()

		// Override chain ID if requested
		if *chainID != 0 {
			genesisConfig.Config.ChainID = big.NewInt(*chainID)
		}

		if *verbose {
			log.Printf("Loaded genesis with %d alloc accounts (chainId=%s)",
				len(config.GenesisAccounts), genesisConfig.Config.ChainID)
		}
	}

	if *verbose {
		log.Printf("Configuration:")
		log.Printf("  Database:     %s", config.DBPath)
		log.Printf("  Accounts:     %d", config.NumAccounts)
		if parsedTargetSize > 0 {
			log.Printf("  Contracts:    %d (Phase 1 safety cap, target-size stops earlier)", config.NumContracts)
		} else {
			log.Printf("  Contracts:    %d", config.NumContracts)
		}
		if parsedTargetSize > 0 && !isFlagSet("max-slots") {
			log.Printf("  Max Slots:    %d (auto-scaled for target size)", config.MaxSlots)
		} else {
			log.Printf("  Max Slots:    %d", config.MaxSlots)
		}
		if parsedTargetSize > 0 && !isFlagSet("min-slots") {
			log.Printf("  Min Slots:    %d (auto-scaled for target size)", config.MinSlots)
		} else {
			log.Printf("  Min Slots:    %d", config.MinSlots)
		}
		log.Printf("  Distribution: %s", *distribution)
		log.Printf("  Seed:         %d", config.Seed)
		log.Printf("  Batch Size:   %d", config.BatchSize)
		log.Printf("  Workers:      %d", config.Workers)
		log.Printf("  Code Size:    %d bytes", config.CodeSize)
		log.Printf("  Trie Mode:    %s", config.TrieMode)
		if config.CommitInterval > 0 {
			log.Printf("  Commit Interval: %d trie insertions", config.CommitInterval)
		}
		if config.GroupDepth > 0 {
			log.Printf("  Group Depth:     %d", config.GroupDepth)
		}
		log.Printf("  Pebble Block:    %d bytes", config.PebbleBlockSize)
		if config.TargetSize > 0 {
			log.Printf("  Target Size:  %s", formatBytes(config.TargetSize))
		}
		if *genesisPath != "" {
			log.Printf("  Genesis:      %s", *genesisPath)
		}
	}

	start := time.Now()

	// Dispatch to the selected client's machinery. Each client owns its full
	// pipeline (writer, trie, genesis) inside client/<name>/; main.go only
	// decides who runs. The stats return shape is intentionally identical so
	// the summary prints below work uniformly for any client.
	var stats *generator.Stats
	switch *client {
	case "geth":
		// MPT mode goes through the new direct-Pebble pipeline in
		// client/geth/ (entitygen → temp Pebble → keccak-sorted writes
		// to production). Binary-trie mode still routes through the
		// legacy generator.New().Generate() path because
		// generator/binary_stack_trie.go is intentionally untouched per
		// the design doc.
		if config.TrieMode == generator.TrieModeMPT {
			geth.GenesisFilePath = *genesisPath
			geth.ChainIDOverride = *chainID
			var err error
			stats, err = geth.Populate(context.Background(), config, geth.Options{})
			if err != nil {
				log.Fatalf("Failed to populate Geth DB: %v", err)
			}
			if liveStats != nil && stats != nil {
				liveStats.AddBytes(int64(stats.AccountBytes), int64(stats.StorageBytes), int64(stats.CodeBytes))
				liveStats.SetStateRoot(stats.StateRoot.Hex())
			}
		} else {
			gen, err := generator.New(config)
			if err != nil {
				log.Fatalf("Failed to create generator: %v", err)
			}
			defer gen.Close()

			stats, err = gen.Generate()
			if err != nil {
				log.Fatalf("Failed to generate state: %v", err)
			}

			// Update live stats with final state
			if liveStats != nil {
				liveStats.AddBytes(int64(stats.AccountBytes), int64(stats.StorageBytes), int64(stats.CodeBytes))
				liveStats.SetStateRoot(stats.StateRoot.Hex())
			}

			// Write genesis block if genesis was provided (binary-trie path).
			if genesisConfig != nil {
				if *verbose {
					log.Printf("Writing genesis block with state root: %s", stats.StateRoot.Hex())
				}

				ancientDir := filepath.Join(config.DBPath, "ancient")
				block, err := geth.WriteGenesisBlock(gen.DB(), genesisConfig, stats.StateRoot, true, ancientDir)
				if err != nil {
					log.Fatalf("Failed to write genesis block: %v", err)
				}

				if *verbose {
					log.Printf("Genesis block hash: %s", block.Hash().Hex())
					log.Printf("Genesis block number: %d", block.NumberU64())
				}
			}
		}

	case "nethermind":
		// Nethermind path: the writer in client/nethermind/ owns the full
		// pipeline (entitygen → trie.Builder → grocksdb). Phase A
		// (empty-alloc genesis only) lands behind the cgo_neth build tag;
		// vanilla local builds get a stub redirecting users at Docker.
		nethermind.GenesisFilePath = *genesisPath
		nethermind.ChainIDOverride = *chainID
		var err error
		stats, err = nethermind.Run(context.Background(), config, nethermind.Options{})
		if err != nil {
			log.Fatalf("Failed to populate Nethermind DB: %v", err)
		}
		if liveStats != nil && stats != nil {
			liveStats.SetStateRoot(stats.StateRoot.Hex())
		}

	case "besu":
		// Besu path: writer in client/besu/ owns the full pipeline
		// (entitygen → Phase 1 temp Pebble → Phase 2 sorted iter →
		// Bonsai trie.Builder → single grocksdb instance with 8 column
		// families). Behind the cgo_besu build tag; vanilla local builds
		// get a stub redirecting users at Docker.
		besu.GenesisFilePath = *genesisPath
		besu.ChainIDOverride = *chainID
		var err error
		stats, err = besu.Run(context.Background(), config, besu.Options{})
		if err != nil {
			log.Fatalf("Failed to populate Besu DB: %v", err)
		}
		if liveStats != nil && stats != nil {
			liveStats.SetStateRoot(stats.StateRoot.Hex())
		}

	case "reth":
		// Reth path writes a complete v2 datadir directly via mdbx-go +
		// grocksdb (cgo). Without -tags cgo_reth, RunCgo returns a clear
		// error pointing at Dockerfile.reth — local builds without
		// libmdbx/librocksdb cannot exercise this path.
		reth.GenesisFilePath = *genesisPath
		reth.ChainIDOverride = *chainID
		var err error
		stats, err = reth.RunCgo(context.Background(), config, reth.Options{})
		if err != nil {
			log.Fatalf("Failed to populate Reth DB: %v", err)
		}
		if liveStats != nil {
			liveStats.SetStateRoot(stats.StateRoot.Hex())
		}
	}

	elapsed := time.Since(start)

	fmt.Printf("\n=== State Generation Complete ===\n")
	fmt.Printf("Total Time:        %v\n", elapsed.Round(time.Millisecond))
	fmt.Printf("Accounts Created:  %d\n", stats.AccountsCreated)
	fmt.Printf("Contracts Created: %d\n", stats.ContractsCreated)
	// StorageSlotsCreated / TotalBytes / Throughput are populated by the
	// geth path; the nethermind path doesn't track them yet (writer
	// streams through grocksdb without accumulating per-slot counters).
	// Hide the rows when there's nothing to show rather than printing
	// misleading zeros.
	if stats.StorageSlotsCreated > 0 {
		fmt.Printf("Storage Slots:     %d\n", stats.StorageSlotsCreated)
	}
	if stats.TotalBytes > 0 {
		fmt.Printf("Total Bytes:       %s\n", formatBytes(stats.TotalBytes))
	}
	if stats.TrieNodeBytes > 0 {
		fmt.Printf("Trie Node Bytes:   %s\n", formatBytes(stats.TrieNodeBytes))
	}
	if stats.StemBlobBytes > 0 {
		fmt.Printf("Stem Blob Bytes:   %s\n", formatBytes(stats.StemBlobBytes))
	}
	// Report actual on-disk size (after Pebble compression).
	if dbSize, err := dirSize(config.DBPath); err == nil {
		fmt.Printf("Total DB Size:     %s\n", formatBytes(dbSize))
	}
	if stats.StorageSlotsCreated > 0 {
		fmt.Printf("Throughput:        %.2f slots/sec\n", float64(stats.StorageSlotsCreated)/elapsed.Seconds())
	}
	fmt.Printf("State Root:        %s\n", stats.StateRoot.Hex())

	if genesisConfig != nil {
		fmt.Printf("Genesis:           included (ready to use without geth init)\n")
	}

	if *benchmark {
		fmt.Printf("\n=== Detailed Stats ===\n")
		fmt.Printf("Account Bytes:     %s\n", formatBytes(stats.AccountBytes))
		fmt.Printf("Storage Bytes:     %s\n", formatBytes(stats.StorageBytes))
		fmt.Printf("Code Bytes:        %s\n", formatBytes(stats.CodeBytes))
		fmt.Printf("DB Write Time:     %v\n", stats.DBWriteTime.Round(time.Millisecond))
		fmt.Printf("Generation Time:   %v\n", stats.GenerationTime.Round(time.Millisecond))
		if len(config.GenesisAccounts) > 0 {
			fmt.Printf("Genesis Accounts:  %d\n", len(config.GenesisAccounts))
		}

		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		fmt.Printf("\n=== Memory Stats ===\n")
		fmt.Printf("Total Alloc:       %s\n", formatBytes(m.TotalAlloc))
		fmt.Printf("Current Alloc:     %s\n", formatBytes(m.Alloc))
		fmt.Printf("Sys Memory:        %s\n", formatBytes(m.Sys))
	}

	// Print sample addresses for verification
	if len(stats.SampleEOAs) > 0 {
		fmt.Printf("\n=== Sample Addresses (for verification) ===\n")
		for i, addr := range stats.SampleEOAs {
			fmt.Printf("  EOA #%d:      %s\n", i+1, addr.Hex())
		}
		for i, addr := range stats.SampleContracts {
			fmt.Printf("  Contract #%d: %s\n", i+1, addr.Hex())
		}
	}

	// Keep stats server running after completion if enabled
	if statsServer != nil {
		fmt.Printf("\n=== Stats server still running at http://localhost:%d ===\n", *statsPort)
		fmt.Printf("Press Ctrl+C to exit...\n")
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		<-sigCh
		fmt.Println("\nShutting down...")
	}
}

func formatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// parseSize parses a human-readable size string (e.g. "5GB", "500MB", "1TB")
// into bytes. Supports KB, MB, GB, TB suffixes (case-insensitive, base-1024).
func parseSize(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	upper := strings.ToUpper(s)

	suffixes := []struct {
		suffix string
		mult   uint64
	}{
		{"TB", 1 << 40},
		{"GB", 1 << 30},
		{"MB", 1 << 20},
		{"KB", 1 << 10},
	}

	for _, sf := range suffixes {
		if strings.HasSuffix(upper, sf.suffix) {
			numStr := strings.TrimSpace(s[:len(s)-len(sf.suffix)])
			val, err := strconv.ParseFloat(numStr, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid number %q in size %q", numStr, s)
			}
			if val <= 0 {
				return 0, fmt.Errorf("size must be positive: %s", s)
			}
			return uint64(val * float64(sf.mult)), nil
		}
	}

	// Plain number = bytes
	val, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size format %q (use e.g. '5GB', '500MB')", s)
	}
	return val, nil
}

// isFlagSet returns true if the named flag was explicitly set on the command line.
func isFlagSet(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// dirSize returns the total size of all files in a directory tree.
func dirSize(path string) (uint64, error) {
	var total uint64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += uint64(info.Size())
		}
		return nil
	})
	return total, err
}
