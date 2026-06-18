package ethrex_test

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	gethrlp "github.com/ethereum/go-ethereum/rlp"

	"github.com/ethereum/state-actor/genesis"
	"github.com/ethereum/state-actor/internal/genesisheader"
)

// TestGenesisHeaderGolden builds a genesis header matching the ethrex spike
// fixture (internal/ethrex/testdata/gen/genesis.json) and verifies:
//  1. rlp.EncodeToBytes(header) byte-equals the genesis_dump.json `headers` value.
//  2. header.Hash() == 0xb9351fb93aa55de48457a067ecdf1d4d25ef2c2204ed7cbe6f72ab009db07b63.
//
// This test is pure Go (no cgo) and validates the header path locally.
func TestGenesisHeaderGolden(t *testing.T) {
	// Build a genesis matching internal/ethrex/testdata/gen/genesis.json.
	// Fields:
	//   chainId=1337, gasLimit=0x17d7840, timestamp=0, difficulty=0,
	//   extraData=0x (empty), coinbase=0x0000...0000, mixHash=0x0000...0000, nonce=0x0
	//   shanghaiTime=0, cancunTime=0, pragueTime=0.
	//   No OsakaTime (genesis.json has no osaka field).
	//   No baseFee field → genesisheader.Build uses params.InitialBaseFee (1 gwei = 0x3b9aca00).
	g, err := genesis.BuildSynthetic("prague", big.NewInt(1337), 0x17d7840, 0, nil)
	if err != nil {
		t.Fatalf("BuildSynthetic: %v", err)
	}
	// genesis.BuildSynthetic sets gasLimit even when passed 0; we pass 0x17d7840.
	// Double-check.
	if uint64(g.GasLimit) != 0x17d7840 {
		t.Fatalf("GasLimit: got 0x%x, want 0x17d7840", uint64(g.GasLimit))
	}

	stateRoot := common.HexToHash("0x7acb3fa2fa378c411135fef796a61c4c681c14e9c844e4af1d779a6f6a7006fb")
	header := genesisheader.Build(g, 0, common.Hash{}, stateRoot)

	// Compute RLP.
	gotRLP, err := gethrlp.EncodeToBytes(header)
	if err != nil {
		t.Fatalf("rlp.EncodeToBytes: %v", err)
	}

	// Load expected header RLP from genesis_dump.json.
	wantRLP := loadDumpHeaderRLP(t)

	if hex.EncodeToString(gotRLP) != hex.EncodeToString(wantRLP) {
		t.Errorf("header RLP mismatch:\n got  %x\n want %x", gotRLP, wantRLP)
		// Print field-by-field diff hint.
		reportHeaderDiff(t, header)
	}

	// Verify the hash.
	gotHash := header.Hash()
	wantHash := common.HexToHash("0xb9351fb93aa55de48457a067ecdf1d4d25ef2c2204ed7cbe6f72ab009db07b63")
	if gotHash != wantHash {
		t.Errorf("header.Hash():\n got  %s\n want %s", gotHash.Hex(), wantHash.Hex())
	}
}

// loadDumpHeaderRLP reads the genesis_dump.json and returns the raw bytes of
// the `headers` value (the ethrex BlockHeaderRLP).
func loadDumpHeaderRLP(t *testing.T) []byte {
	t.Helper()
	// Path relative to the module root: internal/ethrex/testdata/genesis_dump.json.
	data, err := os.ReadFile("../../internal/ethrex/testdata/genesis_dump.json")
	if err != nil {
		t.Fatalf("read genesis_dump.json: %v", err)
	}
	var dump struct {
		CFs map[string][][2]string `json:"cfs"`
	}
	if err := json.Unmarshal(data, &dump); err != nil {
		t.Fatalf("unmarshal genesis_dump.json: %v", err)
	}
	headerRows := dump.CFs["headers"]
	if len(headerRows) != 1 {
		t.Fatalf("expected 1 header row, got %d", len(headerRows))
	}
	return mustHex(t, headerRows[0][1])
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	s = strings.TrimPrefix(s, "0x")
	if len(s)%2 != 0 {
		s = "0" + s
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex decode %q: %v", s, err)
	}
	return b
}

// reportHeaderDiff prints individual header fields from the computed header
// to aid debugging when the golden test fails.
func reportHeaderDiff(t *testing.T, header interface{}) {
	t.Helper()
	// Use fmt to avoid import cycle; just print the struct.
	t.Logf("computed header fields: %+v", fmt.Sprintf("%+v", header))
}
