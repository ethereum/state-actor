package nimbus

import (
	"encoding/json"
	"math/big"
	"reflect"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/ethereum/state-actor/genesis"
	"github.com/ethereum/state-actor/internal/genesisheader"
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

// An Amsterdam sidecar schedules amsterdamTime as a JSON number and gives
// amsterdam the Prague blob schedule, as go-ethereum v1.17.6 does. An Osaka
// sidecar must not carry either key.
func TestGenesisSidecarAmsterdam(t *testing.T) {
	doc := decodeSidecar(t, "amsterdam")
	cfg := doc["config"].(map[string]any)
	if v, ok := cfg["amsterdamTime"].(float64); !ok || v != 0 {
		t.Fatalf("amsterdamTime must be the JSON number 0, got %#v", cfg["amsterdamTime"])
	}
	if _, ok := cfg["osakaTime"].(float64); !ok {
		t.Fatal("osakaTime missing from an amsterdam sidecar")
	}
	bs := cfg["blobSchedule"].(map[string]any)
	want := map[string]any{"target": 6.0, "max": 9.0, "baseFeeUpdateFraction": 5007716.0}
	for _, k := range []string{"prague", "osaka", "amsterdam"} {
		if !reflect.DeepEqual(bs[k], want) {
			t.Fatalf("blobSchedule.%s = %#v, want %#v", k, bs[k], want)
		}
	}

	osaka := decodeSidecar(t, "osaka")["config"].(map[string]any)
	if _, ok := osaka["amsterdamTime"]; ok {
		t.Fatal("amsterdamTime emitted for an osaka genesis")
	}
	if _, ok := osaka["blobSchedule"].(map[string]any)["amsterdam"]; ok {
		t.Fatal("blobSchedule.amsterdam emitted for an osaka genesis")
	}
}

// The writer stores the header from internal/genesisheader.Build, and nimbus
// rebuilds block 0 from the sidecar. Both must give the same hash. The
// pinned hashes come from nimbus-eth1 v0.4.3 ("Writing genesis to DB"),
// booted on a fresh datadir with each sidecar (empty alloc, so the state root
// is the empty root). go-ethereum v1.17.6 gives the same hashes.
func TestGenesisHeaderMatchesNimbusRebuild(t *testing.T) {
	cases := []struct {
		fork string
		hash common.Hash
	}{
		{"osaka", common.HexToHash("0xcb47a1d8b28059a38b6a6d1bc93fa9e53e3f3534571339fe6c63205a1d97cf7f")},
		{"amsterdam", common.HexToHash("0xf2ca3be956811381db0b48028135ddde3053f592125d041acd67d19f832b9645")},
	}
	for _, tc := range cases {
		t.Run(tc.fork, func(t *testing.T) {
			g, err := genesis.BuildSynthetic(tc.fork, big.NewInt(1337), 60_000_000,
				1_700_000_000, []byte{0xde, 0xad, 0xbe, 0xef})
			if err != nil {
				t.Fatalf("BuildSynthetic: %v", err)
			}
			h := genesisheader.Build(g, 0, common.Hash{}, types.EmptyRootHash)
			if got := h.Hash(); got != tc.hash {
				t.Fatalf("genesis hash %s, nimbus rebuilds %s", got.Hex(), tc.hash.Hex())
			}
			isAmsterdam := tc.fork == "amsterdam"
			if (h.BlockAccessListHash != nil) != isAmsterdam || (h.SlotNumber != nil) != isAmsterdam {
				t.Fatalf("blockAccessListHash/slotNumber presence wrong for %s: %v / %v",
					tc.fork, h.BlockAccessListHash, h.SlotNumber)
			}
			if isAmsterdam && (*h.BlockAccessListHash != types.EmptyBlockAccessListHash || *h.SlotNumber != 0) {
				t.Fatalf("amsterdam fields: blockAccessListHash %s slotNumber %d",
					h.BlockAccessListHash.Hex(), *h.SlotNumber)
			}
		})
	}
}

func TestStoreDir(t *testing.T) {
	if got := StoreDir("/data"); got != "/data/ecdb" {
		t.Fatalf("StoreDir = %s", got)
	}
}
