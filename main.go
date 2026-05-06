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
	"github.com/nerolation/state-actor/internal/clientpolicy"
)

var (
	dbPath       = flag.String("db", "", "Path to the database directory (required)")
	accounts     = flag.Int("accounts", 1000, "Number of EOA accounts to create")
	contracts    = flag.Int("contracts", 100, "Number of contracts to create")
	maxSlots     = flag.Int("max-slots", 10000, "Maximum storage slots per contract")
	minSlots     = flag.Int("min-slots", 1, "Minimum storage slots per contract")
	distribution = flag.String("distribution", "power-law", "Storage distribution: 'power-law', 'uniform', or 'exponential'")
	seed         = flag.Int64("seed", 1, "Random seed (deterministic; default 1). Pass --seed=0 to use the current wall-clock time (NON-reproducible).")
	batchSize    = flag.Int("batch-size", 100000, "Database batch size. For --client=reth: per-batch generation size in the streaming Phase 4 (each batch is generated, written to MDBX, RLP-keyed by AddrHash into a temp Pebble sorter, then dropped — Phase 4 RAM stays bounded by one batch + Pebble's 64 MiB buffer regardless of total N).")
	codeSize     = flag.Int("code-size", 1024, "Average contract code size in bytes")
	verbose      = flag.Bool("verbose", false, "Verbose output")
	benchmark    = flag.Bool("benchmark", false, "Run in benchmark mode (print detailed stats)")
	binaryTrie   = flag.Bool("binary-trie", false, "Generate state for binary trie mode (EIP-7864)")

	// Deep-branch accounts
	deepBranchAccounts   = flag.Int("deep-branch-accounts", 0, "Number of additional contracts with deep storage tries (0 = disabled)")
	deepBranchDepth      = flag.Int("deep-branch-depth", 64, "Branch depth per deep slot in nibbles (1-64)")
	deepBranchKnownSlots = flag.Int("deep-branch-known-slots", 1, "Legitimate storage slots with known preimages per deep-branch account")

	// Target size
	targetSize = flag.String("target-size", "", "Target total DB size on disk (e.g. '5GB', '500MB'). Stop condition only — set --accounts/--contracts/--min-slots/--max-slots explicitly. Honored by geth and besu; ignored by nethermind; rejected by reth.")

	// Synthetic genesis configuration. state-actor builds the genesis
	// itself — no --genesis path. The four header knobs users actually
	// vary are exposed as flags below; everything else takes a sensible
	// default (Difficulty=0, Coinbase=0x0, Mixhash=0x0, Alloc empty).
	fork           = flag.String("fork", "", "Hard fork active at genesis. Empty (default) resolves to the latest fork the chosen --client can write. Use --list-forks to see all values.")
	listForks      = flag.Bool("list-forks", false, "Print the list of accepted --fork values and exit.")
	injectAccounts = flag.String("inject-accounts", "", "Comma-separated hex addresses to inject with 999999999 ETH (e.g. 0xf39F...2266). Use this instead of a --genesis alloc.")
	chainID        = flag.Int64("chain-id", 1337, "Chain ID embedded in the synthesized genesis chainspec (default 1337, the devnet convention).")
	gasLimit       = flag.Uint64("gas-limit", 30_000_000, "Genesis block gas limit (default 30M).")
	timestamp      = flag.Uint64("timestamp", 0, "Genesis block timestamp (unix seconds, default 0).")
	extraData      = flag.String("extra-data", "", "Genesis block extraData as hex (default empty).")

	// Binary trie group depth
	groupDepth = flag.Int("group-depth", 8, "Binary trie group depth (1-8, default 8). Controls serialization unit size.")

	// Stats server
	statsPort = flag.Int("stats-port", 0, "Port for live stats HTTP server (0 = disabled)")

	// Client selection (multi-client support). Each client uses its own
	// self-contained machinery inside client/<name>/; only the CLI is shared.
	client = flag.String("client", "geth", "Target Ethereum client: 'geth' (default), 'nethermind', 'besu', or 'reth'. Other clients (erigon) are planned in follow-up PRs.")
)

func main() {
	flag.Parse()

	if *listForks {
		fmt.Println("Supported --fork values (default = latest):")
		for _, f := range genesis.SortedForks() {
			fmt.Printf("  %s\n", f)
		}
		os.Exit(0)
	}

	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "Error: -db flag is required")
		flag.Usage()
		os.Exit(1)
	}

	if *seed == 0 {
		*seed = time.Now().UnixNano()
	}

	// Validate --client value and its compatibility with other flags. Doing
	// this at CLI parse time (before any generation work) means misconfigured
	// runs fail fast instead of burning minutes producing a wrong output.
	// Rules live in internal/clientpolicy/ as one source-of-truth table.
	if err := clientpolicy.ValidateForClient(*client, clientpolicy.FlagValues{
		BinaryTrie:         *binaryTrie,
		DeepBranchAccounts: *deepBranchAccounts,
		TargetSize:         *targetSize,
		Fork:               *fork,
	}); err != nil {
		log.Fatalf("%v", err)
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

	// --target-size is a stop condition, not an auto-scaler. The previous
	// auto-scaling block (which silently rewrote --accounts/--contracts/
	// --min-slots/--max-slots and multiplied --contracts by 5) was removed;
	// users now set per-entity flags explicitly. Geth honours the stop in
	// generator.SizeTracker / dirSize; besu honours it in Phase 1's
	// raw-byte cap; nethermind currently no-ops; reth rejects the flag at
	// parse time.

	// Validate deep-branch flags
	if *deepBranchAccounts > 0 {
		if *deepBranchDepth < 1 || *deepBranchDepth > 64 {
			log.Fatalf("--deep-branch-depth must be 1-64, got %d", *deepBranchDepth)
		}
		if *deepBranchKnownSlots < 1 {
			log.Fatalf("--deep-branch-known-slots must be >= 1, got %d", *deepBranchKnownSlots)
		}
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
		Workers:         runtime.NumCPU(),
		CodeSize:        *codeSize,
		Verbose:         *verbose,
		TrieMode:        trieMode,
		CommitInterval:  500_000,
		WriteTrieNodes:  true, // Always write trie nodes — DB is unusable without them
		InjectAddresses: injectAddrs,
		TargetSize:      parsedTargetSize,
		DeepBranch: generator.DeepBranchConfig{
			NumAccounts: *deepBranchAccounts,
			Depth:       *deepBranchDepth,
			KnownSlots:  *deepBranchKnownSlots,
		},
		LiveStats:  liveStats,
		GroupDepth: *groupDepth,
	}

	// Synthesize the genesis chainspec from CLI flags. state-actor no
	// longer accepts an external --genesis file; the four header knobs
	// users actually vary (--chain-id / --fork / --gas-limit /
	// --timestamp / --extra-data) drive an in-memory *Genesis here, and
	// each client's writer reads it via config.Genesis.
	extraDataBytes := []byte{}
	if *extraData != "" {
		decoded, err := decodeHex(*extraData)
		if err != nil {
			log.Fatalf("--extra-data must be hex (with or without 0x prefix): %v", err)
		}
		extraDataBytes = decoded
	}
	// Empty --fork resolves to the per-client ceiling (auto). Explicit values
	// past the ceiling were rejected by ValidateForClient above.
	chosenFork := *fork
	if chosenFork == "" {
		chosenFork = genesis.MaxForkForClient(*client)
	}
	genesisConfig, err := genesis.BuildSynthetic(chosenFork, big.NewInt(*chainID), *gasLimit, *timestamp, extraDataBytes)
	if err != nil {
		log.Fatalf("--fork %q: %v", chosenFork, err)
	}
	config.Genesis = genesisConfig

	if *verbose {
		log.Printf("Synthesized genesis: fork=%s chainID=%s gasLimit=%d timestamp=%d extraData=%dB",
			chosenFork, genesisConfig.Config.ChainID, uint64(genesisConfig.GasLimit), uint64(genesisConfig.Timestamp), len(genesisConfig.ExtraData))
	}

	if *verbose {
		log.Printf("Configuration:")
		log.Printf("  Database:     %s", config.DBPath)
		log.Printf("  Accounts:     %d", config.NumAccounts)
		log.Printf("  Contracts:    %d", config.NumContracts)
		log.Printf("  Max Slots:    %d", config.MaxSlots)
		log.Printf("  Min Slots:    %d", config.MinSlots)
		log.Printf("  Distribution: %s", *distribution)
		log.Printf("  Seed:         %d", config.Seed)
		log.Printf("  Batch Size:   %d", config.BatchSize)
		log.Printf("  Code Size:    %d bytes", config.CodeSize)
		log.Printf("  Trie Mode:    %s", config.TrieMode)
		if config.GroupDepth > 0 {
			log.Printf("  Group Depth:     %d", config.GroupDepth)
		}
		if config.TargetSize > 0 {
			log.Printf("  Target Size:  %s", formatBytes(config.TargetSize))
		}
		if config.DeepBranch.Enabled() {
			log.Printf("  Deep Branch:  %d accounts, depth=%d, known_slots=%d",
				config.DeepBranch.NumAccounts, config.DeepBranch.Depth, config.DeepBranch.KnownSlots)
		}
		log.Printf("  Fork:         %s", chosenFork)
		log.Printf("  Chain ID:     %d", *chainID)
		log.Printf("  Gas Limit:    %d", *gasLimit)
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
		//
		// Both paths read the synthesized genesisConfig — main.go's
		// BuildSynthetic call always populates it, replacing the old
		// --genesis JSON flow. config.Genesis is the canonical surface;
		// Populate reads it directly, and the binary path threads
		// genesisConfig into WriteGenesisBlock explicitly.
		if config.TrieMode == generator.TrieModeMPT {
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

			if liveStats != nil {
				liveStats.AddBytes(int64(stats.AccountBytes), int64(stats.StorageBytes), int64(stats.CodeBytes))
				liveStats.SetStateRoot(stats.StateRoot.Hex())
			}

		// Write genesis block (geth-specific). Always runs now that the
		// synthesized config.Genesis is always present.
		if *verbose {
			log.Printf("Writing genesis block with state root: %s", stats.StateRoot.Hex())
		}
		ancientDir := filepath.Join(config.DBPath, "ancient")
		block, err := geth.WriteGenesisBlock(gen.DB(), genesisConfig, stats.StateRoot, config.TrieMode == generator.TrieModeBinary, ancientDir)
		if err != nil {
			log.Fatalf("Failed to write genesis block: %v", err)
		}
		if *verbose {
			log.Printf("Genesis block hash: %s", block.Hash().Hex())
			log.Printf("Genesis block number: %d", block.NumberU64())
			// Always write genesis block — synthesized config is always present.
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

	case "nethermind":
		// Nethermind path: writer in client/nethermind/ reads
		// config.Genesis directly (no package globals).
		var err error
		stats, err = nethermind.Run(context.Background(), config, nethermind.Options{})
		if err != nil {
			log.Fatalf("Failed to populate Nethermind DB: %v", err)
		}
		if liveStats != nil && stats != nil {
			liveStats.SetStateRoot(stats.StateRoot.Hex())
		}

	case "besu":
		// Besu path: same — writer reads config.Genesis directly.
		var err error
		stats, err = besu.Run(context.Background(), config, besu.Options{})
		if err != nil {
			log.Fatalf("Failed to populate Besu DB: %v", err)
		}
		if liveStats != nil && stats != nil {
			liveStats.SetStateRoot(stats.StateRoot.Hex())
		}

	case "reth":
		// Reth path: same — writer reads config.Genesis directly.
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

// decodeHex parses a 0x-prefixed-or-bare hex string into bytes.
func decodeHex(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		s = s[2:]
	}
	if s == "" {
		return []byte{}, nil
	}
	out := make([]byte, len(s)/2)
	if len(s)%2 != 0 {
		return nil, fmt.Errorf("odd-length hex string %q", s)
	}
	for i := 0; i < len(out); i++ {
		var b byte
		for j := 0; j < 2; j++ {
			c := s[i*2+j]
			b <<= 4
			switch {
			case c >= '0' && c <= '9':
				b |= c - '0'
			case c >= 'a' && c <= 'f':
				b |= c - 'a' + 10
			case c >= 'A' && c <= 'F':
				b |= c - 'A' + 10
			default:
				return nil, fmt.Errorf("invalid hex char %q at offset %d", c, i*2+j)
			}
		}
		out[i] = b
	}
	return out, nil
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
