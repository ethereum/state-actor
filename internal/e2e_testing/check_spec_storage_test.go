package e2e_testing

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/ethereum/state-actor/internal/templates"
)

// TestCheckSpecStorage_HappyPath drives CheckSpecStorage against the
// shared rpcStub: a nil-Storage entity must be skipped, a map-backed
// entity and a pure-closure entity must both have their slots probed and
// matched. (Mismatch detection — where CheckSpecStorage returns false —
// is covered end-to-end by the per-client e2e suites, same rationale as
// TestCheckEntities_HappyPath: capturing t.Errorf needs a mock T.)
func TestCheckSpecStorage_HappyPath(t *testing.T) {
	plainEOA := common.HexToAddress("0xaaaa")    // nil Storage → skipped
	mapAddr := common.HexToAddress("0xbbbb")     // MapToSeq-backed
	closureAddr := common.HexToAddress("0xcccc") // pure-closure-backed

	mapStorage := map[common.Hash]common.Hash{
		common.HexToHash("0x0"): common.HexToHash("0x2a"),
		common.HexToHash("0x1"): common.HexToHash("0xcafe"),
	}
	// SynthesizeSlots is a pure (re-iterable) closure — the storage_pattern
	// / approximate_size_bytes shape that has no backing map.
	closureSeq := templates.SynthesizeSlots(7, closureAddr, "test", 12)

	stubStorage := map[common.Address]map[common.Hash]common.Hash{
		mapAddr:     mapStorage,
		closureAddr: templates.CollectMap(closureSeq),
	}
	stub := &rpcStub{storage: stubStorage}
	srv := stub.Server(t)
	defer srv.Close()

	preAlloc := []templates.PreAllocEntity{
		{Address: plainEOA}, // Storage nil
		{Address: mapAddr, Storage: templates.MapToSeq(mapStorage)},
		{Address: closureAddr, Storage: templates.SynthesizeSlots(7, closureAddr, "test", 12)},
	}
	if !CheckSpecStorage(t, srv.URL, preAlloc, "latest") {
		t.Fatal("expected pass on happy-path stub; got false")
	}
}

// TestDrainStoragePrefixCapsAndEarlyStops pins the no-full-drain
// guarantee: a closure that would yield far more than the cap must be
// stopped at exactly specStorageDrainCap, and the closure must observe
// exactly that many yields (proving early-stop, not post-truncation).
func TestDrainStoragePrefixCapsAndEarlyStops(t *testing.T) {
	const yieldBudget = 10_000
	observed := 0
	seq := func(yield func(common.Hash, common.Hash) bool) {
		for i := 0; i < yieldBudget; i++ {
			observed++
			k := common.BigToHash(common.Big0)
			k[31] = byte(i)
			k[30] = byte(i >> 8)
			if !yield(k, common.HexToHash("0x1")) {
				return
			}
		}
	}
	out := drainStoragePrefix(seq, specStorageDrainCap)
	if len(out) != specStorageDrainCap {
		t.Errorf("drained %d pairs, want cap %d", len(out), specStorageDrainCap)
	}
	if observed != specStorageDrainCap {
		t.Errorf("closure observed %d yields, want %d (early-stop broken — draining past the cap)", observed, specStorageDrainCap)
	}
}

// TestDrainStoragePrefixSetDeterministicForMapBackedUnderCap: a map
// segment smaller than the cap drains to the same SET every run despite
// Go's random map iteration order — this is why sampleStorageSlots
// (which sorts) yields a stable sample across runs and across clients.
func TestDrainStoragePrefixSetDeterministicForMapBackedUnderCap(t *testing.T) {
	m := make(map[common.Hash]common.Hash, 20)
	for i := 0; i < 20; i++ {
		var k common.Hash
		k[31] = byte(i)
		m[k] = common.HexToHash("0x1")
	}
	first := drainStoragePrefix(templates.MapToSeq(m), specStorageDrainCap)
	second := drainStoragePrefix(templates.MapToSeq(m), specStorageDrainCap)
	if len(first) != 20 || len(second) != 20 {
		t.Fatalf("under-cap map should drain fully: got %d / %d, want 20", len(first), len(second))
	}
	for k, v := range first {
		if second[k] != v {
			t.Errorf("drained set diverged at %s across runs", k.Hex())
		}
	}
}
