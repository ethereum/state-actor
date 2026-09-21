package autofill

import (
	"encoding/binary"
	mrand "math/rand"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/ethereum/state-actor/internal/templates"
)

// codePoolSeed is fixed (not --seed) so every client derives the same pool.
const codePoolSeed = 0x57a7ec0de

// corpusByLen is templates.CodeCorpus sorted by length ascending, so poolCode can binary-search
// the members long enough to hold a given draw instead of scanning linearly.
var corpusByLen = func() [][]byte {
	m := append([][]byte(nil), templates.CodeCorpus...)
	sort.Slice(m, func(i, j int) bool { return len(m[i]) < len(m[j]) })
	return m
}()

// poolCode returns code-pool entry j: a CodeSampler-sized window into one real mainnet
// contract from templates.CodeCorpus, with its trailing 8 bytes stamped to a per-j hash.
//
// The member is chosen uniformly among corpus entries at least as long as the drawn size, and
// the window start is a uniform offset within it — never tiled, never spans two contracts, so
// an entry compresses like the one real contract it comes from (about 0.44 deflate), not like
// a repeated pattern.
//
// The stamp exists only so every entry hashes uniquely even when two draws land in the same
// member at the same size (small members can otherwise collide): real bytecode ends in a CBOR
// metadata hash, so replacing the trailing 8 bytes costs nothing in realism or compressibility.
//
// The previous implementation tiled one embedded ERC20 runtime and rotated it per entry. A
// store built from it compressed its code column family to 0.056 physical over logical against
// mainnet's 0.44, and a benchmark against that store read code 46-51% faster than mainnet.
func poolCode(j int, s Sampler) ([]byte, common.Hash) {
	r := mrand.New(mrand.NewSource(codePoolSeed + int64(j)))
	size := int(s.Draw(r))

	start := sort.Search(len(corpusByLen), func(i int) bool { return len(corpusByLen[i]) >= size })
	elig := corpusByLen[start:]
	if len(elig) == 0 {
		// No corpus member reaches the drawn size (only reachable at the very top of the
		// Sampler's range, past the longest embedded contract): fall back to the single
		// longest member, whole, rather than panic on an empty eligible set.
		elig = corpusByLen[len(corpusByLen)-1:]
		size = len(elig[0])
	}
	member := elig[r.Intn(len(elig))]
	off := r.Intn(len(member) - size + 1)
	code := append([]byte(nil), member[off:off+size]...)

	stamp := crypto.Keccak256([]byte("state-actor/code-pool/v2"), binary.BigEndian.AppendUint64(nil, uint64(j)))
	copy(code[len(code)-8:], stamp[:8])

	return code, crypto.Keccak256Hash(code)
}
