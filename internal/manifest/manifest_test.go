package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
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
