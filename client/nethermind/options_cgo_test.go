//go:build cgo_neth

package nethermind

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestNethOptionsPersisted opens the full Nethermind DB set and asserts
// the composed DbConfig strings were accepted and applied, via the
// effective per-CF configuration RocksDB records in each OPTIONS-* file.
func TestNethOptionsPersisted(t *testing.T) {
	dir := t.TempDir()
	dbs, err := openNethDBs(dir)
	if err != nil {
		t.Fatalf("openNethDBs: %v", err)
	}
	dbs.Close()

	type check struct {
		section string // OPTIONS-file section header prefix
		want    string // line that must appear inside it
	}
	cases := map[string][]check{ // key = DB subdir
		dbNameState: {
			{`[CFOptions "default"]`, "optimize_filters_for_hits=true"},
			{`[CFOptions "default"]`, "compression=kLZ4Compression"},
			{`[CFOptions "default"]`, "level_compaction_dynamic_level_bytes=false"},
			{`[CFOptions "default"]`, "max_bytes_for_level_multiplier=30.000000"},
			{`[TableOptions/BlockBasedTable "default"]`, "block_size=32000"},
			{`[TableOptions/BlockBasedTable "default"]`, "index_type=kTwoLevelIndexSearch"},
		},
		dbNameCode: {
			{`[TableOptions/BlockBasedTable "default"]`, "filter_policy=nullptr"},
			{`[TableOptions/BlockBasedTable "default"]`, "index_type=kHashSearch"},
			{`[CFOptions "default"]`, "optimize_filters_for_hits=false"},
		},
		dbNameBlocks: {
			{`[CFOptions "default"]`, "optimize_filters_for_hits=false"},
			{`[CFOptions "default"]`, "compaction_pri=kOldestLargestSeqFirst"},
		},
		dbNameFlat: {
			{`[CFOptions "Account"]`, "compression=kNoCompression"},
			{`[CFOptions "Account"]`, "optimize_filters_for_hits=false"},
			{`[TableOptions/BlockBasedTable "Account"]`, "block_size=4096"},
			{`[CFOptions "StateNodes"]`, "level_compaction_dynamic_level_bytes=true"},
			{`[TableOptions/BlockBasedTable "Storage"]`, "block_size=8000"},
			{`[TableOptions/BlockBasedTable "StateTopNodes"]`, "index_type=kBinarySearch"},
		},
	}

	for db, checks := range cases {
		sections := optionsFileSections(t, filepath.Join(dir, db))
		for _, c := range checks {
			body, ok := sections[c.section]
			if !ok {
				t.Errorf("%s: OPTIONS file has no section %s (have: %v)", db, c.section, sectionNames(sections))
				continue
			}
			if !strings.Contains(body, c.want+"\n") && !strings.HasSuffix(body, c.want) {
				t.Errorf("%s %s: missing %q", db, c.section, c.want)
			}
		}
		// Every state/flat table must have SOME filter — the exact policy
		// serialization varies across RocksDB versions, so assert non-null.
		if db == dbNameState || db == dbNameFlat {
			for name, body := range sections {
				if !strings.HasPrefix(name, `[TableOptions/BlockBasedTable`) {
					continue
				}
				for _, line := range strings.Split(body, "\n") {
					line = strings.TrimSpace(line)
					if strings.HasPrefix(line, "filter_policy=") && strings.Contains(line, "nullptr") {
						t.Errorf("%s %s: filter_policy is nullptr, expected a real filter", db, name)
					}
				}
			}
		}
	}
}

// optionsFileSections parses the newest OPTIONS-* file of a RocksDB dir into
// section-header → body.
func optionsFileSections(t *testing.T, dir string) map[string]string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "OPTIONS-*"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("no OPTIONS-* in %s (err=%v)", dir, err)
	}
	sort.Strings(matches)
	raw, err := os.ReadFile(matches[len(matches)-1])
	if err != nil {
		t.Fatal(err)
	}
	sections := make(map[string]string)
	var current string
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			current = trimmed
			continue
		}
		if current != "" && trimmed != "" {
			sections[current] += trimmed + "\n"
		}
	}
	return sections
}

func sectionNames(m map[string]string) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
