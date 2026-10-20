package engineapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// EngineDriver emulates a consensus layer over the engine API so a
// post-Merge EL with no native dev consensus can produce blocks. Used
// by client/besu/e2e_test.go — geth/reth/nethermind have built-in dev
// modes and don't need this. Modeled after besu's own
// PragueAcceptanceTestHelper.java (acceptance-tests/tests/src/
// acceptanceTest/java/.../PragueAcceptanceTestHelper.java) which drives
// engine API the same way from besu's integration tests.
//
// Each iteration of DriveLoop produces ONE block:
//
//  1. engine_forkchoiceUpdatedV{F} with payloadAttributes → payloadId
//  2. wait BlockTime/2 (lets the EL assemble the payload)
//  3. engine_getPayloadV{N} → executionPayload + executionRequests
//  4. engine_newPayloadV{M} → asserts status="VALID"
//  5. engine_forkchoiceUpdatedV{F} without attributes → advances head
//
// Method-version dispatch by Fork (verified against besu 25.11.0
// ForkSupportHelper bounds — see EngineGetPayloadV{4,5}.java +
// EngineNewPayloadV{4,5}.java validateForkSupported):
//
//	cancun: fcUpdatedV3 + getPayloadV3 + newPayloadV3
//	prague: fcUpdatedV3 + getPayloadV4 + newPayloadV4
//	osaka:  fcUpdatedV3 + getPayloadV5 + newPayloadV4 — besu's V5
//	        newPayload requires Amsterdam (blockAccessList field), so
//	        Osaka chains use V4 newPayload which covers [Prague, Amsterdam).
//	        getPayloadV5 covers [Osaka, Amsterdam) — V4 stops at Osaka.
//	amsterdam: fcUpdatedV4 + getPayloadV6 + newPayloadV5 — per
//	        go-ethereum v1.17.6 (eth/catalyst). fcUpdatedV4 requires
//	        slotNumber (EIP-7843) in payloadAttributes. The block access
//	        list (EIP-7928) rides inside executionPayload, so the driver
//	        passes getPayload's payload to newPayload unchanged.
//
// JWT is supported: set JWTSecret to the raw 32-byte HMAC key
// (parsed from the 64-hex-char file the EL is configured against).
// When non-empty, every callEngine builds a fresh HS256 token with
// iat=now and attaches it as `Authorization: Bearer <token>`. When
// empty, no auth header is set (use with --engine-jwt-disabled).
type EngineDriver struct {
	// EngineURL is the EL's engine-RPC endpoint, e.g. http://<host>:8551.
	EngineURL string

	// EthRPCURL is the EL's public JSON-RPC endpoint, e.g.
	// http://<host>:8545. Used once at start to bootstrap the head
	// hash + timestamp from eth_getBlockByNumber("latest").
	EthRPCURL string

	// BlockTime is the inter-block delay. The EL gets BlockTime/2 to
	// build the payload between forkchoiceUpdated and getPayload, then
	// the loop sleeps BlockTime/2 again before the next block. Total
	// effective block interval ≈ BlockTime. Zero → 1s default.
	BlockTime time.Duration

	// Fork picks the engine method versions. Use one of the package
	// constants (ForkCancun / ForkPrague / ForkOsaka); empty defaults
	// to ForkPrague. Unknown values cause versions() to return
	// Prague-style methods (back-compat), but new callers should use
	// the typed constants to avoid the silent-default footgun.
	Fork Fork

	// JWTSecret is the raw 32-byte HMAC-SHA256 key the EL was booted
	// with via --authrpc.jwtsecret (reth) or equivalent. When non-empty,
	// every engine API call signs a fresh JWT (iat=now) and sets
	// Authorization: Bearer. Empty disables JWT for ELs that boot with
	// --engine-jwt-disabled (besu, nethermind).
	JWTSecret []byte

	// httpClient is lazily created with a 30s timeout on first call.
	httpClient *http.Client
}

// Fork is the engine-API method-version dispatch key. Typed (rather
// than bare string) so a typo like "osakaa" is a compile error instead
// of silently falling into the Prague default at runtime.
type Fork string

const (
	ForkCancun Fork = "cancun"
	ForkPrague Fork = "prague"
	ForkOsaka  Fork = "osaka"
	// ForkAmsterdam is the Sepolia Amsterdam spec (go-ethereum v1.17.6).
	ForkAmsterdam Fork = "amsterdam"
)

// engineFeeRecipient is the suggestedFeeRecipient passed to every
// forkchoiceUpdated call. Block fees stay on this address; we don't
// care about the destination since spamoor uses its own funded sender.
const engineFeeRecipient = "0x0000000000000000000000000000000000000000"

// zeroHash32 is the hex form of the all-zero 32-byte hash, used for
// prevRandao + parentBeaconBlockRoot. PoS dev mode treats randao as
// pure entropy; zero is fine.
const zeroHash32 = "0x0000000000000000000000000000000000000000000000000000000000000000"

// DriveLoop runs Step in a loop until ctx is canceled. Returns ctx.Err()
// on cancellation. Transient Step errors are logged via the configured
// logger (or stderr by default) and retried after BlockTime — startup
// races shouldn't kill the driver.
func (d *EngineDriver) DriveLoop(ctx context.Context) error {
	d.ensureHTTPClient()

	head, err := d.fetchLatestBlock(ctx)
	if err != nil {
		return fmt.Errorf("EngineDriver: fetch initial head: %w", err)
	}
	headHash := head.Hash
	blockTS := head.Time
	slot := head.SlotNumber

	// Permanent-rejection guard: if Step fails this many times in a row
	// (no recovery in between), surface the latest error instead of
	// spinning forever. Spamoor's 5-min deadline would otherwise be the
	// only signal, mis-attributing chain rejection to "spamoor too slow".
	const maxConsecutiveStepErrors = 5
	consecutiveErrors := 0

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		blockTS++
		newHash, stepErr := d.Step(ctx, headHash, blockTS, slot+1)
		if stepErr == nil {
			headHash = newHash
			slot++
			consecutiveErrors = 0
		} else {
			consecutiveErrors++
			if consecutiveErrors >= maxConsecutiveStepErrors {
				return fmt.Errorf("EngineDriver: %d consecutive Step failures (chain rejecting payloads?); last err: %w",
					consecutiveErrors, stepErr)
			}
		}
		// Sleep regardless of success — failure path is "EL not ready,
		// try again next tick"; success path is "block produced, wait
		// for next slot".
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d.blockTime() / 2):
		}
	}
}

// Step drives one block-production iteration. Returns the new tip's
// blockHash on success. Caller's responsibility to advance the
// timestamp monotonically — re-using a parent's ts will cause besu to
// reject the payload. slot is the EIP-7843 slotNumber of the new block;
// only Amsterdam+ sends it (payloadAttributes.slotNumber).
func (d *EngineDriver) Step(ctx context.Context, headHash common.Hash, ts, slot uint64) (common.Hash, error) {
	d.ensureHTTPClient()

	fcVer, getVer, newVer := d.versions()

	// 1. forkchoiceUpdated with payloadAttributes → payloadId.
	//
	// Erigon v3.4.x returns SYNCING on the FIRST FCU after daemon boot
	// because `exec_module.go:649-668 Ready()` polls
	// `blockReader.Ready(ctx, 1s)` and the in-memory `s.ready` signal
	// only fires after `SpawnStageSnapshots` runs its first cycle. Erigon's
	// own MockCl mirrors this by retrying FCU on SYNCING with 50 ms
	// backoff (execution/engineapi/engineapitester/mock_cl.go:289-321).
	// We follow the same pattern: retry until VALID or 60 s elapsed
	// (well past Erigon's first-cycle latency).
	attrs := map[string]any{
		"timestamp":             fmt.Sprintf("0x%x", ts),
		"prevRandao":            zeroHash32,
		"suggestedFeeRecipient": engineFeeRecipient,
		"withdrawals":           []any{},
		"parentBeaconBlockRoot": zeroHash32,
	}
	if d.isAmsterdam() {
		attrs["slotNumber"] = fmt.Sprintf("0x%x", slot)
	}
	fcParams := []any{
		map[string]string{
			"headBlockHash":      headHash.Hex(),
			"safeBlockHash":      headHash.Hex(),
			"finalizedBlockHash": headHash.Hex(),
		},
		attrs,
	}
	var fcResp map[string]any
	var payloadID string
	{
		retryCtx, retryCancel := context.WithTimeout(ctx, 60*time.Second)
		defer retryCancel()
		for {
			resp, err := d.callEngine(retryCtx, fcVer, fcParams)
			if err != nil {
				return common.Hash{}, fmt.Errorf("fc with attrs: %w", err)
			}
			status := fcResponseStatus(resp)
			if status == "SYNCING" {
				select {
				case <-retryCtx.Done():
					return common.Hash{}, fmt.Errorf("fc with attrs: still SYNCING after 60s — daemon never opened RoSnapshots.ready")
				case <-time.After(50 * time.Millisecond):
					continue
				}
			}
			pid, ok := resp["payloadId"].(string)
			if !ok || pid == "" {
				return common.Hash{}, fmt.Errorf("fc with attrs: missing payloadId in response (status=%q)", status)
			}
			fcResp = resp
			payloadID = pid
			break
		}
	}
	_ = fcResp // referenced via payloadID + status branch

	// 2. Wait for the EL to assemble the payload. besu's
	// PragueAcceptanceTestHelper hardcodes 500ms; we scale with BlockTime
	// so 1s blocks → 500ms build window.
	select {
	case <-ctx.Done():
		return common.Hash{}, ctx.Err()
	case <-time.After(d.blockTime() / 2):
	}

	// 3. getPayload → executionPayload + executionRequests
	getResp, err := d.callEngine(ctx, getVer, []any{payloadID})
	if err != nil {
		return common.Hash{}, fmt.Errorf("getPayload: %w", err)
	}
	execPayload, ok := getResp["executionPayload"].(map[string]any)
	if !ok {
		return common.Hash{}, fmt.Errorf("getPayload: missing executionPayload")
	}
	execRequests, _ := getResp["executionRequests"].([]any)
	if execRequests == nil {
		execRequests = []any{}
	}
	newBlockHashStr, ok := execPayload["blockHash"].(string)
	if !ok || newBlockHashStr == "" {
		return common.Hash{}, fmt.Errorf("getPayload: missing blockHash in executionPayload")
	}

	// 4. newPayload(executionPayload, [], parentBeaconBlockRoot, executionRequests)
	npParams := []any{
		execPayload,
		[]any{}, // expectedBlobVersionedHashes — empty (spamoor doesn't send blobs)
		zeroHash32,
		execRequests,
	}
	npResp, err := d.callEngine(ctx, newVer, npParams)
	if err != nil {
		return common.Hash{}, fmt.Errorf("newPayload: %w", err)
	}
	status, _ := npResp["status"].(string)
	if status != "VALID" {
		return common.Hash{}, fmt.Errorf("newPayload status=%q (want VALID); resp=%v", status, npResp)
	}

	// 5. forkchoiceUpdated without attributes → advances the canonical head.
	advanceParams := []any{
		map[string]string{
			"headBlockHash":      newBlockHashStr,
			"safeBlockHash":      newBlockHashStr,
			"finalizedBlockHash": newBlockHashStr,
		},
	}
	if _, err := d.callEngine(ctx, fcVer, advanceParams); err != nil {
		return common.Hash{}, fmt.Errorf("fc advance: %w", err)
	}
	return common.HexToHash(newBlockHashStr), nil
}

// fcResponseStatus extracts payloadStatus.status from an
// engine_forkchoiceUpdated response. Returns "" if absent.
func fcResponseStatus(resp map[string]any) string {
	ps, ok := resp["payloadStatus"].(map[string]any)
	if !ok {
		return ""
	}
	s, _ := ps["status"].(string)
	return s
}

func (d *EngineDriver) fork() Fork {
	return Fork(strings.ToLower(strings.TrimSpace(string(d.Fork))))
}

func (d *EngineDriver) isAmsterdam() bool { return d.fork() == ForkAmsterdam }

func (d *EngineDriver) versions() (fcVer, getVer, newVer string) {
	switch d.fork() {
	case ForkCancun:
		return "engine_forkchoiceUpdatedV3", "engine_getPayloadV3", "engine_newPayloadV3"
	case ForkOsaka:
		// besu 25.11.0 ForkSupportHelper bounds:
		//   getPayloadV5: [Osaka, Amsterdam)
		//   newPayloadV5: [Amsterdam, ...)  ← requires blockAccessList field
		//   newPayloadV4: [Prague, Amsterdam) ← accepts Osaka payloads
		return "engine_forkchoiceUpdatedV3", "engine_getPayloadV5", "engine_newPayloadV4"
	case ForkAmsterdam:
		// go-ethereum v1.17.6: fcuV4 is Amsterdam-only and requires
		// slotNumber; getPayloadV6 and newPayloadV5 carry the
		// blockAccessList and slotNumber payload fields.
		return "engine_forkchoiceUpdatedV4", "engine_getPayloadV6", "engine_newPayloadV5"
	default:
		// Prague (and unrecognized — matches state-actor's DefaultFork).
		return "engine_forkchoiceUpdatedV3", "engine_getPayloadV4", "engine_newPayloadV4"
	}
}

func (d *EngineDriver) blockTime() time.Duration {
	if d.BlockTime <= 0 {
		return time.Second
	}
	return d.BlockTime
}

// engineJWT builds an HS256-signed engine-API token with iat=now.
//
// The engine-API spec (https://github.com/ethereum/execution-apis/blob/
// main/src/engine/authentication.md) requires:
//   - alg: HS256, typ: JWT (the only mandatory header claims)
//   - iat: issued-at unix timestamp, within ±60 s of the EL's clock
//   - secret: the raw 32-byte HMAC-SHA256 key (NOT hex-decoded inside
//     the signer — the caller is responsible for parsing the 64-hex-
//     char file the EL was configured against)
//
// Other JWT claims (exp, nbf, etc.) are not required and not set —
// keeping the token small minimises wire overhead per engine call.
func engineJWT(secret []byte) (string, error) {
	header := []byte(`{"alg":"HS256","typ":"JWT"}`)
	payload, err := json.Marshal(map[string]any{"iat": time.Now().Unix()})
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	headerB64 := enc.EncodeToString(header)
	payloadB64 := enc.EncodeToString(payload)
	signingInput := headerB64 + "." + payloadB64
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signingInput))
	sig := mac.Sum(nil)
	return signingInput + "." + enc.EncodeToString(sig), nil
}

func (d *EngineDriver) ensureHTTPClient() {
	if d.httpClient == nil {
		d.httpClient = &http.Client{Timeout: 30 * time.Second}
	}
}

// callEngine POSTs a JSON-RPC request to EngineURL and unmarshals the
// "result" field. Errors from the JSON-RPC layer ({"error":{...}}) are
// surfaced as Go errors with the original code+message.
func (d *EngineDriver) callEngine(ctx context.Context, method string, params []any) (map[string]any, error) {
	reqBody, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
		"id":      1,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.EngineURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	// Engine-API JWT auth: when the EL is booted with --authrpc.jwtsecret
	// (reth requires this), every engine call must carry an HS256 JWT
	// signed with the raw secret. iat must be within ±60 s of the EL's
	// clock per the spec — generate fresh per call so we never edge out
	// of tolerance on slow consumers.
	if len(d.JWTSecret) > 0 {
		tok, terr := engineJWT(d.JWTSecret)
		if terr != nil {
			return nil, fmt.Errorf("engine jwt: %w", terr)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(respBody, 200))
	}

	var parsed struct {
		Result map[string]any `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("unmarshal %s: %w (body=%s)", method, err, truncate(respBody, 200))
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("%s: rpc error %d: %s", method, parsed.Error.Code, parsed.Error.Message)
	}
	if parsed.Result == nil {
		return nil, fmt.Errorf("%s: empty result", method)
	}
	return parsed.Result, nil
}

// engineHeadInfo is the subset of eth_getBlockByNumber("latest") that
// DriveLoop needs to bootstrap. SlotNumber is 0 when the head has no
// slotNumber field (pre-Amsterdam).
type engineHeadInfo struct {
	Hash       common.Hash
	Time       uint64
	SlotNumber uint64
}

// fetchLatestBlock reads the current head's hash + timestamp from
// EthRPCURL via eth_getBlockByNumber("latest", false).
func (d *EngineDriver) fetchLatestBlock(ctx context.Context) (engineHeadInfo, error) {
	reqBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "eth_getBlockByNumber",
		"params":  []any{"latest", false},
		"id":      1,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.EthRPCURL, bytes.NewReader(reqBody))
	if err != nil {
		return engineHeadInfo{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.httpClient.Do(req)
	if err != nil {
		return engineHeadInfo{}, fmt.Errorf("eth_getBlockByNumber: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var parsed struct {
		Result struct {
			Hash       string `json:"hash"`
			Timestamp  string `json:"timestamp"`
			SlotNumber string `json:"slotNumber"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return engineHeadInfo{}, fmt.Errorf("unmarshal eth_getBlockByNumber: %w (body=%s)", err, truncate(body, 200))
	}
	if parsed.Result.Hash == "" {
		return engineHeadInfo{}, fmt.Errorf("eth_getBlockByNumber returned no hash (body=%s)", truncate(body, 200))
	}

	ts, err := parseHexUint64(parsed.Result.Timestamp)
	if err != nil {
		return engineHeadInfo{}, fmt.Errorf("parse timestamp %q: %w", parsed.Result.Timestamp, err)
	}
	slot, err := parseHexUint64(parsed.Result.SlotNumber)
	if err != nil {
		return engineHeadInfo{}, fmt.Errorf("parse slotNumber %q: %w", parsed.Result.SlotNumber, err)
	}
	return engineHeadInfo{
		Hash:       common.HexToHash(parsed.Result.Hash),
		Time:       ts,
		SlotNumber: slot,
	}, nil
}

// parseHexUint64 parses a 0x-prefixed quantity; "" (field absent) is 0.
func parseHexUint64(s string) (uint64, error) {
	s = strings.TrimPrefix(s, "0x")
	if s == "" {
		return 0, nil
	}
	return strconv.ParseUint(s, 16, 64)
}

// truncate returns up to n bytes of b for error messages, avoiding
// gigabyte-sized log lines on misbehaving servers.
func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
