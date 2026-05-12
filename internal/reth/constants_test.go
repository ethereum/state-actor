package reth

import (
	"strings"
	"testing"
)

func TestPinnedConstants(t *testing.T) {
	if PinnedCodecsVer != "0.3.1" {
		t.Errorf("PinnedCodecsVer = %q, want %q", PinnedCodecsVer, "0.3.1")
	}
	if PinnedAlloyTrieVer != "0.9.5" {
		t.Errorf("PinnedAlloyTrieVer = %q, want %q", PinnedAlloyTrieVer, "0.9.5")
	}
	if PinnedMdbxGoVer != "v0.38.4" {
		t.Errorf("PinnedMdbxGoVer = %q, want %q", PinnedMdbxGoVer, "v0.38.4")
	}
	if DBVersion != 2 {
		t.Errorf("DBVersion = %d, want 2", DBVersion)
	}
	if PinnedRethCommit == "" {
		t.Error("PinnedRethCommit must not be empty")
	}
	if PinnedRethRelease == "" {
		t.Error("PinnedRethRelease must not be empty")
	}
	// PinnedRethRelease must remain digest-pinned. The whole reason this
	// pin exists is CI reproducibility — upstream's `nightly` tag is
	// overwritten daily, so a future bump that drops the `@sha256:...`
	// suffix (e.g., a hasty switch to a plain semver tag, which GHCR
	// allows to be overwritten) silently re-introduces the failure mode
	// this PR exists to prevent. Fail loud here instead.
	if !strings.Contains(PinnedRethRelease, "@sha256:") {
		t.Errorf("PinnedRethRelease = %q must be digest-pinned (contain @sha256:) for CI reproducibility", PinnedRethRelease)
	}
}
