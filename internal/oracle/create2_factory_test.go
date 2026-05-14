package oracle

import (
	"bytes"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/nerolation/state-actor/generator"
)

// userBenchmarkInitcode is the initcode the benchmark suite deploys via
// the deterministic CREATE2 factory. It builds a 24576-byte body of
// JUMPDESTs in memory, then OR-merges ADDRESS into memory[0x20:0x40]
// so the deployed runtime contains the contract's own address — i.e.
// the deployed code is salt-dependent and the EVM must run once per
// salt to capture the right bytes.
const userBenchmarkInitcode = "0x7f5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b6000526020600060205e6040600060405e6080600060805e61010060006101005e61020060006102005e61040060006104005e61080060006108005e61100060006110005e61200060006120005e61400060006140005e7f615fff565b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b5b6000527f5b5b5b5b5b5b5b5b5b5b5b5b000000000000000000000000000000000000000030176020526160006000f3"

func TestAddCREATE2Factory_DropsCanonical(t *testing.T) {
	cfg := &generator.Config{}
	if err := AddCREATE2Factory(cfg); err != nil {
		t.Fatalf("AddCREATE2Factory: %v", err)
	}
	acc, ok := cfg.GenesisAccounts[CanonicalCREATE2FactoryAddress]
	if !ok {
		t.Fatalf("factory address %s missing", CanonicalCREATE2FactoryAddress.Hex())
	}
	if acc.Nonce != 1 {
		t.Errorf("nonce=%d, want 1", acc.Nonce)
	}
	code, ok := cfg.GenesisCode[CanonicalCREATE2FactoryAddress]
	if !ok {
		t.Fatalf("factory code missing")
	}
	if !bytes.Equal(code, CanonicalCREATE2FactoryCode) {
		t.Errorf("factory code mismatch")
	}
	wantCodeHash := crypto.Keccak256Hash(CanonicalCREATE2FactoryCode).Bytes()
	if !bytes.Equal(acc.CodeHash, wantCodeHash) {
		t.Errorf("factory code hash mismatch")
	}
}

func TestAddCREATE2Deploys_SmallSaltRange(t *testing.T) {
	cfg := &generator.Config{}
	initcode := common.FromHex(userBenchmarkInitcode)
	if err := AddCREATE2Deploys(cfg, CanonicalCREATE2FactoryAddress, []CREATE2DeploySpec{
		{Initcode: initcode, SaltStart: 0, SaltCount: 4},
	}); err != nil {
		t.Fatalf("AddCREATE2Deploys: %v", err)
	}
	if len(cfg.GenesisAccounts) != 4 {
		t.Errorf("got %d accounts, want 4", len(cfg.GenesisAccounts))
	}
	if len(cfg.GenesisCode) != 4 {
		t.Errorf("got %d code entries, want 4", len(cfg.GenesisCode))
	}
	// Every deployed contract should be 24576 bytes (EIP-170 max) per the
	// initcode's RETURN of memory[0..0x6000].
	for addr, code := range cfg.GenesisCode {
		if len(code) != 24576 {
			t.Errorf("addr %s: deployed code = %d bytes, want 24576", addr.Hex(), len(code))
		}
	}
}

func TestAddCREATE2Deploys_PerSaltRuntimeDiffers(t *testing.T) {
	// The benchmark initcode embeds ADDRESS into the deployed code via
	// memory[0x20:0x40] = (12×JUMPDEST) | (20-byte ADDRESS). So two
	// different salts must produce two distinct runtime byte sequences.
	cfg := &generator.Config{}
	initcode := common.FromHex(userBenchmarkInitcode)
	if err := AddCREATE2Deploys(cfg, CanonicalCREATE2FactoryAddress, []CREATE2DeploySpec{
		{Initcode: initcode, SaltStart: 0, SaltCount: 2},
	}); err != nil {
		t.Fatalf("AddCREATE2Deploys: %v", err)
	}
	if len(cfg.GenesisCode) != 2 {
		t.Fatalf("got %d code entries, want 2", len(cfg.GenesisCode))
	}
	codes := make([][]byte, 0, 2)
	for _, c := range cfg.GenesisCode {
		codes = append(codes, c)
	}
	if bytes.Equal(codes[0], codes[1]) {
		t.Errorf("runtime bytes identical across salts — per-salt EVM run is not producing salt-dependent code")
	}
}

func TestAddCREATE2Deploys_DeployedCodeOverride(t *testing.T) {
	// When the spec supplies DeployedCode, EVM execution is skipped and
	// the supplied bytes are written verbatim at every derived address.
	cfg := &generator.Config{}
	override := []byte{0x60, 0x00, 0x60, 0x00, 0xf3} // PUSH1 0; PUSH1 0; RETURN
	initcode := []byte{0x60, 0xff}                   // arbitrary — not executed
	if err := AddCREATE2Deploys(cfg, CanonicalCREATE2FactoryAddress, []CREATE2DeploySpec{
		{Initcode: initcode, SaltStart: 10, SaltCount: 3, DeployedCode: override},
	}); err != nil {
		t.Fatalf("AddCREATE2Deploys: %v", err)
	}
	if len(cfg.GenesisCode) != 3 {
		t.Errorf("got %d code entries, want 3", len(cfg.GenesisCode))
	}
	for addr, c := range cfg.GenesisCode {
		if !bytes.Equal(c, override) {
			t.Errorf("addr %s: code != override (got %x, want %x)", addr.Hex(), c, override)
		}
	}
}

func TestAddCREATE2Deploys_DerivedAddressMatchesSpec(t *testing.T) {
	// Independently re-derive the CREATE2 address for salt=0 and confirm
	// that AddCREATE2Deploys wrote into that exact slot.
	cfg := &generator.Config{}
	initcode := common.FromHex(userBenchmarkInitcode)
	if err := AddCREATE2Deploys(cfg, CanonicalCREATE2FactoryAddress, []CREATE2DeploySpec{
		{Initcode: initcode, SaltStart: 0, SaltCount: 1},
	}); err != nil {
		t.Fatalf("AddCREATE2Deploys: %v", err)
	}
	var salt [32]byte
	wantAddr := crypto.CreateAddress2(
		CanonicalCREATE2FactoryAddress, salt, crypto.Keccak256(initcode),
	)
	if _, ok := cfg.GenesisAccounts[wantAddr]; !ok {
		t.Errorf("derived address %s not in GenesisAccounts", wantAddr.Hex())
	}
}

func TestAddCREATE2Deploys_RejectsEmptyInitcode(t *testing.T) {
	cfg := &generator.Config{}
	if err := AddCREATE2Deploys(cfg, CanonicalCREATE2FactoryAddress, []CREATE2DeploySpec{
		{Initcode: nil, SaltStart: 0, SaltCount: 1},
	}); err == nil {
		t.Errorf("expected empty-initcode rejection, got nil")
	}
}

func TestAddCREATE2Deploys_RejectsCollision(t *testing.T) {
	cfg := &generator.Config{}
	initcode := common.FromHex(userBenchmarkInitcode)
	if err := AddCREATE2Deploys(cfg, CanonicalCREATE2FactoryAddress, []CREATE2DeploySpec{
		{Initcode: initcode, SaltStart: 0, SaltCount: 2},
	}); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Re-running the same spec must collide (same derived addresses).
	if err := AddCREATE2Deploys(cfg, CanonicalCREATE2FactoryAddress, []CREATE2DeploySpec{
		{Initcode: initcode, SaltStart: 0, SaltCount: 2},
	}); err == nil {
		t.Errorf("expected collision error, got nil")
	}
}
