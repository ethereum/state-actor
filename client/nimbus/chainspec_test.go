package nimbus

import (
	"encoding/json"
	"math/big"
	"testing"

	"github.com/ethereum/state-actor/genesis"
)

func decodeSidecar(t *testing.T, fork string) map[string]any {
	t.Helper()
	g, err := genesis.BuildSynthetic(fork, big.NewInt(1337), 60_000_000, 1_700_000_000, []byte{0xde, 0xad})
	if err != nil {
		t.Fatalf("BuildSynthetic: %v", err)
	}
	payload, err := BuildGenesisJSON(g)
	if err != nil {
		t.Fatalf("BuildGenesisJSON: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(payload, &doc); err != nil {
		t.Fatalf("sidecar is not JSON: %v\n%s", err, payload)
	}
	return doc
}

// The nimbus loader's field rules: fork times are numbers, the genesis
// timestamp is a string, mixHash is camelCase, alloc is empty, and the blob
// schedule carries osaka only when osakaTime is scheduled.
func TestGenesisSidecarShape(t *testing.T) {
	doc := decodeSidecar(t, "osaka")
	cfg := doc["config"].(map[string]any)
	for _, k := range []string{"shanghaiTime", "cancunTime", "pragueTime", "osakaTime"} {
		if _, ok := cfg[k].(float64); !ok {
			t.Fatalf("%s must be a JSON number, got %#v", k, cfg[k])
		}
	}
	if cfg["chainId"].(float64) != 1337 {
		t.Fatalf("chainId %v", cfg["chainId"])
	}
	if ts, ok := doc["timestamp"].(string); !ok || ts != "0x6553f100" {
		t.Fatalf("timestamp %#v", doc["timestamp"])
	}
	if _, ok := doc["mixHash"]; !ok {
		t.Fatal("mixHash key missing")
	}
	if alloc, ok := doc["alloc"].(map[string]any); !ok || len(alloc) != 0 {
		t.Fatalf("alloc %#v", doc["alloc"])
	}
	bs := cfg["blobSchedule"].(map[string]any)
	for _, k := range []string{"cancun", "prague", "osaka"} {
		if _, ok := bs[k]; !ok {
			t.Fatalf("blobSchedule.%s missing", k)
		}
	}
	if doc["extraData"] != "0xdead" || doc["gasLimit"] != "0x3938700" {
		t.Fatalf("extraData %v gasLimit %v", doc["extraData"], doc["gasLimit"])
	}
	if _, ok := cfg["terminalTotalDifficultyPassed"]; ok {
		t.Fatal("terminalTotalDifficultyPassed should not be emitted")
	}
}

func TestGenesisSidecarPragueOmitsOsaka(t *testing.T) {
	doc := decodeSidecar(t, "prague")
	cfg := doc["config"].(map[string]any)
	if _, ok := cfg["osakaTime"]; ok {
		t.Fatal("osakaTime emitted for a prague genesis")
	}
	bs := cfg["blobSchedule"].(map[string]any)
	if _, ok := bs["osaka"]; ok {
		t.Fatal("blobSchedule.osaka emitted for a prague genesis")
	}
	if _, ok := cfg["pragueTime"].(float64); !ok {
		t.Fatal("pragueTime missing")
	}
}

func TestStoreDir(t *testing.T) {
	if got := StoreDir("/data"); got != "/data/ecdb" {
		t.Fatalf("StoreDir = %s", got)
	}
}
