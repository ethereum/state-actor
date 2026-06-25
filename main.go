// Package main is the state-actor CLI: generates Ethereum client
// databases (geth / besu / nethermind / reth / ethrex / erigon) end-to-end without
// going through the client binary's init path.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/state-actor/client/besu"
	"github.com/ethereum/state-actor/client/erigon"
	clientethrex "github.com/ethereum/state-actor/client/ethrex"
	"github.com/ethereum/state-actor/client/geth"
	"github.com/ethereum/state-actor/client/nethermind"
	"github.com/ethereum/state-actor/client/reth"
	"github.com/ethereum/state-actor/generator"
	"github.com/ethereum/state-actor/genesis"
	"github.com/ethereum/state-actor/internal/autofill"
	"github.com/ethereum/state-actor/internal/clientpolicy"
	"github.com/ethereum/state-actor/internal/manifest"
	"github.com/ethereum/state-actor/internal/progress"
	"github.com/ethereum/state-actor/internal/sizecal"
	"github.com/ethereum/state-actor/internal/spec"
	"github.com/ethereum/state-actor/internal/specbuild"
	"github.com/ethereum/state-actor/internal/syscontracts"
	"github.com/ethereum/state-actor/internal/templates"
)

// Version is the state-actor build version, injected via
// -ldflags "-X main.Version=..." (see Makefile). Defaults to "dev" for
// `go run` / un-stamped builds. Recorded in the run manifest.
var Version = "dev"

var (
	dbPath     = flag.String("db", "", "Path to the database directory (required)")
	seed       = flag.Int64("seed", 1, "Random seed (deterministic; default 1). Pass --seed=0 to use the current wall-clock time (NON-reproducible).")
	verbose    = flag.Bool("verbose", false, "Verbose output")
	benchmark  = flag.Bool("benchmark", false, "Run in benchmark mode (print detailed stats)")
	binaryTrie = flag.Bool("binary-trie", false, "Generate state for binary trie mode (EIP-7864)")

	targetSize = flag.String("target-size", "", "Advisory budget (e.g. '5GB', '500MB') that sizes the auto-fill of 20/10/70 mainnet-shaped synthetic state. Required unless --spec is set. With --spec, fills the headroom after the spec's projected cost; if the spec already meets the target, no auto-fill runs. Not a hard on-disk cap — actual size may vary per client. Honored by geth, besu, nethermind, reth, ethrex, and erigon.")

	fork      = flag.String("fork", "", "Hard fork active at genesis. Empty (default) resolves to the latest fork the chosen --client can write. Use --list-forks to see all values.")
	listForks = flag.Bool("list-forks", false, "Print the list of accepted --fork values and exit.")
	chainID   = flag.Int64("chain-id", 1337, "Chain ID embedded in the synthesized genesis chainspec (default 1337, the devnet convention).")

	specFile  = flag.String("spec", "", "Path to YAML state-spec file. See docs/SPEC.md for the schema.")
	gasLimit  = flag.Uint64("gas-limit", 30_000_000, "Genesis block gas limit (default 30M).")
	timestamp = flag.Uint64("timestamp", 0, "Genesis block timestamp (unix seconds, default 0).")
	extraData = flag.String("extra-data", "", "Genesis block extraData as hex (default empty).")

	groupDepth = flag.Int("group-depth", 8, "Binary trie group depth (1-8, default 8). Controls serialization unit size.")

	client = flag.String("client", "geth", "Target Ethereum client: 'geth' (default), 'nethermind', 'besu', 'reth', 'ethrex', or 'erigon'.")

	archive = flag.Bool("archive", false, "Configure the generated DB for archive-mode operation.\n"+
		"  reth: writes StoragesHistory + AccountsHistory + StorageChangeSets + AccountChangeSets at genesis.\n"+
		"  geth: writes PathDB archive-anchor metadata for --gcmode=archive boots.\n"+
		"Rejected for besu and nethermind (no archive code path).")
)

func main() {
	// Subcommands are dispatched on the first positional arg before the global
	// flags are parsed. `reproduce` regenerates a run from its manifest.
	if len(os.Args) >= 2 && os.Args[1] == "reproduce" {
		reproduce(os.Args[2:])
		return
	}

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

	if *specFile == "" && *targetSize == "" {
		fmt.Fprintln(os.Stderr, "Error: --target-size is required when --spec is not set (e.g. --target-size=5GB)")
		flag.Usage()
		os.Exit(1)
	}

	generate("")
}

// generate runs the full state-generation pipeline from the resolved global
// flags, prints the summary, writes the manifest, and returns the run stats.
// Shared by the default command and the `reproduce` subcommand, which populates
// the same globals from a manifest before calling it. reproducedFrom is the
// source manifest path when invoked via `reproduce` (recorded in the new
// manifest's reproduced_from), and "" for an original run.
func generate(reproducedFrom string) *generator.Stats {
	// Defense-in-depth: main() enforces this for the default command, but
	// reproduce() calls generate() directly. With neither --spec nor
	// --target-size there is nothing to emit but the injected system contracts
	// — a degenerate state that the empty-state guard in Config.Validate does
	// NOT catch (those contracts populate GenesisAccounts). Fail loudly instead.
	if *specFile == "" && *targetSize == "" {
		log.Fatalf("generate: neither --spec nor --target-size is set; nothing to generate")
	}
	// seedInput preserves the raw --seed for the manifest; *seed below is
	// resolved to a concrete value (wall-clock when 0) and is what actually
	// reproduces the run.
	seedInput := *seed
	if *seed == 0 {
		*seed = time.Now().UnixNano()
	}

	if err := clientpolicy.ValidateForClient(*client, clientpolicy.FlagValues{
		BinaryTrie: *binaryTrie,
		TargetSize: *targetSize,
		Fork:       *fork,
	}); err != nil {
		log.Fatalf("%v", err)
	}

	trieMode := generator.TrieModeMPT
	if *binaryTrie {
		trieMode = generator.TrieModeBinary
	}

	var parsedTargetSize uint64
	if *targetSize != "" {
		var err error
		parsedTargetSize, err = parseSize(*targetSize)
		if err != nil {
			log.Fatalf("Invalid --target-size: %v", err)
		}
		if parsedTargetSize == 0 {
			log.Fatalf("--target-size must be positive (got %q); set a positive value or omit the flag entirely",
				*targetSize)
		}
	}

	// Reject --archive for clients that have no archive code path.
	// Erigon accepts --archive but treats it as a no-op (the snapshot
	// tier is archive-by-design once history files ship; until then
	// archive-mode reads degrade gracefully to the value domains).
	if *archive {
		switch *client {
		case "geth", "reth", "erigon":
		default:
			log.Fatalf("--archive is only supported on geth, reth, and erigon (no-op for erigon); got --client=%s. Re-run without --archive.", *client)
		}
	}

	// ethrex shares the common fork ceiling (osaka per MaxForkForClient).
	// The chosenFork resolution below handles this uniformly.

	config := generator.Config{
		DBPath:         *dbPath,
		Seed:           *seed,
		Verbose:        *verbose,
		TrieMode:       trieMode,
		CommitInterval: 500_000,
		WriteTrieNodes: true, // PathDB requires trie nodes on disk
		TargetSize:     parsedTargetSize,
		GroupDepth:     *groupDepth,
		Archive:        *archive,
		// Always-on heartbeat: long runs would otherwise print nothing between
		// the startup banner and the final summary. Throttled internally.
		Progress: progress.New(),
	}

	extraDataBytes := []byte{}
	if *extraData != "" {
		decoded, err := decodeHex(*extraData)
		if err != nil {
			log.Fatalf("--extra-data must be hex (with or without 0x prefix): %v", err)
		}
		extraDataBytes = decoded
	}
	// Empty --fork resolves to the per-client ceiling.
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

	var specCost uint64
	if *specFile != "" {
		specDoc, err := spec.ParseFile(*specFile)
		if err != nil {
			log.Fatalf("--spec: %v", err)
		}
		validateRes, err := specDoc.Validate(templates.UserVisibleNames())
		if err != nil {
			log.Fatalf("--spec validate: %v", err)
		}
		for _, w := range validateRes.Warnings {
			log.Printf("--spec warning: %s", w)
		}
		buildOpts := specbuild.BuildOptions{
			Seed:       *seed,
			ClientName: *client,
			Sizer:      sizecal.Default(),
			TargetSize: config.TargetSize,
		}
		specCost = specbuild.ProjectedCost(specDoc, buildOpts)
		preAlloc, diag, err := specbuild.Build(specDoc, buildOpts)
		if err != nil {
			log.Fatalf("--spec build: %v", err)
		}
		for _, w := range diag.Warnings {
			log.Printf("--spec warning: %s", w)
		}
		config.PreAlloc = preAlloc
		if *verbose {
			log.Printf("--spec: loaded %d entities from %s (projected cost %s)", len(preAlloc), *specFile, formatBytes(specCost))
		}
	}

	// Build the auto-fill Plan for any remaining budget after the spec.
	if config.TargetSize > 0 {
		topUp := config.TargetSize
		if specCost < config.TargetSize {
			topUp = config.TargetSize - specCost
		} else {
			topUp = 0
		}
		if topUp > 0 {
			plan, err := autofill.PlanForBudget(topUp)
			if err != nil {
				log.Fatalf("auto-fill: %v", err)
			}
			config.AutoFill = plan
		} else if *verbose {
			log.Printf("auto-fill skipped: spec cost %s ≥ target_size %s", formatBytes(specCost), formatBytes(config.TargetSize))
		}
	}

	// Inject the 5 canonical mainnet system contracts (Cancun/Prague +
	// Deposit Contract) into cfg.GenesisAccounts / cfg.GenesisCode. Every
	// supported client reads these maps when composing the genesis state,
	// so this single call covers all 4 dispatch branches below. See
	// internal/syscontracts/syscontracts.go for the contract list and rationale.
	syscontracts.AddCanonicalSystemContracts(&config)

	if *verbose {
		log.Printf("Configuration:")
		log.Printf("  Database:     %s", config.DBPath)
		log.Printf("  Seed:         %d", config.Seed)
		log.Printf("  Trie Mode:    %s", config.TrieMode)
		if config.GroupDepth > 0 {
			log.Printf("  Group Depth:  %d", config.GroupDepth)
		}
		if config.TargetSize > 0 {
			log.Printf("  Target Size:  %s", formatBytes(config.TargetSize))
		}
		if config.AutoFill != nil {
			log.Printf("  Auto-fill:    %d EOAs / %d contracts (mainnet 20/10/70)",
				config.AutoFill.NumEOAs, config.AutoFill.NumContracts)
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
		// MPT mode → client/geth/.Populate (direct-Pebble pipeline).
		// Binary-trie mode → generator.New().Generate() + WriteGenesisBlock.
		if config.TrieMode == generator.TrieModeMPT {
			var err error
			stats, err = geth.Populate(context.Background(), config, geth.Options{})
			if err != nil {
				log.Fatalf("Failed to populate Geth DB: %v", err)
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

			// MPT path writes its own genesis block; binary path writes it here.
			if *verbose {
				log.Printf("Writing genesis block with state root: %s", stats.StateRoot.Hex())
			}
			ancientDir := filepath.Join(config.DBPath, "ancient")
			block, err := geth.WriteGenesisBlock(gen.DB(), genesisConfig, stats.StateRoot, true /* binaryTrie */, config.Archive, ancientDir)
			if err != nil {
				log.Fatalf("Failed to write genesis block: %v", err)
			}
			if *verbose {
				log.Printf("Genesis block hash: %s", block.Hash().Hex())
				log.Printf("Genesis block number: %d", block.NumberU64())
			}
		}

	case "nethermind":
		var err error
		stats, err = nethermind.Run(context.Background(), config, nethermind.Options{})
		if err != nil {
			log.Fatalf("Failed to populate Nethermind DB: %v", err)
		}

	case "besu":
		var err error
		stats, err = besu.Run(context.Background(), config, besu.Options{})
		if err != nil {
			log.Fatalf("Failed to populate Besu DB: %v", err)
		}

	case "reth":
		var err error
		stats, err = reth.RunCgo(context.Background(), config, reth.Options{})
		if err != nil {
			log.Fatalf("Failed to populate Reth DB: %v", err)
		}

	case "erigon":
		var err error
		stats, err = erigon.Run(context.Background(), config, erigon.Options{})
		if err != nil {
			log.Fatalf("Failed to populate Erigon DB: %v", err)
		}

	case "ethrex":
		var err error
		stats, err = clientethrex.Run(context.Background(), config, clientethrex.Options{})
		if err != nil {
			log.Fatalf("Failed to populate ethrex DB: %v", err)
		}
	}

	elapsed := time.Since(start)
	dbSize, dbSizeErr := dirSize(config.DBPath)

	fmt.Printf("\n=== State Generation Complete ===\n")
	fmt.Printf("Total Time:        %v\n", elapsed.Round(time.Millisecond))
	fmt.Printf("Accounts Created:  %d\n", stats.AccountsCreated)
	fmt.Printf("Contracts Created: %d\n", stats.ContractsCreated)
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
	if dbSizeErr == nil {
		fmt.Printf("Total DB Size:     %s\n", formatBytes(dbSize))
	}
	if stats.StorageSlotsCreated > 0 {
		fmt.Printf("Throughput:        %.2f slots/sec\n", float64(stats.StorageSlotsCreated)/elapsed.Seconds())
	}
	fmt.Printf("State Root:        %s\n", stats.StateRoot.Hex())

	if genesisConfig != nil {
		fmt.Printf("Genesis:           included (ready to use without geth init)\n")
	}

	// Write the reproducibility manifest to the datadir root. For geth that is
	// two levels up from --db (<datadir>/geth/chaindata → <datadir>), matching
	// where geth-genesis.json lands; for the other clients --db IS the datadir,
	// alongside their chainspec/genesis sidecars.
	manifestDir := *dbPath
	if *client == "geth" {
		manifestDir = geth.DatadirRoot(*dbPath)
	}
	// Manifest failures are warnings, not fatal: the DB is already fully
	// written and valid, so a missing manifest must not turn a successful
	// run into a non-zero exit.
	specFileEntry, err := manifest.WriteSpecSidecar(manifestDir, *specFile)
	if err != nil {
		// Non-fatal (the DB is valid), but a failed sidecar on a --spec run
		// means reproduce() — which reads the spec from the sidecar, not the
		// original path — cannot regenerate this run. Warn unmistakably.
		log.Printf("WARNING: manifest spec sidecar failed: %v\n"+
			"         this run is NOT reproducible from its manifest (the --spec sidecar is missing)", err)
	}
	man := &manifest.Manifest{
		SchemaVersion: manifest.SchemaVersion,
		StateActor:    manifest.NewBuild(Version),
		GeneratedAt:   start.UTC().Format(time.RFC3339),
		Command:       os.Args,
		Flags: manifest.Flags{
			Client:     *client,
			DB:         *dbPath,
			Seed:       *seed,
			SeedInput:  seedInput,
			Fork:       chosenFork,
			ForkInput:  *fork,
			ChainID:    *chainID,
			GasLimit:   *gasLimit,
			Timestamp:  *timestamp,
			ExtraData:  *extraData,
			TargetSize: *targetSize,
			BinaryTrie: *binaryTrie,
			GroupDepth: *groupDepth,
			Archive:    *archive,
			SpecPath:   *specFile,
		},
		Spec: specFileEntry,
		Result: &manifest.Result{
			StateRoot:        stats.StateRoot.Hex(),
			AccountsCreated:  uint64(stats.AccountsCreated),
			ContractsCreated: uint64(stats.ContractsCreated),
			StorageSlots:     uint64(stats.StorageSlotsCreated),
			ElapsedMS:        elapsed.Milliseconds(),
		},
		ReproducedFrom: reproducedFrom,
	}
	if dbSizeErr == nil {
		man.Result.TotalDBSizeBytes = dbSize
	}
	manifestPath, err := man.Write(manifestDir)
	if err != nil {
		log.Printf("warning: manifest write failed: %v", err)
	} else {
		fmt.Printf("Manifest:          %s\n", manifestPath)
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

	if len(stats.SampleEOAs) > 0 {
		fmt.Printf("\n=== Sample Addresses (for verification) ===\n")
		for i, addr := range stats.SampleEOAs {
			fmt.Printf("  EOA #%d:      %s\n", i+1, addr.Hex())
		}
		for i, addr := range stats.SampleContracts {
			fmt.Printf("  Contract #%d: %s\n", i+1, addr.Hex())
		}
	}

	return stats
}

// reproduce regenerates a prior run from its state-actor-manifest.json into a
// fresh --db, then verifies the resulting state root against the manifest
// (exiting non-zero on mismatch). It populates the same global flags the
// default command uses, so generation goes through the identical pipeline.
func reproduce(args []string) {
	fs := flag.NewFlagSet("reproduce", flag.ExitOnError)
	manifestPath := fs.String("manifest", "", "Path to the state-actor-manifest.json to reproduce (required)")
	outDB := fs.String("db", "", "Output database directory for the reproduced run (required; must differ from the original)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: state-actor reproduce --manifest <manifest.json> --db <new-output-dir>")
		fmt.Fprintln(os.Stderr, "Flags must follow the 'reproduce' subcommand.")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	if *manifestPath == "" || *outDB == "" {
		fs.Usage()
		os.Exit(1)
	}

	m, err := manifest.Load(*manifestPath)
	if err != nil {
		log.Fatalf("reproduce: %v", err)
	}

	// Compare the manifest's RESOLVED version against this binary's resolved
	// version — NewBuild applies the same go-build VCS fallback that produced
	// the recorded value, so an identical binary doesn't spuriously warn (the
	// raw Version var is "dev" for any un-stamped go build).
	localVersion := manifest.NewBuild(Version).Version
	if m.StateActor.Version != localVersion {
		log.Printf("warning: manifest was produced by state-actor %q but this binary is %q; reproduction may differ",
			m.StateActor.Version, localVersion)
	}

	// A --spec run whose sidecar failed to write (a warning at generation time)
	// records SpecPath but no Spec entry: it cannot be reproduced. Refuse
	// clearly rather than regenerating a spec-less (wrong) state → MISMATCH.
	if m.Spec == nil && m.Flags.SpecPath != "" {
		log.Fatalf("reproduce: manifest references --spec %q but has no spec sidecar (the original run failed to write it); this manifest is not reproducible", m.Flags.SpecPath)
	}

	// Never clobber the original datadir.
	if samePath(*outDB, m.Flags.DB) {
		log.Fatalf("reproduce: --db %q is the manifest's original datadir; choose a different output directory", *outDB)
	}
	// Require a fresh output dir: refuse a pre-existing non-empty directory so a
	// reproduction can't interleave with or overwrite unrelated client state.
	if dirExistsNonEmpty(*outDB) {
		log.Fatalf("reproduce: --db %q must be a fresh (empty or nonexistent) directory", *outDB)
	}

	// Populate the generation globals from the manifest's RESOLVED flags. The
	// concrete seed + fork are what make the run deterministic regardless of
	// when or where it is reproduced.
	*client = m.Flags.Client
	*seed = m.Flags.Seed
	*fork = m.Flags.Fork
	*chainID = m.Flags.ChainID
	*gasLimit = m.Flags.GasLimit
	*timestamp = m.Flags.Timestamp
	*extraData = m.Flags.ExtraData
	*targetSize = m.Flags.TargetSize
	*binaryTrie = m.Flags.BinaryTrie
	*groupDepth = m.Flags.GroupDepth
	*archive = m.Flags.Archive
	*dbPath = *outDB

	// Reproduce from the content-addressed spec sidecar next to the manifest —
	// guaranteed present and hash-named, unlike the original input path which
	// may not exist on this machine. Verify its sha256 first so a tampered or
	// corrupted sidecar fails fast instead of silently changing the result.
	manifestRoot := filepath.Dir(*manifestPath)
	if m.Spec != nil {
		if err := m.Spec.Verify(manifestRoot); err != nil {
			log.Fatalf("reproduce: %v", err)
		}
		*specFile = filepath.Join(manifestRoot, m.Spec.OutputFile)
	}

	fmt.Printf("Reproducing run from %s\n", *manifestPath)
	fmt.Printf("  client=%s seed=%d fork=%s → %s\n\n", *client, *seed, *fork, *dbPath)

	stats := generate(*manifestPath)

	// Fail-on-mismatch verification against the recorded state root. A valid
	// (schema-checked) manifest always records one; its absence means a corrupt
	// or hand-edited file, which must not silently pass a verification command.
	if m.Result == nil || m.Result.StateRoot == "" {
		log.Fatalf("reproduce: manifest recorded no state root to verify against (corrupt manifest?)")
	}
	got := stats.StateRoot.Hex()
	if got == m.Result.StateRoot {
		fmt.Printf("\nReproduction: PASS — state root matches %s\n", got)
		return
	}
	fmt.Printf("\nReproduction: MISMATCH\n  expected: %s\n  got:      %s\n", m.Result.StateRoot, got)
	os.Exit(1)
}

// samePath reports whether a and b resolve to the same filesystem location.
func samePath(a, b string) bool {
	aa, err1 := filepath.Abs(a)
	bb, err2 := filepath.Abs(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return aa == bb
}

// dirExistsNonEmpty reports whether path is an existing directory with at least
// one entry. A nonexistent path, an empty directory, or an unreadable/non-dir
// path all report false (treated as "not an obstacle" for a fresh reproduce).
func dirExistsNonEmpty(path string) bool {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false
	}
	return len(entries) > 0
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
	if val == 0 {
		return 0, fmt.Errorf("size must be positive: %s", s)
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
