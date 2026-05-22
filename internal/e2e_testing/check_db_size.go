package e2e_testing

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// CheckDBSize walks dbPath, sums regular-file sizes, and asserts the total
// lands within tolerancePct of expectedBytes. Skips silently when
// expectedBytes is zero (caller didn't supply a target). Returns true on
// success, t.Errorf-fails and returns false otherwise.
//
// Takes testing.TB so unit tests can pass a mock; production callers use
// *testing.T directly.
//
// tolerancePct is a fraction in [0, 1] — e.g. 0.10 → ±10 %. The cross-
// client invariance gate uses a generous tolerance because Pebble (geth /
// besu) and MDBX (reth / nethermind) compaction behavior differs
// substantially at the small-DB scale the e2e suites run at.
func CheckDBSize(t testing.TB, dbPath string, expectedBytes uint64, tolerancePct float64) bool {
	t.Helper()
	if expectedBytes == 0 {
		return true
	}
	got, err := sumDirBytes(dbPath)
	if err != nil {
		t.Errorf("CheckDBSize: walk %q: %v", dbPath, err)
		return false
	}
	lo := uint64(float64(expectedBytes) * (1 - tolerancePct))
	hi := uint64(float64(expectedBytes) * (1 + tolerancePct))
	t.Logf("CheckDBSize: dbPath=%s size=%s (expected %s ±%.0f%%)",
		dbPath, fmtBytes(got), fmtBytes(expectedBytes), tolerancePct*100)
	if got < lo || got > hi {
		t.Errorf("CheckDBSize: %s is %s, want within [%s, %s] (%.0f%% of %s)",
			dbPath, fmtBytes(got), fmtBytes(lo), fmtBytes(hi),
			tolerancePct*100, fmtBytes(expectedBytes))
		return false
	}
	return true
}

func sumDirBytes(root string) (uint64, error) {
	var total uint64
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += uint64(info.Size())
		}
		return nil
	})
	return total, err
}

func fmtBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
