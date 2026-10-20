package reth

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/ethereum/state-actor/genesis"
)

// TestBuildBlock0Header_GenesisHash pins the block-0 hash that reth boot
// reads from static files. The values are the `geth init` hashes from
// go-ethereum v1.17.6 for the same synthetic genesis (see
// internal/genesisheader/amsterdam_test.go). reth v2.7.0's
// make_genesis_header sets the same Amsterdam fields: blockAccessListHash
// = EMPTY_BLOCK_ACCESS_LIST_HASH and slotNumber = 0.
func TestBuildBlock0Header_GenesisHash(t *testing.T) {
	for _, tc := range []struct {
		fork string
		want common.Hash
	}{
		{"osaka", common.HexToHash("0xcb47a1d8b28059a38b6a6d1bc93fa9e53e3f3534571339fe6c63205a1d97cf7f")},
		{"amsterdam", common.HexToHash("0xf2ca3be956811381db0b48028135ddde3053f592125d041acd67d19f832b9645")},
	} {
		t.Run(tc.fork, func(t *testing.T) {
			g, err := genesis.BuildSynthetic(tc.fork, big.NewInt(1337), 60_000_000,
				1_700_000_000, []byte{0xde, 0xad, 0xbe, 0xef})
			if err != nil {
				t.Fatalf("BuildSynthetic(%s): %v", tc.fork, err)
			}
			h, err := buildBlock0Header(g)
			if err != nil {
				t.Fatalf("buildBlock0Header: %v", err)
			}
			if got := h.Hash(); got != tc.want {
				t.Errorf("genesis hash: got %s, want %s", got.Hex(), tc.want.Hex())
			}
		})
	}
}
