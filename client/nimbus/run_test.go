//go:build !cgo_nimbus

package nimbus

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ethereum/state-actor/generator"
	"github.com/ethereum/state-actor/internal/autofill"
)

// TestRun_StubReturnsNotImplemented pins the !cgo_nimbus build behavior:
// Run returns a clearly-labeled error directing the user at Docker so
// `--client=nimbus` on a vanilla `go build` doesn't panic, return nil, or
// silently no-op. Mirrors client/erigon/run_test.go.
func TestRun_StubReturnsNotImplemented(t *testing.T) {
	plan, err := autofill.PlanForBudget(512 << 10)
	if err != nil {
		t.Fatalf("PlanForBudget: %v", err)
	}
	cfg := generator.Config{AutoFill: plan, Seed: 1}
	stats, err := Run(context.Background(), cfg, Options{})
	if err == nil {
		t.Fatal("Run returned nil error; expected stub error")
	}
	if !errors.Is(err, errNotImplemented) {
		t.Fatalf("expected errNotImplemented, got %v", err)
	}
	if stats != nil {
		t.Errorf("expected nil stats from stub, got %#v", stats)
	}
	if !strings.Contains(err.Error(), "Docker") {
		t.Errorf("error text should mention Docker: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "cgo_nimbus") {
		t.Errorf("error text should mention cgo_nimbus: %q", err.Error())
	}
}
