// Vendored from github.com/erigontech/erigon execution/commitment/metrics.go @ 14273f79a6 (production pin).
// Modifications: package commitment -> hph; build tag; R2+R3: reduced to the live counter subset (CSV read+write paths, MetricValues, Reset/AsValues/headers, cache/miss counters removed)
//
//go:build cgo_erigon_commitment

package hph

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/erigontech/erigon/common/dbg"
	"github.com/erigontech/erigon/common/length"
)

type Metrics struct {
	Accounts       *AccountMetrics
	Branches       *BranchMetrics
	addressKeys    atomic.Uint64
	storageKeys    atomic.Uint64
	loadBranch     atomic.Uint64
	loadAccount    atomic.Uint64
	loadStorage    atomic.Uint64
	updateBranch   atomic.Uint64
	unfolds        atomic.Uint64
	spentUnfolding time.Duration
	spentFolding   time.Duration

	collectCommitmentMetrics bool
	writeCommitmentMetrics   bool
}

func NewMetrics() *Metrics {
	return &Metrics{
		Accounts:                 NewAccounts(),
		Branches:                 NewBranches(),
		collectCommitmentMetrics: dbg.KVReadLevelledMetrics,
	}
}

func (m *Metrics) Updates(plainKey []byte) {
	if len(plainKey) == length.Addr {
		m.addressKeys.Add(1)
	} else {
		m.storageKeys.Add(1)

		if m.collectCommitmentMetrics {
			m.Accounts.collect(plainKey, func(mx *AccountStats) {
				mx.StorageUpates++
			})
		}
	}
}

func (m *Metrics) AccountLoad(plainKey []byte) {
	m.loadAccount.Add(1)
	if m.collectCommitmentMetrics {
		m.Accounts.collect(plainKey, func(mx *AccountStats) {
			mx.LoadAccount++
		})
	}
}

func (m *Metrics) StorageLoad(plainKey []byte) {
	m.loadStorage.Add(1)
	if m.collectCommitmentMetrics {
		m.Accounts.collect(plainKey, func(mx *AccountStats) {
			mx.LoadStorage++
		})
	}
}

func (m *Metrics) BranchLoad(plainKey []byte) {
	m.loadBranch.Add(1)
	if m.collectCommitmentMetrics {
		m.Branches.collect(plainKey, func(mx *BranchStats) {
			mx.LoadBranch++
		})
	}
}

func (m *Metrics) StartUnfolding(plainKey []byte) func() {
	m.unfolds.Add(1)
	if m.collectCommitmentMetrics {
		start := time.Now()
		return func() {
			d := time.Since(start)
			m.spentUnfolding += d
			m.Accounts.collect(plainKey, func(mx *AccountStats) {
				mx.SpentUnfolding += d
			})
		}
	}
	return func() {}
}

func (m *Metrics) StartFolding(plainKey []byte) func() {
	if m.collectCommitmentMetrics {
		start := time.Now()
		return func() {
			d := time.Since(start)
			m.spentFolding += d
			m.Accounts.collect(plainKey, func(mx *AccountStats) {
				mx.SpentFolding += d
			})
		}
	}
	return func() {}
}

func NewAccounts() *AccountMetrics {
	return &AccountMetrics{
		AccountStats: make(map[string]*AccountStats),
	}
}

type AccountStats struct {
	StorageUpates  uint64
	LoadAccount    uint64
	LoadStorage    uint64
	Unfolds        uint64
	Folds          uint64
	SpentUnfolding time.Duration
	SpentFolding   time.Duration
}

type AccountMetrics struct {
	m sync.RWMutex
	// will be separate value for each key in parallel processing
	AccountStats map[string]*AccountStats
	// metric config related
	writeCommitmentMetrics bool
}

func (am *AccountMetrics) collect(plainKey []byte, fn func(mx *AccountStats)) {
	if !am.writeCommitmentMetrics {
		return
	}
	var addr string
	if len(plainKey) > 0 {
		addr = string(plainKey[:min(length.Addr, len(plainKey))])
	}
	am.m.Lock()
	defer am.m.Unlock()
	as, ok := am.AccountStats[addr]
	if !ok {
		as = &AccountStats{}
		am.AccountStats[addr] = as
	}
	fn(as)
}

type BranchStats struct {
	LoadBranch uint64
}

func NewBranches() *BranchMetrics {
	return &BranchMetrics{BranchStats: make(map[string]*BranchStats)}
}

type BranchMetrics struct {
	m sync.RWMutex
	// will be separate value for each key in parallel processing
	BranchStats map[string]*BranchStats
	// metric config related
	writeCommitmentMetrics bool
}

func (bm *BranchMetrics) collect(plainKey []byte, fn func(mx *BranchStats)) {
	if !bm.writeCommitmentMetrics {
		return
	}
	if len(plainKey) == 0 {
		return
	}
	addr := string(plainKey)
	bm.m.Lock()
	defer bm.m.Unlock()
	bs, ok := bm.BranchStats[addr]
	if !ok {
		bs = &BranchStats{}
		bm.BranchStats[addr] = bs
	}
	fn(bs)
}
