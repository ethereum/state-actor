package oracle

import "github.com/ethereum/go-ethereum/common"

// SpamoorSenderAddr / SpamoorSenderPrivKey are the conventional dev key 1
// (privkey = 0x000…001 → addr = 0x7e5f4552…). state-actor pre-funds the
// address via cfg.InjectAddresses; spamoor uses the privkey as deployer.
//
// Must match SMOKE_INJECT_ADDRS[0] in Makefile (the local smoke targets
// inject the same address). Rotating the dev key requires updating both
// places — the per-client e2e tests now consume these constants instead
// of duplicating the literals 4×, so the desync risk is contained to
// "match this one place to the Makefile".
var (
	SpamoorSenderAddr    = common.HexToAddress("0x7e5f4552091a69125d5dfcb7b8c2659029395bdf")
	SpamoorSenderPrivKey = "0x0000000000000000000000000000000000000000000000000000000000000001"
)
