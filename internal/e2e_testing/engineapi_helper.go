package e2e_testing

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/state-actor/internal/engineapi"
)

// StartEngineDriver spins up an engineapi.EngineDriver mock CL targeting
// the EL at containerIP:8551 (engine-API standard port) and runs DriveLoop
// in a background goroutine until the test's cleanup fires. Caller must
// ensure the EL is up (e.g. via rpcprobe.WaitForRPC) and the engine port
// is reachable from this process.
func StartEngineDriver(t *testing.T, containerIP, rpcURL string, fork engineapi.Fork) {
	t.Helper()
	driver := &engineapi.EngineDriver{
		EngineURL: "http://" + containerIP + ":8551",
		EthRPCURL: rpcURL,
		BlockTime: time.Second,
		Fork:      fork,
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		// DriveLoop only returns non-context-Canceled errors when something
		// is permanently wrong (fetchLatestBlock at startup, or K
		// consecutive Step failures); surface those via t.Errorf — t.Fatalf
		// is not safe outside the test goroutine.
		if err := driver.DriveLoop(ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("EngineDriver exited: %v", err)
		}
	}()
}

// StartEngineDriverWithJWT is identical to StartEngineDriver but reads a
// 64-hex-char JWT secret from jwtSecretPath and supplies it to the engine
// driver so every engine-API call is signed with an HS256 Bearer token.
// Required for clients that mandate authrpc JWT authentication (ethrex)
// and do not offer an --engine-jwt-disabled equivalent.
func StartEngineDriverWithJWT(t *testing.T, containerIP, rpcURL string, fork engineapi.Fork, jwtSecretPath string) {
	t.Helper()
	raw, err := os.ReadFile(jwtSecretPath)
	if err != nil {
		t.Fatalf("StartEngineDriverWithJWT: read %s: %v", jwtSecretPath, err)
	}
	hexStr := strings.TrimSpace(strings.TrimPrefix(string(raw), "0x"))
	secret, err := hex.DecodeString(hexStr)
	if err != nil {
		t.Fatalf("StartEngineDriverWithJWT: decode JWT secret hex: %v", err)
	}
	driver := &engineapi.EngineDriver{
		EngineURL: "http://" + containerIP + ":8551",
		EthRPCURL: rpcURL,
		BlockTime: time.Second,
		Fork:      fork,
		JWTSecret: secret,
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		if err := driver.DriveLoop(ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("EngineDriver exited: %v", err)
		}
	}()
}
