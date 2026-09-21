//go:build cgo_besu

// dump-code-corpus samples real contract bytecode out of a Besu Bonsai store's CODE_STORAGE
// column family into internal/templates/corpus.{bin,idx}, the corpus autofill's shared code
// pool is sliced from.
//
// The sample is uniform over distinct bytecodes between 1 KiB and 24 KiB. Two populations are
// excluded on purpose: EIP-7702 delegation designators (23 bytes, below the floor) and
// 24,576-byte contracts, which on a benchmark-bloated store are the benchmark's own fixtures
// and deflate to about 1%. Uniform over bytecodes reproduces mainnet's per-record
// compressibility (0.443 deflate); reuse frequency is the pool's job, so it is not weighted
// here. Every record's key is checked to be keccak256 of its value, so what lands in the
// corpus is real code under its real hash.
//
// Usage (needs the Besu builder image's RocksDB):
//
//	go run -tags cgo_besu ./scripts/dump-code-corpus -db /path/to/besu/database -out internal/templates
package main

import (
	"bytes"
	"compress/flate"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/linxGnu/grocksdb"

	"github.com/ethereum/state-actor/internal/besu/keys"
)

func main() {
	dbPath := flag.String("db", "", "Besu Bonsai RocksDB directory (the one holding CURRENT)")
	out := flag.String("out", "internal/templates", "directory to write corpus.bin and corpus.idx into")
	want := flag.Int("n", 64, "corpus members to keep")
	seed := flag.Int64("seed", 42, "reservoir-sampling seed")
	flag.Parse()
	if *dbPath == "" {
		log.Fatal("-db is required")
	}

	opts := grocksdb.NewDefaultOptions()
	names, err := grocksdb.ListColumnFamilies(opts, *dbPath)
	if err != nil {
		log.Fatalf("list column families: %v", err)
	}
	cfOpts := make([]*grocksdb.Options, len(names))
	for i := range cfOpts {
		cfOpts[i] = opts
	}
	db, handles, err := grocksdb.OpenDbForReadOnlyColumnFamilies(opts, *dbPath, names, cfOpts, false)
	if err != nil {
		log.Fatalf("open read-only: %v", err)
	}
	defer db.Close()
	var code *grocksdb.ColumnFamilyHandle
	for i, n := range names {
		if n == string(keys.CFCodeStorage) {
			code = handles[i]
		}
	}
	if code == nil {
		log.Fatal("no CODE_STORAGE column family")
	}

	// Reservoir sampling (Algorithm R): uniform over the eligible population without
	// holding the column family in memory.
	rng := rand.New(rand.NewSource(*seed))
	var keep [][]byte
	var seen, scanned int64
	ro := grocksdb.NewDefaultReadOptions()
	ro.SetFillCache(false)
	it := db.NewIteratorCF(ro, code)
	for it.SeekToFirst(); it.Valid(); it.Next() {
		scanned++
		k, v := it.Key(), it.Value()
		n := len(v.Data())
		if n < 1<<10 || n > 24<<10 || n == 24576 {
			k.Free()
			v.Free()
			continue
		}
		if !bytes.Equal(k.Data(), crypto.Keccak256(v.Data())) {
			log.Fatalf("record %x is not keyed by its code hash: not a code-hash-keyed CODE_STORAGE", k.Data())
		}
		seen++
		if len(keep) < *want {
			keep = append(keep, bytes.Clone(v.Data()))
		} else if r := rng.Int63n(seen); r < int64(*want) {
			keep[r] = bytes.Clone(v.Data())
		}
		k.Free()
		v.Free()
	}
	if err := it.Err(); err != nil {
		log.Fatalf("iterate: %v", err)
	}
	it.Close()

	var blob bytes.Buffer
	var idx bytes.Buffer
	var comp int
	fmt.Fprintln(&idx, "# offset length keccak256")
	for _, c := range keep {
		fmt.Fprintf(&idx, "%d %d %x\n", blob.Len(), len(c), crypto.Keccak256(c))
		blob.Write(c)
		comp += deflated(c)
	}
	for name, b := range map[string][]byte{"corpus.bin": blob.Bytes(), "corpus.idx": idx.Bytes()} {
		if err := os.WriteFile(filepath.Join(*out, name), b, 0o644); err != nil {
			log.Fatal(err)
		}
	}
	fmt.Printf("scanned %d records, %d eligible, kept %d\n", scanned, seen, len(keep))
	fmt.Printf("corpus bytes %d, mean %d, deflate %.4f (mainnet 0.443)\n",
		blob.Len(), blob.Len()/max(len(keep), 1), float64(comp)/float64(blob.Len()))
}

func deflated(b []byte) int {
	var buf bytes.Buffer
	w, _ := flate.NewWriter(&buf, flate.BestSpeed)
	w.Write(b)
	w.Close()
	return buf.Len()
}
