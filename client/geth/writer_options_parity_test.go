package geth

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble"
	gethpebble "github.com/ethereum/go-ethereum/ethdb/pebble"
)

// TestPebbleLevelOptionsMatchGeth locks state-actor's per-level Pebble
// table-format options to go-ethereum's own ethdb/pebble configuration.
//
// The per-level options (bloom filter policy, target file size, block
// size, compression) decide the persistent sstable format of the DB we
// hand to geth. Rather than trusting a hand-copied gethLevelOptions()
// to stay in sync with upstream, this test opens one throwaway DB via
// geth's ethdb/pebble.New and one via prodPebbleOptions(), then
// compares the [Level "N"] sections Pebble records in each DB's
// OPTIONS-* file. If a go-ethereum bump changes its level config in
// any observable way, this fails and points at gethLevelOptions().
//
// Runtime-only knobs (memtable sizing, compaction thresholds, cache)
// live in the [Options] section, which is deliberately NOT compared:
// state-actor legitimately tunes those for one-shot bulk import, and
// they leave no trace in the sstables geth later reads.
//
// Known blind spot: pebble serializes filter_policy by NAME only
// ("rocksdb.BuiltinBloomFilter"), not bits-per-key — a geth change
// from bloom.FilterPolicy(10) to another bit count would pass this
// test. There is no observable place the bit count is recorded, so
// that dimension can only be caught by reviewing geth bumps.
func TestPebbleLevelOptionsMatchGeth(t *testing.T) {
	gethDir := t.TempDir()
	gdb, err := gethpebble.New(gethDir, 16, 16, "parity-test", false)
	if err != nil {
		t.Fatalf("open geth ethdb/pebble: %v", err)
	}
	if err := gdb.Close(); err != nil {
		t.Fatalf("close geth ethdb/pebble: %v", err)
	}

	saDir := t.TempDir()
	sdb, err := pebble.Open(saDir, prodPebbleOptions())
	if err != nil {
		t.Fatalf("open state-actor pebble: %v", err)
	}
	if err := sdb.Close(); err != nil {
		t.Fatalf("close state-actor pebble: %v", err)
	}

	gethLevels := levelSections(t, gethDir)
	saLevels := levelSections(t, saDir)
	if gethLevels != saLevels {
		t.Errorf("per-level Pebble options diverge from geth's ethdb/pebble\n--- geth ---\n%s\n--- state-actor ---\n%s", gethLevels, saLevels)
	}
}

// levelSections returns the concatenated [Level "N"] sections of the
// newest OPTIONS-* file in a Pebble DB directory. Lexicographic sort
// is enough to pick "newest" here: a freshly created DB has exactly
// one OPTIONS file.
func levelSections(t *testing.T, dir string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "OPTIONS-*"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("no OPTIONS-* file in %s (err=%v)", dir, err)
	}
	sort.Strings(matches)
	raw, err := os.ReadFile(matches[len(matches)-1])
	if err != nil {
		t.Fatalf("read %s: %v", matches[len(matches)-1], err)
	}

	var out strings.Builder
	inLevel := false
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			inLevel = strings.HasPrefix(trimmed, `[Level`)
		}
		if inLevel && trimmed != "" {
			out.WriteString(trimmed)
			out.WriteString("\n")
		}
	}
	if out.Len() == 0 {
		t.Fatalf("no [Level ...] sections found in %s", matches[len(matches)-1])
	}
	return out.String()
}
