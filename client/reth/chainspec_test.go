package reth

import (
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/nerolation/state-actor/genesis"
)

func readWrittenSpec(t *testing.T, fork string) map[string]any {
	t.Helper()
	g, err := genesis.BuildSynthetic(fork, big.NewInt(1337), 30_000_000, 0, nil)
	if err != nil {
		t.Fatalf("BuildSynthetic(%q): %v", fork, err)
	}
	out := filepath.Join(t.TempDir(), "chainspec.json")
	if err := writeChainSpec(g, out); err != nil {
		t.Fatalf("writeChainSpec: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return spec
}

// TestWriteChainSpec_CancunBlobGasFieldsNotNull guards against the
// chainspec/header divergence flagged in #51: BuildSynthetic leaves
// g.{ExcessBlobGas,BlobGasUsed} nil → json.Marshal emits "null", while
// internal/genesisheader.Build materializes both to 0 in the on-disk
// header. Without the writer's zero-coalesce, alloy-genesis could parse
// null inconsistently with the header's 0 and produce a different
// genesis hash.
func TestWriteChainSpec_CancunBlobGasFieldsNotNull(t *testing.T) {
	spec := readWrittenSpec(t, "osaka")
	for _, k := range []string{"excessBlobGas", "blobGasUsed"} {
		v, ok := spec[k]
		if !ok {
			t.Errorf("%s missing", k)
			continue
		}
		if v == nil {
			t.Errorf("%s = null; want \"0x0\" (chainspec/header parity)", k)
			continue
		}
		if got, ok := v.(string); !ok || got != "0x0" {
			t.Errorf("%s = %v (%T); want \"0x0\"", k, v, v)
		}
	}
}
