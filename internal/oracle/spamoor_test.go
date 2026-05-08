package oracle

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSpamoorRun_ArgValidation(t *testing.T) {
	cases := []struct {
		name     string
		cfg      SpamoorRunCfg
		wantSubstr string
	}{
		{"empty Binary", SpamoorRunCfg{Binary: "", RPCURL: "http://x", TargetBlockDelta: 1}, "empty Binary"},
		{"empty RPCURL", SpamoorRunCfg{Binary: "/bin/true", RPCURL: "", TargetBlockDelta: 1}, "empty RPCURL"},
		{"zero TargetBlockDelta", SpamoorRunCfg{Binary: "/bin/true", RPCURL: "http://x", TargetBlockDelta: 0}, "TargetBlockDelta"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := SpamoorRun(tc.cfg)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSubstr)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("expected error containing %q, got %v", tc.wantSubstr, err)
			}
		})
	}
}

// TestSpamoorRun_BrokenRPCTimesOut — verifies the deadline-fire fix
// from PR-C-followup Commit 1. Pre-fix, the select had only `<-tick.C`
// and the deadline check was inside the err==nil branch, so once a
// running spamoor lost its RPC mid-run, SpamoorRun spun until the
// outer GHA timeout.
//
// Stub: pre-flight EthBlockNumber succeeds (returns 0x0); every
// subsequent call errors. SpamoorRun must enter the post-launch loop,
// hit the broken-RPC path, and time out within ~Timeout — NOT loop
// forever.
func TestSpamoorRun_BrokenRPCTimesOut(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in -short")
	}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			// Pre-flight succeeds.
			var req struct{ ID int }
			_ = json.NewDecoder(r.Body).Decode(&req)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID, "result": "0x0",
			})
			return
		}
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()

	// Note: SpamoorRun adds many positional args (erc20_bloater + flags)
	// after the binary path. /bin/cat with stdin closed (no -d redirect)
	// just exits 0 immediately; that's fine — the test exercises the
	// deadline path on the polling loop, not the binary itself.
	start := time.Now()
	_, err := SpamoorRun(SpamoorRunCfg{
		Binary:           "/bin/cat",
		RPCURL:           srv.URL,
		PrivKey:          "0x0",
		Seed:             0,
		TargetBlockDelta: 100,
		Timeout:          3 * time.Second,
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected timeout error, got %v", err)
	}
	// Must fire within ~Timeout + SIGTERM grace + slack, NOT loop forever.
	if elapsed > 30*time.Second {
		t.Errorf("SpamoorRun took %v; expected ≤30s (deadline-fire bug regressed?)", elapsed)
	}
}

// TestSpamoorRun_PreFlightRetries — pre-flight EthBlockNumber should
// retry transient RPC failures (5×500ms) per the fix in Commit 1. The
// stub starts erroring then recovers; SpamoorRun must succeed (or
// fail later, in the post-launch loop) rather than hard-fail at the
// pre-flight stage.
func TestSpamoorRun_PreFlightRetries(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in -short")
	}
	// Server: first 3 calls error, then succeed.
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n <= 3 {
			http.Error(w, "warming up", http.StatusInternalServerError)
			return
		}
		var req struct{ Method string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 1, "result": "0x0",
		})
	}))
	defer srv.Close()

	// We expect SpamoorRun to start the binary, then fail in the post-
	// launch loop because /bin/false exits immediately. The pre-flight
	// retry should succeed (5 attempts at 500ms each = up to 2.5s vs
	// our 3 stub-failures = ~1.5s of retry).
	_, err := SpamoorRun(SpamoorRunCfg{
		Binary:           "/bin/false", // exits immediately
		RPCURL:           srv.URL,
		PrivKey:          "0x0",
		Seed:             0,
		TargetBlockDelta: 100,
		Timeout:          2 * time.Second,
	})
	// Either timeout (binary exited; tip never advances → deadline) or
	// a different error — both are acceptable; what we're verifying is
	// the pre-flight DIDN'T return "read start tip" hard-failure.
	if err != nil && strings.Contains(err.Error(), "read start tip") {
		t.Errorf("pre-flight hard-failed despite stub recovering: %v", err)
	}
}
