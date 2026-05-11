package oracle

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// canonicalBlockHash is a deterministic mock-EL response hash. The
// driver passes this through; the actual value doesn't matter for the
// test except that it appears in subsequent fc/newPayload calls.
const canonicalBlockHash = "0x1111111111111111111111111111111111111111111111111111111111111111"

// mockEngineEL spins up a httptest.Server that pretends to be a
// post-Merge EL's engine + public RPC ports. Records the methods
// called for assertions; can inject errors via configurable handlers.
type mockEngineEL struct {
	server *httptest.Server

	// methodsSeen tracks which engine methods have been called.
	mu          sync.Mutex
	methodsSeen []string

	// produced counts successful Step iterations (advance fc-no-attrs).
	produced atomic.Int64

	// initialHash is what eth_getBlockByNumber("latest") returns the
	// first time. Defaults to canonicalBlockHash.
	initialHash string

	// payloadIDCounter feeds unique payloadIds to each fc call.
	payloadIDCounter atomic.Int64
}

func newMockEL(t *testing.T) *mockEngineEL {
	t.Helper()
	m := &mockEngineEL{
		initialHash: canonicalBlockHash,
	}
	m.server = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.server.Close)
	return m
}

func (m *mockEngineEL) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Method string `json:"method"`
		Params []any  `json:"params"`
		ID     any    `json:"id"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	m.mu.Lock()
	m.methodsSeen = append(m.methodsSeen, req.Method)
	m.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)

	switch req.Method {
	case "eth_getBlockByNumber":
		enc.Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result": map[string]any{
				"hash":      m.initialHash,
				"timestamp": "0x0",
			},
		})
	case "engine_forkchoiceUpdatedV3":
		// Two flavours: with payloadAttributes (params[1] present) →
		// returns payloadId; without → just OK (advance).
		hasAttrs := len(req.Params) >= 2 && req.Params[1] != nil
		var result map[string]any
		if hasAttrs {
			id := m.payloadIDCounter.Add(1)
			result = map[string]any{
				"payloadStatus": map[string]any{
					"status":          "VALID",
					"latestValidHash": canonicalBlockHash,
				},
				"payloadId": "0x" + strings.Repeat("0", 15) + intToHex(id),
			}
		} else {
			m.produced.Add(1)
			result = map[string]any{
				"payloadStatus": map[string]any{
					"status":          "VALID",
					"latestValidHash": canonicalBlockHash,
				},
				"payloadId": nil,
			}
		}
		enc.Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  result,
		})
	case "engine_getPayloadV3", "engine_getPayloadV4", "engine_getPayloadV5":
		enc.Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result": map[string]any{
				"executionPayload": map[string]any{
					"blockHash":    canonicalBlockHash,
					"blockNumber":  "0x1",
					"timestamp":    "0x1",
					"feeRecipient": engineFeeRecipient,
				},
				"executionRequests": []any{},
				"blockValue":        "0x0",
			},
		})
	case "engine_newPayloadV3", "engine_newPayloadV4", "engine_newPayloadV5":
		enc.Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result": map[string]any{
				"status":          "VALID",
				"latestValidHash": canonicalBlockHash,
			},
		})
	default:
		http.Error(w, "unknown method "+req.Method, http.StatusBadRequest)
	}
}

func (m *mockEngineEL) seen() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.methodsSeen))
	copy(out, m.methodsSeen)
	return out
}

func intToHex(n int64) string {
	const hex = "0123456789abcdef"
	if n == 0 {
		return "0"
	}
	out := []byte{}
	for n > 0 {
		out = append([]byte{hex[n&0xf]}, out...)
		n >>= 4
	}
	return string(out)
}

func TestEngineDriver_Step_OsakaUsesV5GetV4New(t *testing.T) {
	mock := newMockEL(t)
	d := &EngineDriver{
		EngineURL: mock.server.URL,
		EthRPCURL: mock.server.URL,
		Fork:      ForkOsaka,
		BlockTime: 20 * time.Millisecond, // fast for unit test
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	newHash, err := d.Step(ctx, common.HexToHash(canonicalBlockHash), 1)
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if newHash != common.HexToHash(canonicalBlockHash) {
		t.Errorf("newHash = %s, want %s", newHash.Hex(), canonicalBlockHash)
	}

	seen := mock.seen()
	want := []string{
		"engine_forkchoiceUpdatedV3", // fc-with-attrs
		"engine_getPayloadV5",        // Osaka → V5
		"engine_newPayloadV4",        // Osaka uses V4 newPayload (V5 is Amsterdam)
		"engine_forkchoiceUpdatedV3", // fc-advance
	}
	if len(seen) != len(want) {
		t.Fatalf("methods seen = %v, want %v", seen, want)
	}
	for i, m := range want {
		if seen[i] != m {
			t.Errorf("method[%d] = %s, want %s (full: %v)", i, seen[i], m, seen)
		}
	}
}

func TestEngineDriver_Step_PragueUsesV4(t *testing.T) {
	mock := newMockEL(t)
	d := &EngineDriver{
		EngineURL: mock.server.URL,
		EthRPCURL: mock.server.URL,
		Fork:      ForkPrague,
		BlockTime: 20 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, err := d.Step(ctx, common.HexToHash(canonicalBlockHash), 1); err != nil {
		t.Fatalf("Step: %v", err)
	}
	seen := mock.seen()
	for _, w := range []string{"engine_getPayloadV4", "engine_newPayloadV4"} {
		if !slices.Contains(seen, w) {
			t.Errorf("expected %s in methods seen, got %v", w, seen)
		}
	}
}

func TestEngineDriver_Step_CancunUsesV3(t *testing.T) {
	mock := newMockEL(t)
	d := &EngineDriver{
		EngineURL: mock.server.URL,
		EthRPCURL: mock.server.URL,
		Fork:      ForkCancun,
		BlockTime: 20 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, err := d.Step(ctx, common.HexToHash(canonicalBlockHash), 1); err != nil {
		t.Fatalf("Step: %v", err)
	}
	seen := mock.seen()
	for _, w := range []string{"engine_getPayloadV3", "engine_newPayloadV3"} {
		if !slices.Contains(seen, w) {
			t.Errorf("expected %s in methods seen, got %v", w, seen)
		}
	}
}

func TestEngineDriver_DriveLoop_ProducesBlocksUntilCanceled(t *testing.T) {
	mock := newMockEL(t)
	d := &EngineDriver{
		EngineURL: mock.server.URL,
		EthRPCURL: mock.server.URL,
		Fork:      ForkOsaka,
		BlockTime: 40 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.DriveLoop(ctx) }()

	// Let the driver run a few iterations then cancel.
	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("DriveLoop returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("DriveLoop did not exit within 2s of cancel")
	}

	produced := mock.produced.Load()
	if produced < 2 {
		t.Errorf("DriveLoop produced %d blocks in 300ms (BlockTime=40ms); want ≥2", produced)
	}
}

func TestEngineDriver_Step_RejectsNonVALIDStatus(t *testing.T) {
	// Override newPayload to return INVALID — driver must surface as error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct{ Method string }
		json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "engine_forkchoiceUpdatedV3":
			w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"payloadStatus":{"status":"VALID"},"payloadId":"0x01"}}`))
		case "engine_getPayloadV5":
			w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"executionPayload":{"blockHash":"` + canonicalBlockHash + `"},"executionRequests":[]}}`))
		case "engine_newPayloadV4":
			w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"status":"INVALID","validationError":"bad block"}}`))
		}
	}))
	defer srv.Close()

	d := &EngineDriver{
		EngineURL: srv.URL,
		EthRPCURL: srv.URL,
		Fork:      ForkOsaka,
		BlockTime: 10 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	_, err := d.Step(ctx, common.HexToHash(canonicalBlockHash), 1)
	if err == nil {
		t.Fatal("Step should have returned an error for newPayload status=INVALID")
	}
	if !strings.Contains(err.Error(), "INVALID") {
		t.Errorf("error = %v, want it to mention INVALID", err)
	}
}
