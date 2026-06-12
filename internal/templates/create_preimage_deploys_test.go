package templates

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

const bittrexAddrHex = "0xA3C1E324CA1CE40DB73ED6026C4A177F099B5770"

func TestCreatePreimageDeploysExpand(t *testing.T) {
	runtime := "0x6080604052348015600f57600080fd5b50"
	ent := mkContractEntity("create_preimage_deploys", map[string]any{
		"sender":      bittrexAddrHex,
		"start_nonce": 2,
		"count":       3,
		"runtime":     runtime,
	})

	out, err := (&createPreimageDeploysTemplate{}).Expand(Context{}, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("count: got %d, want 3", len(out))
	}
	sender := common.HexToAddress(bittrexAddrHex)
	runtimeBytes, _ := ParseHexBytesParam(runtime, "runtime")
	wantHash := crypto.Keccak256Hash(runtimeBytes).Bytes()
	for i, pe := range out {
		want := crypto.CreateAddress(sender, uint64(2+i))
		if pe.Address != want {
			t.Errorf("nonce=%d addr: got %s, want %s", 2+i, pe.Address.Hex(), want.Hex())
		}
		if !bytes.Equal(pe.Code, runtimeBytes) {
			t.Errorf("nonce=%d code mismatch", 2+i)
		}
		if !bytes.Equal(pe.Account.CodeHash, wantHash) {
			t.Errorf("nonce=%d CodeHash mismatch", 2+i)
		}
		if pe.Account.Nonce != 1 {
			t.Errorf("nonce=%d account nonce: got %d, want 1", 2+i, pe.Account.Nonce)
		}
		if pe.Storage != nil {
			t.Errorf("nonce=%d: Storage must be nil when storage_init is unset (got non-nil iter)", 2+i)
		}
	}
}

// TestCreatePreimageDeploysStorageInit pins that `storage_init` is
// propagated to every derived contract's Storage iter. The Bittrex
// example sets slot 0 = controller address on every child.
func TestCreatePreimageDeploysStorageInit(t *testing.T) {
	ent := mkContractEntity("create_preimage_deploys", map[string]any{
		"sender":      bittrexAddrHex,
		"start_nonce": 2,
		"count":       3,
		"runtime":     "0xfe",
		"storage_init": map[string]any{
			"0x0": "0xa3c1e324ca1ce40db73ed6026c4a177f099b5770",
		},
	})
	out, err := (&createPreimageDeploysTemplate{}).Expand(Context{}, ent)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("count: got %d, want 3", len(out))
	}
	wantSlot0 := common.HexToHash("0x000000000000000000000000a3c1e324ca1ce40db73ed6026c4a177f099b5770")
	for i, pe := range out {
		if pe.Storage == nil {
			t.Fatalf("derived[%d]: Storage must be non-nil when storage_init is set", i)
		}
		pairs := collectPairs(pe.Storage)
		if len(pairs) != 1 {
			t.Fatalf("derived[%d]: storage pair count: got %d, want 1", i, len(pairs))
		}
		if pairs[0].K != (common.Hash{}) {
			t.Errorf("derived[%d]: slot key: got %s, want 0x00..00", i, pairs[0].K.Hex())
		}
		if pairs[0].V != wantSlot0 {
			t.Errorf("derived[%d]: slot 0 value: got %s, want %s", i, pairs[0].V.Hex(), wantSlot0.Hex())
		}
	}
}

// TestCreatePreimageDeploysRejectsZeroCount pins the I3 fix: count=0
// used to expand to zero entities with zero warnings. Both entry points
// now reject it.
func TestCreatePreimageDeploysRejectsZeroCount(t *testing.T) {
	tmpl := &createPreimageDeploysTemplate{}
	params := map[string]any{
		"sender":  "0x000000000000000000000000000000000000beef",
		"count":   0,
		"runtime": "0x00",
	}
	if err := tmpl.ValidateParameters(params); err == nil || !strings.Contains(err.Error(), "must be >= 1") {
		t.Errorf("ValidateParameters(count=0): want 'must be >= 1' error, got %v", err)
	}
	if _, err := tmpl.Expand(Context{}, mkContractEntity("create_preimage_deploys", params)); err == nil || !strings.Contains(err.Error(), "must be >= 1") {
		t.Errorf("Expand(count=0): want 'must be >= 1' error, got %v", err)
	}
}

func TestCreatePreimageDeploysOmittedStartNonce(t *testing.T) {
	// start_nonce omitted defaults to 0 — first derived address uses
	// the deployer's first-ever CREATE nonce.
	senderHex := "0x000000000000000000000000000000000000beef"
	sender := common.HexToAddress(senderHex)
	ent := mkContractEntity("create_preimage_deploys", map[string]any{
		"sender":  senderHex,
		"count":   1,
		"runtime": "0x00",
	})
	out, _ := (&createPreimageDeploysTemplate{}).Expand(Context{}, ent)
	want := crypto.CreateAddress(sender, 0)
	if out[0].Address != want {
		t.Errorf("addr: got %s, want %s", out[0].Address.Hex(), want.Hex())
	}
}

func TestCreatePreimageDeploysValidate(t *testing.T) {
	tmpl := &createPreimageDeploysTemplate{}
	good := map[string]any{
		"sender":  bittrexAddrHex,
		"count":   10,
		"runtime": "0xfe",
	}
	if err := tmpl.ValidateParameters(good); err != nil {
		t.Errorf("valid: %v", err)
	}
	bad := []map[string]any{
		{"count": 10, "runtime": "0xfe"},                                              // missing sender
		{"sender": bittrexAddrHex, "runtime": "0xfe"},                                 // missing count
		{"sender": bittrexAddrHex, "count": 10},                                       // missing runtime
		{"sender": "not-an-address", "count": 10, "runtime": "0xfe"},                  // bad sender
		{"sender": bittrexAddrHex, "count": 10, "runtime": ""},                        // empty runtime
		{"sender": bittrexAddrHex, "count": "10", "runtime": "0xfe"},                  // bad count type
		{"sender": bittrexAddrHex, "count": -1, "runtime": "0xfe"},                    // negative count
		{"sender": bittrexAddrHex, "count": 10, "runtime": "0xfe", "start_nonce": -1}, // negative start_nonce
		{"sender": bittrexAddrHex, "count": 10, "runtime": "0xfe", "wat": "x"},        // unknown key
		// storage_init must be a map of hex-string slot → hex-string value.
		{"sender": bittrexAddrHex, "count": 1, "runtime": "0xfe", "storage_init": "0x0"},
		{"sender": bittrexAddrHex, "count": 1, "runtime": "0xfe",
			"storage_init": map[string]any{"0x0": 1}}, // value not a string
		{"sender": bittrexAddrHex, "count": 1, "runtime": "0xfe",
			"storage_init": map[string]any{"not-hex": "0x0"}}, // bad slot
	}
	for i, p := range bad {
		if err := tmpl.ValidateParameters(p); err == nil {
			t.Errorf("bad[%d]: expected error, got nil for %v", i, p)
		}
	}
	// storage_init: well-formed map is accepted.
	good = map[string]any{
		"sender": bittrexAddrHex, "count": 1, "runtime": "0xfe",
		"storage_init": map[string]any{
			"0x0": "0xa3c1e324ca1ce40db73ed6026c4a177f099b5770",
		},
	}
	if err := tmpl.ValidateParameters(good); err != nil {
		t.Errorf("storage_init valid: %v", err)
	}
}
