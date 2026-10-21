package erigon

import (
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	stategenesis "github.com/ethereum/state-actor/genesis"
)

// TestWriteGenesisJSON_AmsterdamTime pins the fork key that `erigon init`
// reads: chain.Config.AmsterdamTime has the JSON tag "amsterdamTime" on
// the pinned erigon. An Osaka genesis must not carry the key.
//
// The header fields that Amsterdam adds (blockAccessListHash, slotNumber)
// stay out of genesis.json. erigon's GenesisWithoutStateToBlock sets
// them to the empty-list hash and 0 when they are absent, the same
// values that internal/genesisheader.Build writes.
func TestWriteGenesisJSON_AmsterdamTime(t *testing.T) {
	for _, tc := range []struct {
		fork          string
		wantAmsterdam bool
	}{
		{"osaka", false},
		{"amsterdam", true},
	} {
		t.Run(tc.fork, func(t *testing.T) {
			g, err := stategenesis.BuildSynthetic(tc.fork, big.NewInt(1337), 60_000_000,
				1_700_000_000, []byte{0xde, 0xad, 0xbe, 0xef})
			if err != nil {
				t.Fatalf("BuildSynthetic(%s): %v", tc.fork, err)
			}
			path := filepath.Join(t.TempDir(), "genesis.json")
			if err := writeGenesisJSON(g, path, nil); err != nil {
				t.Fatalf("writeGenesisJSON: %v", err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read genesis.json: %v", err)
			}
			var top, config map[string]json.RawMessage
			if err := json.Unmarshal(raw, &top); err != nil {
				t.Fatalf("decode genesis.json: %v", err)
			}
			if err := json.Unmarshal(top["config"], &config); err != nil {
				t.Fatalf("decode genesis.json config: %v", err)
			}

			if string(config["osakaTime"]) != "0" {
				t.Errorf("config.osakaTime = %s; want 0", config["osakaTime"])
			}
			got, ok := config["amsterdamTime"]
			switch {
			case tc.wantAmsterdam && string(got) != "0":
				t.Errorf("config.amsterdamTime = %s; want 0", got)
			case !tc.wantAmsterdam && ok:
				t.Errorf("config.amsterdamTime = %s; want absent", got)
			}
			for _, key := range []string{"blockAccessListHash", "slotNumber"} {
				if v, ok := top[key]; ok {
					t.Errorf("genesis.json has %q = %s; want absent (erigon sets the default)", key, v)
				}
			}
		})
	}
}
