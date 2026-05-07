package oracle

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/nerolation/state-actor/internal/rpcprobe"
)

// SpamoorRunCfg parameterizes a spamoor erc20_bloater run for the per-
// client suite tests. The goal here is "force the node to mine + execute
// some real txs" — NOT to stress the storage layer. Block budget is
// short (~100 blocks), gas is intentionally low, slot duration is short
// so block production starts immediately.
type SpamoorRunCfg struct {
	// Binary is the spamoor executable. Caller resolves $SPAMOOR /
	// command -v spamoor before invoking; SpamoorRun does not.
	Binary string

	// RPCURL is the running client's JSON-RPC endpoint
	// (e.g. "http://172.17.0.4:8545").
	RPCURL string

	// PrivKey is the hex private key (no 0x prefix) of a pre-funded
	// account state-actor wrote into genesis via --inject-accounts.
	// erc20_bloater uses this as the sender + base wallet for child-
	// wallet derivation.
	PrivKey string

	// Seed is the wallet-pool BIP32-derivation seed. Same value across
	// all 4 client suites so the per-spamoor test wallets are
	// deterministic across runs.
	Seed uint64

	// TargetBlockDelta — keep running until tip advances by this many
	// blocks past the start tip. ~100 is enough to verify "blocks are
	// being mined and processed" without bloating CI runtime.
	TargetBlockDelta uint64

	// SlotDuration is passed as --slot-duration. Shorter = faster
	// bock fill; client-side dev-mining controls actual block period.
	SlotDuration time.Duration

	// WalletCount is passed as --wallet-count.
	WalletCount int

	// TargetGasRatio — fraction of block gas filled per slot. 0.1 is
	// "low gas" (~3M of a 30M block); enough to populate state but
	// keeps each block cheap.
	TargetGasRatio float64

	// Timeout — overall wall-clock cap. SpamoorRun returns an error if
	// the target block delta isn't reached within this window.
	Timeout time.Duration
}

// SpamoorRun launches the spamoor erc20_bloater scenario as a background
// subprocess, polls eth_blockNumber until tip advances by cfg.TargetBlockDelta
// blocks, then SIGTERMs spamoor. Returns the post-run tip block number.
//
// Stdout/stderr from spamoor go to the calling process's stderr, so test
// logs capture them automatically (useful for triage when spamoor
// crashes mid-run).
//
// Caveat: SpamoorRun assumes the chain has at least 1 block already
// (so a starting tip is observable). For boot-only suites where tip
// is at 0, SpamoorRun's "tip - start ≥ delta" check still works
// because eth_blockNumber returns 0 → spamoor advances to ≥ delta.
func SpamoorRun(cfg SpamoorRunCfg) (uint64, error) {
	if cfg.Binary == "" {
		return 0, fmt.Errorf("SpamoorRun: empty Binary (set $SPAMOOR or pass explicit path)")
	}
	if cfg.RPCURL == "" {
		return 0, fmt.Errorf("SpamoorRun: empty RPCURL")
	}
	if cfg.TargetBlockDelta == 0 {
		return 0, fmt.Errorf("SpamoorRun: TargetBlockDelta must be > 0")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Minute
	}
	if cfg.SlotDuration == 0 {
		cfg.SlotDuration = time.Second
	}
	if cfg.WalletCount == 0 {
		cfg.WalletCount = 5
	}
	if cfg.TargetGasRatio == 0 {
		cfg.TargetGasRatio = 0.1
	}

	// Pre-flight: read start tip with the same 5×500ms tolerance
	// rpcprobe.WaitForRPC uses. Today's startup races (RPC accepting
	// connections per WaitForRPC but eth_blockNumber momentarily
	// stalling) shouldn't hard-fail the suite asymmetrically vs the
	// in-loop tolerance below.
	var startTip uint64
	{
		const preflightAttempts = 5
		var preErr error
		for i := 0; i < preflightAttempts; i++ {
			startTip, preErr = rpcprobe.EthBlockNumber(cfg.RPCURL)
			if preErr == nil {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if preErr != nil {
			return 0, fmt.Errorf("SpamoorRun: read start tip after %d attempts: %w", preflightAttempts, preErr)
		}
	}
	targetTip := startTip + cfg.TargetBlockDelta

	args := []string{
		"erc20_bloater",
		"--rpchost", cfg.RPCURL,
		"--privkey", cfg.PrivKey,
		"--seed", fmt.Sprintf("%d", cfg.Seed),
		// --target-gb=100 keeps the scenario from self-terminating
		// before the block delta is reached. SpamoorRun owns the
		// stop signal (SIGTERM after tip ≥ targetTip).
		"--target-gb", "100",
		"--target-gas-ratio", fmt.Sprintf("%.3f", cfg.TargetGasRatio),
		"--wallet-count", fmt.Sprintf("%d", cfg.WalletCount),
		"--slot-duration", cfg.SlotDuration.String(),
	}
	cmd := exec.Command(cfg.Binary, args...)
	cmd.Stdout = os.Stderr // route to test logs
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // own process group → kill children with one signal

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("SpamoorRun: start: %w", err)
	}
	defer func() {
		if cmd.Process == nil {
			return
		}
		pid := cmd.Process.Pid
		// SIGTERM the process group so spamoor's child goroutines
		// wind down with it.
		if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
			fmt.Fprintf(os.Stderr, "SpamoorRun cleanup: SIGTERM pgid=%d: %v\n", pid, err)
		}
		// Wait up to 10s for graceful exit; if it doesn't, escalate
		// to SIGKILL. A wedged spamoor (e.g. blocked on a syscall
		// that ignores SIGTERM) would otherwise leak the test
		// goroutine until the outer GHA timeout.
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil && !strings.Contains(err.Error(), "signal: terminated") {
				fmt.Fprintf(os.Stderr, "SpamoorRun cleanup: Wait: %v\n", err)
			}
		case <-time.After(10 * time.Second):
			fmt.Fprintf(os.Stderr, "SpamoorRun cleanup: SIGTERM didn't exit in 10s, escalating to SIGKILL\n")
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			<-done
		}
	}()

	deadline := time.Now().Add(cfg.Timeout)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()

	var lastTip uint64
	var lastRPCErr error
	for {
		select {
		case <-tick.C:
			// Deadline check FIRST — must fire even when RPC has been
			// failing the whole time (broken RPC mid-spamoor must not
			// silently hang until GHA's outer 30-min kill). Surfaces
			// the most recent RPC error in the timeout message.
			if time.Now().After(deadline) {
				if lastRPCErr != nil {
					return lastTip, fmt.Errorf("SpamoorRun: tip=%d, want ≥ %d, timed out after %s; last RPC error: %v",
						lastTip, targetTip, cfg.Timeout, lastRPCErr)
				}
				return lastTip, fmt.Errorf("SpamoorRun: tip=%d, want ≥ %d, timed out after %s",
					lastTip, targetTip, cfg.Timeout)
			}
			tip, err := rpcprobe.EthBlockNumber(cfg.RPCURL)
			if err != nil {
				// Tolerate transient RPC errors — spamoor's load can
				// briefly stall the node's JSON-RPC. Track the last
				// error so the deadline-fire message has context.
				lastRPCErr = err
				continue
			}
			lastTip = tip
			lastRPCErr = nil
			if tip >= targetTip {
				return tip, nil
			}
		}
	}
}
