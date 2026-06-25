// Package manifest writes a state-actor-manifest.json into a generated
// datadir capturing everything needed to reproduce the run: the exact
// command, the resolved flags (note: the *resolved* seed and fork, not the
// raw inputs — see Flags), any embedded --spec config, the state-actor
// build, and the run's result. It is a leaf package: it imports only the
// standard library so any caller can populate and write it.
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
)

// FileName is the manifest dropped at the datadir root.
const FileName = "state-actor-manifest.json"

// SchemaVersion is bumped on any breaking change to the manifest shape so
// consumers can detect format drift.
const SchemaVersion = 1

// Manifest is the full reproducibility record for one state-actor run.
type Manifest struct {
	SchemaVersion int    `json:"schema_version"`
	StateActor    Build  `json:"state_actor"`
	GeneratedAt   string `json:"generated_at"`
	// Command is os.Args as the process was invoked. argv[0] is the launcher
	// path (e.g. a go-run temp binary or ./state-actor), not a canonicalized
	// program name.
	Command []string  `json:"command"`
	Flags   Flags     `json:"flags"`
	Spec    *SpecFile `json:"spec,omitempty"`
	Result  *Result   `json:"result,omitempty"`
	// ReproducedFrom is set when this run was produced by the `reproduce`
	// subcommand; it holds the manifest path that was replayed. Empty for an
	// original run. (Command stays the literal reproduce invocation, so this is
	// what links a reproduced datadir back to its source manifest.)
	ReproducedFrom string `json:"reproduced_from,omitempty"`
}

// Build identifies the binary that produced the run. Version comes from the
// -X main.Version ldflag; the VCS fields and GoVersion come from the embedded
// build info, so a binary built outside the Makefile (e.g. inside a client
// Docker image) still records its provenance when built from a VCS checkout.
type Build struct {
	Version     string `json:"version"`
	GoVersion   string `json:"go_version"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	VCSRevision string `json:"vcs_revision,omitempty"`
	VCSTime     string `json:"vcs_time,omitempty"`
	// VCSModified is emitted even when false: for a provenance record, "built
	// from a clean tree" (false) must be distinguishable from "unknown".
	VCSModified bool `json:"vcs_modified"`
}

// Flags is the resolved configuration. Seed and Fork hold the values actually
// used, which may differ from the command line: --seed=0 expands to a
// wall-clock seed, and an empty --fork resolves to the client's maximum fork.
// ForkInput / SeedInput preserve what the user passed for auditing.
type Flags struct {
	Client     string `json:"client"`
	DB         string `json:"db"`
	Seed       int64  `json:"seed"`
	SeedInput  int64  `json:"seed_input"`
	Fork       string `json:"fork"`
	ForkInput  string `json:"fork_input"`
	ChainID    int64  `json:"chain_id"`
	GasLimit   uint64 `json:"gas_limit"`
	Timestamp  uint64 `json:"timestamp"`
	ExtraData  string `json:"extra_data,omitempty"`
	TargetSize string `json:"target_size,omitempty"`
	BinaryTrie bool   `json:"binary_trie"`
	GroupDepth int    `json:"group_depth"`
	Archive    bool   `json:"archive"`
	SpecPath   string `json:"spec_path,omitempty"`
}

// SpecFile references the --spec config used for the run. The raw spec is
// written verbatim to a content-addressed sidecar (OutputFile) next to the
// manifest rather than inlined, so the manifest stays readable and the spec is
// directly reusable: --spec=<OutputFile>. SHA256 names the sidecar and lets a
// consumer verify it. InputPath records the original --spec path for auditing.
type SpecFile struct {
	InputPath  string `json:"input_path"`
	SHA256     string `json:"sha256"`
	OutputFile string `json:"output_file"`
}

// SpecFilePrefix + <sha256> + ".yaml" is the content-addressed sidecar name.
const SpecFilePrefix = "state-actor-spec-"

// Result records the run's output for verification: a reproduction should
// land on the same StateRoot. The counters are always emitted (no omitempty)
// so a legitimate zero is distinguishable from an omitted field.
type Result struct {
	StateRoot        string `json:"state_root"`
	AccountsCreated  uint64 `json:"accounts_created"`
	ContractsCreated uint64 `json:"contracts_created"`
	StorageSlots     uint64 `json:"storage_slots"`
	TotalDBSizeBytes uint64 `json:"total_db_size_bytes"`
	ElapsedMS        int64  `json:"elapsed_ms"`
}

// NewBuild assembles the Build record from the linked-in version and the
// embedded build info.
func NewBuild(version string) Build {
	b := Build{
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				b.VCSRevision = s.Value
			case "vcs.time":
				b.VCSTime = s.Value
			case "vcs.modified":
				b.VCSModified = s.Value == "true"
			}
		}
	}
	b.Version = resolveVersion(version, b.VCSRevision, b.VCSModified)
	return b
}

// resolveVersion picks the version string. An explicit -X main.Version (e.g. a
// release tag passed via the Docker build-arg) always wins. Otherwise it falls
// back to the short commit that Go automatically embeds for `go build` /
// `go install` from a git checkout — so Docker builds without the build-arg
// still record a meaningful, reproducible version (the .git is in the build
// context and git is installed in the builder images). Note: `go run` does NOT
// embed VCS info, so an un-stamped `go run` reports "dev"; likewise when there
// is no VCS info at all.
func resolveVersion(version, revision string, modified bool) string {
	if version != "" && version != "dev" {
		return version
	}
	if revision != "" {
		short := revision
		if len(short) > 12 {
			short = short[:12]
		}
		if modified {
			short += "-dirty"
		}
		return short
	}
	return "dev"
}

// WriteSpecSidecar reads the --spec file at inputPath, writes it verbatim to a
// content-addressed sidecar (<dir>/state-actor-spec-<sha256>.yaml), and returns
// a SpecFile referencing it. Returns nil when inputPath is empty (no --spec).
// The sidecar is byte-identical to the input so its sha matches the filename.
func WriteSpecSidecar(dir, inputPath string) (*SpecFile, error) {
	if inputPath == "" {
		return nil, nil
	}
	data, err := os.ReadFile(inputPath)
	if err != nil {
		return nil, fmt.Errorf("manifest: read spec %q: %w", inputPath, err)
	}
	sum := sha256.Sum256(data)
	hexSum := hex.EncodeToString(sum[:])
	outFile := SpecFilePrefix + hexSum + ".yaml"
	if err := os.WriteFile(filepath.Join(dir, outFile), data, 0o644); err != nil {
		return nil, fmt.Errorf("manifest: write spec sidecar: %w", err)
	}
	return &SpecFile{
		InputPath:  inputPath,
		SHA256:     hexSum,
		OutputFile: outFile,
	}, nil
}

// Verify recomputes the sha256 of the sidecar file (OutputFile, located in dir)
// and checks it against the recorded SHA256. This guards a reproduction against
// a tampered or corrupted spec sidecar before it is replayed: without it, a
// changed sidecar would silently alter the reproduced state and surface only as
// a confusing state-root mismatch.
func (s *SpecFile) Verify(dir string) error {
	path := filepath.Join(dir, s.OutputFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("manifest: read spec sidecar %q: %w", path, err)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != s.SHA256 {
		return fmt.Errorf("manifest: spec sidecar %q sha256 mismatch: recorded %s, got %s", path, s.SHA256, got)
	}
	return nil
}

// Load reads and parses a manifest JSON file (used by the reproduce path).
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("manifest: read %q: %w", path, err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("manifest: parse %q: %w", path, err)
	}
	// Refuse a schema this binary cannot safely consume: a newer (breaking)
	// schema may rename/move fields we'd otherwise silently read as zero, and a
	// missing/zero version means the file is not a v1 manifest at all.
	if m.SchemaVersion == 0 || m.SchemaVersion > SchemaVersion {
		return nil, fmt.Errorf("manifest: unsupported schema_version %d in %q (this binary supports %d)", m.SchemaVersion, path, SchemaVersion)
	}
	return &m, nil
}

// Write marshals the manifest and writes it to <dir>/state-actor-manifest.json,
// returning the path written.
func (m *Manifest) Write(dir string) (string, error) {
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", fmt.Errorf("manifest: marshal: %w", err)
	}
	outPath := filepath.Join(dir, FileName)
	if err := os.WriteFile(outPath, append(out, '\n'), 0o644); err != nil {
		return "", fmt.Errorf("manifest: write %q: %w", outPath, err)
	}
	return outPath, nil
}
