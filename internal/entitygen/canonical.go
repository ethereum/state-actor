package entitygen

import "github.com/ethereum/go-ethereum/common"

// CanonicalOsakaMPTRoot is the cross-client MPT state root for the
// canonical e2e config: 10 entitygen EOAs + 5 entitygen contracts
// (seed=12345, PowerLaw, MinSlots=1, MaxSlots=100, CodeSize=256) PLUS
// the 4 EIP system contracts (BeaconRoots, HistoryStorage,
// WithdrawalQueue, ConsolidationQueue) deployed at their canonical
// addresses (nonce=1, Balance=0, Root=EmptyRootHash, CodeHash=keccak256
// of the bytecodes from go-ethereum/params/protocol_params.go).
//
// Every MPT-mode client adapter (geth, nethermind, besu, reth) MUST
// produce exactly this hash when run with the matching config — same
// RNG draws + same system-contract injection → same state → same root.
// Drift here means a coordinated update across all 4 client adapters
// is required.
//
// This hash is the "Osaka-bootable cross-client invariant" — strictly
// stronger than the entitygen-entities-only hash because it covers
// the actual chain shape used by the e2e suites (besu refuses to boot
// without these contracts; the other 3 silently no-op the system
// calls, masking divergence). The 4 per-client golden tests + the
// pure-Go canonical_mpt_test all pin against this constant.
//
// To update (when intentional): run any one client's golden test,
// capture the new hash from the failure message, paste here. Then
// all 4 client tests + canonical_mpt_test will agree on the new value.
var CanonicalOsakaMPTRoot = common.HexToHash("0x015874dcec57915518fd4d98f98d8543ce2a8cbbee4bc8fa714e43ce055ed32a")
