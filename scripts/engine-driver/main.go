// Command engine-driver is a standalone CL mock that drives the engine
// API of a running execution-layer client (besu / nethermind / reth /
// erigon) in dev mode. Wraps internal/engineapi.EngineDriver.
//
// Usage:
//
//	engine-driver \
//	  -engine http://127.0.0.1:8551 \
//	  -eth http://127.0.0.1:8545 \
//	  -fork osaka \
//	  -block-time 1s \
//	  [-jwt /path/to/jwt.hex]
//
// Runs DriveLoop until SIGINT/SIGTERM. JWT is optional — required by
// erigon (which has no --engine-jwt-disabled flag), unused by besu /
// nethermind (which boot with --engine-jwt-disabled).
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	engineapi "github.com/ethereum/state-actor/internal/engineapi"
)

func main() {
	engineURL := flag.String("engine", "http://127.0.0.1:8551", "engine API URL")
	ethURL := flag.String("eth", "http://127.0.0.1:8545", "JSON-RPC URL")
	fork := flag.String("fork", "osaka", "engine method version: cancun | prague | osaka | amsterdam")
	blockTime := flag.Duration("block-time", time.Second, "slot duration")
	jwtPath := flag.String("jwt", "", "path to JWT secret hex file (32 random bytes); empty disables JWT")
	flag.Parse()

	var jwtSecret []byte
	if *jwtPath != "" {
		raw, err := os.ReadFile(*jwtPath)
		if err != nil {
			log.Fatalf("read JWT secret %q: %v", *jwtPath, err)
		}
		// Allow whitespace / "0x" prefix.
		s := strings.TrimSpace(string(raw))
		s = strings.TrimPrefix(s, "0x")
		jwtSecret, err = hex.DecodeString(s)
		if err != nil {
			log.Fatalf("decode JWT secret %q: %v", *jwtPath, err)
		}
		if len(jwtSecret) != 32 {
			log.Fatalf("JWT secret must be 32 bytes, got %d", len(jwtSecret))
		}
		log.Printf("engine-driver: JWT enabled (32-byte secret loaded from %s)", *jwtPath)
	}

	driver := &engineapi.EngineDriver{
		EngineURL: *engineURL,
		EthRPCURL: *ethURL,
		BlockTime: *blockTime,
		Fork:      engineapi.Fork(*fork),
		JWTSecret: jwtSecret,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Printf("signal received, shutting down driver")
		cancel()
	}()

	log.Printf("engine-driver: engine=%s eth=%s fork=%s block-time=%s",
		*engineURL, *ethURL, *fork, *blockTime)
	if err := driver.DriveLoop(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("driver exited with error: %v", err)
	}
	log.Printf("driver shut down cleanly")
}
