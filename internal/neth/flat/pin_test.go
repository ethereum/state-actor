package flat

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/ethereum/state-actor/internal/neth"
)

// repoRoot walks up from the test working directory to the module root
// (the directory containing go.mod), so the pin-consistency test is robust
// to where `go test` is invoked from.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from cwd")
		}
		dir = parent
	}
}

var nethImageRE = regexp.MustCompile(`nethermind/nethermind:[0-9]+\.[0-9]+\.[0-9]+`)

// TestNethermindImagePinConsistency guards against the multi-site pin drift
// that used to require editing five files by hand: it asserts every
// `nethermind/nethermind:<ver>` reference in the operational boot files equals
// neth.PinnedNethermindVersion, and that the docs at least reference the pinned
// version.
func TestNethermindImagePinConsistency(t *testing.T) {
	root := repoRoot(t)
	want := "nethermind/nethermind:" + neth.PinnedNethermindVersion

	// Boot-critical files: every occurrence MUST equal the pin.
	strict := []string{
		"Makefile",
		"scripts/run-bloatnet.sh",
		"client/nethermind/testdata/validate-big-db.sh",
	}
	for _, f := range strict {
		data, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Errorf("read %s: %v", f, err)
			continue
		}
		found := nethImageRE.FindAllString(string(data), -1)
		if len(found) == 0 {
			t.Errorf("%s: no nethermind/nethermind:<ver> pin found", f)
		}
		for _, m := range found {
			if m != want {
				t.Errorf("%s: stale/mismatched pin %q, want %q", f, m, want)
			}
		}
	}

	// Docs: must reference the pinned version at least once.
	docs := []string{
		"client/nethermind/testdata/README.md",
		"docs/RUNBOOK.md",
	}
	for _, f := range docs {
		data, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Errorf("read %s: %v", f, err)
			continue
		}
		found := nethImageRE.FindAllString(string(data), -1)
		hasWant := false
		for _, m := range found {
			if m == want {
				hasWant = true
			}
		}
		if !hasWant {
			t.Errorf("%s: does not reference %q", f, want)
		}
	}
}
