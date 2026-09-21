package templates

import (
	_ "embed"
	"fmt"
	"strconv"
	"strings"
)

// CodeCorpus is real mainnet runtime bytecode, 64 contracts sampled uniformly from a
// mainnet snapshot's code column family (block 24,402,727), restricted to 1 KiB to 24 KiB.
// It exists so that autofill's shared bytecode pool compresses like mainnet's code does.
//
// Mainnet code compresses (about 0.37 physical over logical under LZ4) because many
// *different* contracts repeat across accounts. A pool built from one contract tiled to size
// compresses to 0.056, six times too well, and a benchmark run against it reads code six times
// too cheaply. Several distinct real contracts, never tiled, is what reproduces the mainnet
// figure; a sample of 64 measured 0.4285 per-record deflate against mainnet's 0.443.
//
// Provenance: each member's keccak256 is its mainnet code hash and is recorded in corpus.idx.
// Regenerate with scripts/dump-code-corpus.
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
