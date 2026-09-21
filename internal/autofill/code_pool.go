package autofill

import (
	mrand "math/rand"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/ethereum/state-actor/internal/templates"
)

// codePoolSeed is fixed (not --seed) so every client derives the same pool.
const codePoolSeed = 0x57a7ec0de

// poolCode returns code-pool entry j: a CodeSampler-sized slice of real mainnet bytecode.
//
// Entry j starts in corpus member j % N at an offset that also advances with j, and when the
// member runs out it continues into the next member rather than wrapping back into the same
// one, so no byte sequence repeats inside an entry and an entry compresses like real code
// (about 0.45 deflate), not like a tiled runtime (about 0.2). Spreading entries evenly over
// the members keeps two entries from the same member rare within one data block; the store
// orders code by hash, so a block holds a random handful of entries.
//
// The previous implementation tiled one ERC20 runtime and rotated it per entry. A store built
// from it compressed its code column family to 0.056 physical over logical against mainnet's
// 0.371, and a benchmark against that store read code 46% faster than mainnet.
func poolCode(j int, s Sampler) ([]byte, common.Hash) {
	corpus := templates.CodeCorpus
	code := make([]byte, s.Draw(mrand.New(mrand.NewSource(codePoolSeed+int64(j)))))
	member := j % len(corpus)
	// A stride coprime with typical member lengths spreads same-member entries across the
	// member instead of sharing a prefix.
	off := (j / len(corpus) * 4099) % len(corpus[member])
	for n := 0; n < len(code); {
		n += copy(code[n:], corpus[member][off:])
		member, off = (member+1)%len(corpus), 0
	}
	return code, crypto.Keccak256Hash(code)
}
