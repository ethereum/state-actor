package genesis

import (
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/params"
)

// DefaultFork is the fork-active-at-genesis when --fork is not set.
//
// "prague" is the safe default that geth and reth writers populate
// structurally complete (RequestsHash etc.). Bump to "osaka"/"amsterdam"
// once besu and nethermind writers cover post-Prague header fields (B7
// in the unify-client-features plan).
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
	{"cancun", true, func(c *params.ChainConfig) { c.CancunTime = newUint64Ptr(0) }},
	{"prague", true, func(c *params.ChainConfig) { c.PragueTime = newUint64Ptr(0) }},
	{"osaka", true, func(c *params.ChainConfig) { c.OsakaTime = newUint64Ptr(0) }},
}

// BuildChainConfigForFork synthesizes a *params.ChainConfig with the named
// fork active at genesis (block 0 / time 0). All earlier forks in the
// historical order are also activated at 0, so every IsX(0) predicate
// returns true up to and including the chosen fork.
//
// chainID becomes the chain's only chainID (no override semantics — the
// new --chain-id flag is the source of truth).
//
// Returns an error if name is empty or unknown. Use ListForks() for the
// full set of accepted names; "latest" / "default" alias DefaultFork.
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
	cfg := &params.ChainConfig{ChainID: new(big.Int).Set(chainID)}
	for i := 0; i <= idx; i++ {
		forks[i].apply(cfg)
	}
	return cfg, nil
}

// LatestForkName returns the fork name state-actor defaults to when --fork
// is not set. Tracks DefaultFork; exposed so the CLI help string can
// reflect the current default without duplicating the constant.
func LatestForkName() string { return DefaultFork }

// ListForks returns the canonical fork names in historical activation
// order. Backed directly by the forks slice so changes to the catalogue
// flow through automatically.
func ListForks() []string {
	out := make([]string, len(forks))
	for i, f := range forks {
		out[i] = f.name
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
// Today's ceilings:
//   - geth, reth: prague (writers populate RequestsHash + blob fields)
//   - besu: shanghai (genesis_cgo.supportedFork rejects Cancun+; writer
//     lacks ParentBeaconRoot/ExcessBlobGas/BlobGasUsed/RequestsHash)
//   - nethermind: merge (writer hardcodes a pre-Shanghai header — no
//     WithdrawalsHash, no Cancun+ fields)
//
// Bump after the corresponding writer adds the missing header fields
// (B7 / Stage C in the unify-client-features plan).
func MaxForkForClient(client string) string {
	switch client {
	case "geth", "reth":
		return DefaultFork
	case "besu":
		return "shanghai"
	case "nethermind":
		return "merge"
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
