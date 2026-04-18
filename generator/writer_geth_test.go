package generator

import (
	"os"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
)

func TestGethWriterBasic(t *testing.T) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "geth-writer-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create writer
	w, err := NewGethWriter(tmpDir, 1000, 4)
	if err != nil {
		t.Fatalf("Failed to create Geth writer: %v", err)
	}
	defer w.Close()

	// Write an account
	addr := common.HexToAddress("0x1234567890abcdef1234567890abcdef12345678")
	acc := &types.StateAccount{
		Nonce:    42,
		Balance:  uint256.NewInt(1000000000000000000), // 1 ETH
		Root:     types.EmptyRootHash,
		CodeHash: types.EmptyCodeHash.Bytes(),
	}

	if err := w.WriteAccount(addr, crypto.Keccak256Hash(addr[:]), acc, 0); err != nil {
		t.Fatalf("Failed to write account: %v", err)
	}

	// Write storage
	slot := common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000001")
	value := common.HexToHash("0x000000000000000000000000000000000000000000000000000000000000002a")

	if err := w.WriteStorage(addr, crypto.Keccak256Hash(addr[:]), slot, crypto.Keccak256Hash(slot[:]), value); err != nil {
		t.Fatalf("Failed to write storage: %v", err)
	}

	// Flush
	if err := w.Flush(); err != nil {
		t.Fatalf("Failed to flush: %v", err)
	}

	// Check stats
	stats := w.Stats()
	if stats.AccountBytes == 0 {
		t.Error("Expected non-zero account bytes")
	}
	if stats.StorageBytes == 0 {
		t.Error("Expected non-zero storage bytes")
	}

	t.Logf("Geth writer stats: accounts=%d, storage=%d, code=%d",
		stats.AccountBytes, stats.StorageBytes, stats.CodeBytes)
}
