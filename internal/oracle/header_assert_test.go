package oracle

import (
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nerolation/state-actor/genesis"
)

// mockHeaderEL serves the JSON-RPC subset AssertGenesisHeaderMatches reads:
// eth_chainId and eth_getBlockByNumber("0x0"). Field values are returned
// verbatim from the struct, so a test can drive any combination of
// agreement and divergence.
type mockHeaderEL struct {
	chainID   string // hex with "0x" prefix
	gasLimit  string
	timestamp string
	extraData string
}

func (m *mockHeaderEL) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Method string `json:"method"`
			ID     int    `json:"id"`
		}
		_ = json.Unmarshal(body, &req)

		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "eth_chainId":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":%q}`, req.ID, m.chainID)
		case "eth_getBlockByNumber":
			// BlockByNumber rejects zero stateRoot and missing Number; fake
			// both with stable non-zero values so the call returns.
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{`+
				`"number":"0x0",`+
				`"hash":"0xaa00000000000000000000000000000000000000000000000000000000000000",`+
				`"stateRoot":"0xbb00000000000000000000000000000000000000000000000000000000000000",`+
				`"parentHash":"0x0000000000000000000000000000000000000000000000000000000000000000",`+
				`"timestamp":%q,`+
				`"gasLimit":%q,`+
				`"extraData":%q`+
				`}}`, req.ID, m.timestamp, m.gasLimit, m.extraData)
		default:
			http.Error(w, "method not mocked: "+req.Method, http.StatusBadRequest)
		}
	}
}

func startMockHeaderEL(t *testing.T, chainID, gasLimit, timestamp, extraData string) string {
	t.Helper()
	m := &mockHeaderEL{chainID: chainID, gasLimit: gasLimit, timestamp: timestamp, extraData: extraData}
	srv := httptest.NewServer(m.handler())
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestAssertGenesisHeaderMatches_AllFieldsAgree(t *testing.T) {
	g, err := genesis.BuildSynthetic("osaka", big.NewInt(1337), 20_000_000, 1_700_000_000, []byte{0xde, 0xad, 0xbe, 0xef})
	if err != nil {
		t.Fatalf("BuildSynthetic: %v", err)
	}
	// Hex equivalents: 1337=0x539, 20M=0x1312d00, 1700000000=0x6553f100, extraData="0xdeadbeef".
	url := startMockHeaderEL(t, "0x539", "0x1312d00", "0x6553f100", "0xdeadbeef")
	// Agreement case — t.Errorf isn't called. If the helper has a bug that
	// reports a divergence here, the subtest fails.
	AssertGenesisHeaderMatches(t, url, g)
}
