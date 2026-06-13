package e2e_testing

import (
	"iter"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/nerolation/state-actor/internal/rpcprobe"
	"github.com/nerolation/state-actor/internal/templates"
)

// specStorageDrainCap bounds how many (key, value) pairs CheckSpecStorage
// drains from one entity's Storage iterator before sampling. Full drains
// are forbidden here: PreAllocEntity.Storage is iter.Seq2 precisely so
// multi-GB entities never materialize (the CI fixture synthesizes a few
// hundred slots per entity; production specs reach millions, and
// templates.CollectMap's own docstring rules out O(N) collection). 64 is
// large enough to swallow every map-backed segment in the CI fixture
// whole — which is what keeps the sampled SET deterministic run-to-run —
// at ~4 KiB per entity.
const specStorageDrainCap = 64

// CheckSpecStorage re-queries a bounded, deterministic sample of each
// spec entity's storage via eth_getStorageAt and compares against the
// PreAlloc expectation. Expectations are read from the PreAlloc records
// ONLY — cfg.GenesisStorage is empty on the spec path
// (generator.Config.materializePreAlloc leaves Storage on the iterator
// for the writers' Phase-0 streaming; populating GenesisStorage would
// risk double-writes since all four writers consume that map). Without
// this probe, spec storage is verified at e2e level only by cross-client
// root agreement — four writers reproducing the same wrong layout pass.
//
// Entities with nil Storage (plain EOAs, code-only contracts) are
// skipped. Errors are anchored by PreAlloc index + address hex
// (PreAllocEntity carries no entity name). Reports via t.Errorf and
// returns false on any mismatch, mirroring CheckEntities / CheckInjections.
func CheckSpecStorage(t *testing.T, rpcURL string, preAlloc []templates.PreAllocEntity, blockTag string) bool {
	t.Helper()
	passed := true
	for i := range preAlloc {
		pe := &preAlloc[i]
		if pe.Storage == nil {
			continue
		}
		drained := drainStoragePrefix(pe.Storage, specStorageDrainCap)
		for _, slot := range sampleStorageSlots(drained) {
			want := drained[slot]
			got, err := rpcprobe.EthGetStorageAt(rpcURL, pe.Address, slot, blockTag)
			if err != nil {
				t.Errorf("[%s] spec PreAlloc[%d] eth_getStorageAt %s slot %s: %v",
					blockTag, i, pe.Address.Hex(), slot.Hex(), err)
				passed = false
				continue
			}
			if got != want {
				t.Errorf("[%s] spec PreAlloc[%d] eth_getStorageAt %s slot %s: got %s want %s — writer dropped spec-entity storage?",
					blockTag, i, pe.Address.Hex(), slot.Hex(), got.Hex(), want.Hex())
				passed = false
			}
		}
	}
	return passed
}

// drainStoragePrefix collects at most capN pairs from seq, early-stopping
// the iterator (yield → false) the moment the cap is reached. It never
// drains past capN — see specStorageDrainCap for why full drains are
// forbidden. PreAllocEntity.Storage iterators are pure and re-iterable
// (templates.PreAllocEntity doc), so reading a prefix here is
// side-effect-free even though the writers already consumed the same
// iterator during Phase 0.
func drainStoragePrefix(seq iter.Seq2[common.Hash, common.Hash], capN int) map[common.Hash]common.Hash {
	out := make(map[common.Hash]common.Hash, capN)
	seq(func(k, v common.Hash) bool {
		out[k] = v
		return len(out) < capN
	})
	return out
}
