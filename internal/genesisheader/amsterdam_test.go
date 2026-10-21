package genesisheader

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/ethereum/state-actor/genesis"
)

// TestBuild_MatchesGethGenesisHash locks Build's header hash to the
// genesis hash the real geth binary derives for the same synthetic
// genesis. Each fork adds optional header fields, and the genesis hash is
// the keccak of the full RLP, so a missing or extra field fails here
// instead of as a "wrong genesis hash" at client boot.
//
// The hashes come from `geth init` (ethereum/client-go:v1.17.6, the
// Sepolia Amsterdam release) on the JSON of each BuildSynthetic genesis
// below. go-ethereum's core package cannot be imported for a live check:
// the module's holiman/bloomfilter replace (needed by erigon) breaks
// core/state/snapshot. Prague and Osaka share a hash because Osaka adds
// no header field.
func TestBuild_MatchesGethGenesisHash(t *testing.T) {
	for _, tc := range []struct {
		fork string
		want common.Hash
	}{
		{"prague", common.HexToHash("0xcb47a1d8b28059a38b6a6d1bc93fa9e53e3f3534571339fe6c63205a1d97cf7f")},
		{"osaka", common.HexToHash("0xcb47a1d8b28059a38b6a6d1bc93fa9e53e3f3534571339fe6c63205a1d97cf7f")},
		{"amsterdam", common.HexToHash("0xf2ca3be956811381db0b48028135ddde3053f592125d041acd67d19f832b9645")},
	} {
		t.Run(tc.fork, func(t *testing.T) {
			g, err := genesis.BuildSynthetic(tc.fork, big.NewInt(1337), 60_000_000,
				1_700_000_000, []byte{0xde, 0xad, 0xbe, 0xef})
			if err != nil {
				t.Fatalf("BuildSynthetic(%s): %v", tc.fork, err)
			}
			got := Build(g, 0, common.Hash{}, types.EmptyRootHash).Hash()
			if got != tc.want {
				t.Errorf("genesis hash: got %s, want %s (geth v1.17.6 init)", got.Hex(), tc.want.Hex())
			}
		})
	}
}

// TestBuild_AmsterdamFields pins the two header fields Amsterdam adds.
func TestBuild_AmsterdamFields(t *testing.T) {
	g, err := genesis.BuildSynthetic("amsterdam", big.NewInt(1337), 60_000_000, 0, nil)
	if err != nil {
		t.Fatalf("BuildSynthetic(amsterdam): %v", err)
	}
	if g.Config.AmsterdamTime == nil || *g.Config.AmsterdamTime != 0 {
		t.Fatalf("AmsterdamTime: got %v, want *0", g.Config.AmsterdamTime)
	}
	h := Build(g, 0, common.Hash{}, types.EmptyRootHash)
	if h.BlockAccessListHash == nil || *h.BlockAccessListHash != types.EmptyBlockAccessListHash {
		t.Errorf("BlockAccessListHash: got %v, want %s", h.BlockAccessListHash, types.EmptyBlockAccessListHash.Hex())
	}
	if h.SlotNumber == nil || *h.SlotNumber != 0 {
		t.Errorf("SlotNumber: got %v, want *0", h.SlotNumber)
	}
	if h.RequestsHash == nil {
		t.Error("RequestsHash should be set under Amsterdam (inherits Prague)")
	}

	osaka, err := genesis.BuildSynthetic("osaka", big.NewInt(1337), 60_000_000, 0, nil)
	if err != nil {
		t.Fatalf("BuildSynthetic(osaka): %v", err)
	}
	oh := Build(osaka, 0, common.Hash{}, types.EmptyRootHash)
	if oh.BlockAccessListHash != nil || oh.SlotNumber != nil {
		t.Errorf("Osaka header must not carry Amsterdam fields: blockAccessListHash=%v slotNumber=%v",
			oh.BlockAccessListHash, oh.SlotNumber)
	}
}
