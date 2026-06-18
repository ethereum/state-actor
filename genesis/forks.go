package genesis

import (
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/params"
)

// DefaultFork is the conservative cross-client fallback fork, used where no
// client context is available: BuildChainConfigForFork("")/LatestForkName and
// the engine-API default. It is intentionally NOT the genesis default — the CLI
// resolves an unset --fork to MaxForkForClient(client), which returns "osaka"
// for every supported client (see MaxForkForClient). "prague" stays the
// context-free fallback because it is the last fork all writers populate
// structurally complete without per-client conditionals.
const DefaultFork = "prague"

// Fork identifies a hard-fork by its lower-case canonical name. Activation
// can be block-based (pre-Merge) or time-based (post-Merge). For genesis
// synthesis the activation value is always 0 (active at block 0 / time 0)
// — earlier forks are pinned to 0 as well so the resulting ChainConfig
// satisfies all `IsX(0)` predicates up to and including the chosen fork.
//
// The order of the slice is the historical activation order; earlier in
// the slice = earlier in mainnet history.
type forkSpec struct {
	name      string
	timeBased bool
	apply     func(cfg *params.ChainConfig)
}

// forks lists every fork state-actor knows how to synthesize. The list is
// authoritative; --list-forks dumps it; BuildChainConfigForFork walks it
// from the start and applies every entry up to and including the chosen
// fork (so "prague" implies "shanghai" implies "london" implies …).
var forks = []forkSpec{
	{"homestead", false, func(c *params.ChainConfig) { c.HomesteadBlock = big.NewInt(0) }},
	{"eip150", false, func(c *params.ChainConfig) { c.EIP150Block = big.NewInt(0) }},
	{"eip155", false, func(c *params.ChainConfig) {
		c.EIP155Block = big.NewInt(0)
		c.EIP158Block = big.NewInt(0)
	}},
	{"byzantium", false, func(c *params.ChainConfig) { c.ByzantiumBlock = big.NewInt(0) }},
	{"constantinople", false, func(c *params.ChainConfig) { c.ConstantinopleBlock = big.NewInt(0) }},
	{"petersburg", false, func(c *params.ChainConfig) { c.PetersburgBlock = big.NewInt(0) }},
	{"istanbul", false, func(c *params.ChainConfig) { c.IstanbulBlock = big.NewInt(0) }},
	{"berlin", false, func(c *params.ChainConfig) { c.BerlinBlock = big.NewInt(0) }},
	{"london", false, func(c *params.ChainConfig) { c.LondonBlock = big.NewInt(0) }},
	{"arrowglacier", false, func(c *params.ChainConfig) { c.ArrowGlacierBlock = big.NewInt(0) }},
	{"grayglacier", false, func(c *params.ChainConfig) { c.GrayGlacierBlock = big.NewInt(0) }},
	{"merge", false, func(c *params.ChainConfig) {
		c.TerminalTotalDifficulty = big.NewInt(0)
		c.MergeNetsplitBlock = big.NewInt(0)
	}},
	{"shanghai", true, func(c *params.ChainConfig) { c.ShanghaiTime = newUint64Ptr(0) }},
	{"cancun", true, func(c *params.ChainConfig) {
		c.CancunTime = newUint64Ptr(0)
		// go-ethereum v1.17.2+ requires BlobScheduleConfig once Cancun
		// is active, otherwise core.SetupGenesisBlock fails with
		// "missing entry for fork \"cancun\" in blobSchedule". Default
		// schedule covers Cancun + Prague + Osaka with mainnet-current
		// target/max/updateFraction values.
		c.BlobScheduleConfig = params.DefaultBlobSchedule
	}},
	{"prague", true, func(c *params.ChainConfig) { c.PragueTime = newUint64Ptr(0) }},
	{"osaka", true, func(c *params.ChainConfig) { c.OsakaTime = newUint64Ptr(0) }},
}

// BuildChainConfigForFork synthesizes a *params.ChainConfig with the named
// fork active at genesis (block 0 / time 0). All earlier forks in the
// historical order are also activated at 0, so every IsX(0) predicate
// returns true up to and including the chosen fork.
//
// User-selectable forks: prague, osaka. Pre-Prague forks are rejected
// at parse time — state-actor only supports current/future mainnet
// configs (PoW + pre-Pectra are EOL). Pre-Prague entries in the forks
// slice stay as cascade-only steps (they apply when a post-Prague fork
// is selected so the resulting ChainConfig is structurally complete).
//
// chainID becomes the chain's only chainID (no override semantics —
// the --chain-id flag is the source of truth).
//
// Returns an error if name is empty, unknown, or pre-Prague.
func BuildChainConfigForFork(name string, chainID *big.Int) (*params.ChainConfig, error) {
	if chainID == nil {
		return nil, fmt.Errorf("genesis: chainID cannot be nil")
	}
	canonical := strings.ToLower(strings.TrimSpace(name))
	if canonical == "" || canonical == "latest" || canonical == "default" {
		canonical = DefaultFork
	}
	idx := -1
	for i, f := range forks {
		if f.name == canonical {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, fmt.Errorf("genesis: unknown fork %q (use --list-forks to see valid names)", name)
	}
	if !ForkAtLeast(canonical, "prague") {
		return nil, fmt.Errorf("genesis: fork %q rejected — state-actor only supports prague and later (PoW + pre-Pectra are EOL)", canonical)
	}
	cfg := &params.ChainConfig{ChainID: new(big.Int).Set(chainID)}
	for i := 0; i <= idx; i++ {
		forks[i].apply(cfg)
	}
	return cfg, nil
}

// LatestForkName returns DefaultFork, the context-free fallback fork (NOT the
// per-client genesis default, which is MaxForkForClient). Exposed so callers
// can reference the fallback without duplicating the constant.
func LatestForkName() string { return DefaultFork }

// ListForks returns the user-selectable fork names in historical
// activation order — prague and later. Pre-Prague forks (PoW +
// pre-Pectra) are EOL and rejected at parse time; not surfaced here.
func ListForks() []string {
	out := make([]string, 0, len(forks))
	for _, f := range forks {
		if !ForkAtLeast(f.name, "prague") {
			continue
		}
		out = append(out, f.name)
	}
	return out
}

// SortedForks returns ListForks() sorted alphabetically. Useful when
// printing for human consumption (CLI --list-forks output).
func SortedForks() []string {
	out := ListForks()
	sort.Strings(out)
	return out
}

// MaxForkForClient returns the highest fork name state-actor's <client>
// writer can faithfully produce at genesis. A --fork value past this
// ceiling should be rejected at parse time so the resulting DB doesn't
// boot with a "wrong genesis hash" mismatch.
//
// Today's ceilings (all 5 clients on Osaka after the writer migration to internal/genesisheader.Build):
//   - geth, reth, besu, nethermind, ethrex: osaka. Header construction flows
//     through internal/genesisheader.Build for besu/reth/nethermind/ethrex
//     (geth uses go-ethereum's native genesis builder, which handles
//     every fork through Osaka identically). Per-client chainspec
//     writers emit shanghaiTime/cancunTime/pragueTime/osakaTime/
//     terminalTotalDifficulty/blobSchedule conditionally based on
//     g.Config — same activation set the writer encodes.
//
// Osaka adds zero new genesis-block fields per go-ethereum v1.17.2
// (Header struct unchanged from Prague's RequestsHash; OsakaTime gates
// only consensus rules like EIP-7691 BPO + EIP-7825 per-tx gas cap).
// Verified by internal/genesisheader/osaka_smoke_test.go.
//
// Bump beyond Osaka (e.g. Amsterdam) once each writer adds the
// corresponding header fields.
func MaxForkForClient(client string) string {
	switch client {
	case "geth", "reth", "besu", "nethermind", "ethrex":
		return "osaka"
	default:
		return DefaultFork
	}
}

// ForkAtLeast reports whether `a` is the same as or later than `b` in the
// historical activation order. Returns false if either name is unknown.
func ForkAtLeast(a, b string) bool {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	ia, ib := -1, -1
	for i, f := range forks {
		if f.name == a {
			ia = i
		}
		if f.name == b {
			ib = i
		}
	}
	if ia < 0 || ib < 0 {
		return false
	}
	return ia >= ib
}

func newUint64Ptr(v uint64) *uint64 { return &v }
