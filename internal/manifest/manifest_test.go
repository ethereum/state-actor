package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewBuildRecordsRuntime(t *testing.T) {
	b := NewBuild("v1.2.3")
	assert.Equal(t, "v1.2.3", b.Version) // explicit version always wins
	assert.Equal(t, runtime.Version(), b.GoVersion)
	assert.Equal(t, runtime.GOOS, b.OS)
	assert.Equal(t, runtime.GOARCH, b.Arch)
}

func TestResolveVersion(t *testing.T) {
	cases := []struct {
		name     string
		version  string
		revision string
		modified bool
		want     string
	}{
		{"explicit tag wins", "v1.2.3", "a2099cf6dc853cea", false, "v1.2.3"},
		{"explicit wins over dirty vcs", "v1.2.3", "a2099cf6dc853cea", true, "v1.2.3"},
		{"fallback short sha", "", "abcdef0123456789", false, "abcdef012345"},
		{"fallback short sha dirty", "dev", "a2099cf6dc853cea", true, "a2099cf6dc85-dirty"},
		{"no version no vcs", "", "", false, "dev"},
		{"dev no vcs", "dev", "", false, "dev"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, resolveVersion(c.version, c.revision, c.modified))
		})
	}
}

func TestWriteSpecSidecarEmptyPath(t *testing.T) {
	s, err := WriteSpecSidecar(t.TempDir(), "")
	require.NoError(t, err)
	assert.Nil(t, s)
}

func TestWriteSpecSidecarWritesContentAddressedFile(t *testing.T) {
	srcDir := t.TempDir()
	outDir := t.TempDir()
	inputPath := filepath.Join(srcDir, "spec.yaml")
	content := []byte("entities:\n  - kind: eoa\n")
	require.NoError(t, os.WriteFile(inputPath, content, 0o644))

	s, err := WriteSpecSidecar(outDir, inputPath)
	require.NoError(t, err)
	require.NotNil(t, s)

	want := sha256.Sum256(content)
	wantHex := hex.EncodeToString(want[:])
	assert.Equal(t, inputPath, s.InputPath)
	assert.Equal(t, wantHex, s.SHA256)
	assert.Equal(t, SpecFilePrefix+wantHex+".yaml", s.OutputFile)

	// Sidecar is written verbatim next to the manifest (bare relative name).
	assert.NotContains(t, s.OutputFile, "/")
	written, err := os.ReadFile(filepath.Join(outDir, s.OutputFile))
	require.NoError(t, err)
	assert.Equal(t, content, written)
}

func TestWriteSpecSidecarMissingFile(t *testing.T) {
	_, err := WriteSpecSidecar(t.TempDir(), filepath.Join(t.TempDir(), "nope.yaml"))
	assert.Error(t, err)
}

func TestWriteThenLoad(t *testing.T) {
	dir := t.TempDir()
	m := &Manifest{
		SchemaVersion: SchemaVersion,
		StateActor:    NewBuild("v7"),
		Command:       []string{"state-actor", "--client", "reth"},
		Flags:         Flags{Client: "reth", Seed: 99, Fork: "prague"},
		Result:        &Result{StateRoot: "0xdead"},
	}
	path, err := m.Write(dir)
	require.NoError(t, err)

	got, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "v7", got.StateActor.Version)
	assert.Equal(t, "reth", got.Flags.Client)
	assert.Equal(t, int64(99), got.Flags.Seed)
	assert.Equal(t, "prague", got.Flags.Fork)
	assert.Equal(t, "0xdead", got.Result.StateRoot)
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	assert.Error(t, err)
}

func TestWriteRoundTrips(t *testing.T) {
	dir := t.TempDir()
	m := &Manifest{
		SchemaVersion: SchemaVersion,
		StateActor:    NewBuild("v9"),
		GeneratedAt:   "2026-06-25T10:00:00Z",
		Command:       []string{"state-actor", "--client", "geth"},
		Flags: Flags{
			Client:    "geth",
			DB:        "/tmp/x/geth/chaindata",
			Seed:      42,
			SeedInput: 0,
			Fork:      "osaka",
			ChainID:   1337,
			GasLimit:  30_000_000,
		},
		Result: &Result{
			StateRoot:       "0xabc",
			AccountsCreated: 10,
			ElapsedMS:       1234,
		},
	}

	path, err := m.Write(dir)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, FileName), path)

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	var got Manifest
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, SchemaVersion, got.SchemaVersion)
	assert.Equal(t, int64(42), got.Flags.Seed)
	assert.Equal(t, int64(0), got.Flags.SeedInput)
	assert.Equal(t, "osaka", got.Flags.Fork)
	assert.Equal(t, "0xabc", got.Result.StateRoot)
	// Spec omitted → must not appear.
	assert.NotContains(t, string(data), "\"spec\"")
}

// TestLoadRejectsUnsupportedSchema pins the schema-compatibility guard in Load:
// a missing/zero version (not a v1 manifest) and a newer version (potentially
// breaking) must both be refused rather than silently mis-read.
func TestLoadRejectsUnsupportedSchema(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
		return p
	}

	// schema_version 0 / missing.
	_, err := Load(write("zero.json", `{"schema_version":0}`))
	assert.Error(t, err)
	_, err = Load(write("missing.json", `{"command":["state-actor"]}`))
	assert.Error(t, err)

	// schema_version newer than this binary supports.
	_, err = Load(write("future.json", `{"schema_version":999}`))
	assert.Error(t, err)

	// Current schema loads fine.
	p, err := (&Manifest{SchemaVersion: SchemaVersion, StateActor: NewBuild("v1"), Result: &Result{StateRoot: "0x1"}}).Write(dir)
	require.NoError(t, err)
	_, err = Load(p)
	assert.NoError(t, err)
}

// TestSpecFileVerify covers the content-address check used before a reproduce
// replays a spec sidecar: the matching file passes, a tampered file and a
// missing file both error.
func TestSpecFileVerify(t *testing.T) {
	srcDir, outDir := t.TempDir(), t.TempDir()
	inputPath := filepath.Join(srcDir, "spec.yaml")
	require.NoError(t, os.WriteFile(inputPath, []byte("entities:\n  - kind: eoa\n"), 0o644))

	s, err := WriteSpecSidecar(outDir, inputPath)
	require.NoError(t, err)
	require.NotNil(t, s)

	// Matching sidecar verifies.
	require.NoError(t, s.Verify(outDir))

	// Tampered sidecar fails.
	require.NoError(t, os.WriteFile(filepath.Join(outDir, s.OutputFile), []byte("tampered\n"), 0o644))
	assert.Error(t, s.Verify(outDir))

	// Missing sidecar fails.
	require.NoError(t, os.Remove(filepath.Join(outDir, s.OutputFile)))
	assert.Error(t, s.Verify(outDir))
}

// TestManifestFlagsRoundTripAllFields is the drift guard for Flags: every field
// is set to a distinct non-zero value and must survive Write→Load unchanged.
// The reflection sweep fails if a newly added Flags field is left unset here,
// forcing whoever adds it to also confirm reproduce() restores it (otherwise a
// new state-affecting flag would silently break reproducibility).
func TestManifestFlagsRoundTripAllFields(t *testing.T) {
	flags := Flags{
		Client:     "geth",
		DB:         "/data/geth/chaindata",
		Seed:       42,
		SeedInput:  7,
		Fork:       "osaka",
		ForkInput:  "prague",
		ChainID:    1337,
		GasLimit:   30_000_000,
		Timestamp:  1_700_000_000,
		ExtraData:  "0xdeadbeef",
		TargetSize: "2GB",
		BinaryTrie: true,
		GroupDepth: 8,
		Archive:    true,
		SpecPath:   "/in/spec.yaml",
	}
	rv := reflect.ValueOf(flags)
	for i := range rv.NumField() {
		if rv.Field(i).IsZero() {
			t.Fatalf("Flags.%s is zero in this fixture — set it, and make sure reproduce() restores it from the manifest",
				rv.Type().Field(i).Name)
		}
	}

	dir := t.TempDir()
	m := &Manifest{SchemaVersion: SchemaVersion, StateActor: NewBuild("v1"), Flags: flags, Result: &Result{StateRoot: "0x1"}}
	path, err := m.Write(dir)
	require.NoError(t, err)

	got, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, flags, got.Flags)
}
