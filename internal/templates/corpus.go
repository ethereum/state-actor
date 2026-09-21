package templates

import (
	_ "embed"
	"fmt"
	"strconv"
	"strings"
)

// CodeCorpus is real mainnet runtime bytecode: 128 distinct contracts, each a uniform
// reservoir sample from a mainnet Besu Bonsai CODE_STORAGE column family restricted to
// 1 KiB to 24 KiB (24,576-byte records excluded: on a benchmark-bloated store those are the
// benchmark's own fixtures). It exists so that autofill's shared bytecode pool compresses
// like mainnet's code does.
//
// Mainnet code compresses because many *different* contracts repeat across accounts, not
// because bytes repeat inside one contract. A pool built from one contract tiled to size
// compressed its code column family to 0.056 physical over logical, and a benchmark run
// against it read code 46-51% faster than mainnet. Several distinct real contracts, never
// tiled, is what reproduces the mainnet figure.
//
// Reference figures, measured live on a mainnet Besu store (block 24,350,000) with
// Deflater.BEST_SPEED: per-record deflate 0.443 over records >=1 KiB; 32 KiB blocks packed
// from random records 0.415-0.439 across two runs; whole-column-family LZ4 physical over
// logical 0.531. Older notes citing 0.371 for the column family and 0.329 for packed blocks
// did not reproduce and are not targets.
//
// Two independent 64-member samples measured 0.455 and 0.466 whole-member deflate — a
// 64-member sample is inside the noise of the 0.443 target, which is why the corpus is the
// union of both (0.461). Members 0-63 came from scripts/dump-code-corpus (block 24,402,727,
// seed 42); members 64-127 from an earlier reservoir sample of the same population with the
// same filter. Both halves are real mainnet bytecode under their real code hashes. A future
// regeneration should use scripts/dump-code-corpus alone with -n 512 or more; it replaces
// this file wholesale and rotates every auto-fill golden root.
//
// Provenance: each member's keccak256 is its mainnet code hash and is recorded in corpus.idx.
//
//go:embed corpus.bin
var codeCorpusBlob []byte

//go:embed corpus.idx
var codeCorpusIndex string

var CodeCorpus = decodeCodeCorpus(codeCorpusBlob, codeCorpusIndex)

func decodeCodeCorpus(blob []byte, index string) [][]byte {
	var out [][]byte
	for _, line := range strings.Split(index, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		if len(f) < 2 {
			panic(fmt.Sprintf("corpus: bad index line %q", line))
		}
		off, err1 := strconv.Atoi(f[0])
		ln, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil || off < 0 || ln <= 0 || off+ln > len(blob) {
			panic(fmt.Sprintf("corpus: index entry %q outside %d-byte blob", line, len(blob)))
		}
		out = append(out, blob[off:off+ln])
	}
	if len(out) == 0 {
		panic("corpus: empty")
	}
	return out
}
