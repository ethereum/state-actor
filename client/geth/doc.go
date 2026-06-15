// Package geth implements the geth-format state writer for state-actor.
//
// The package writes Pebble-encoded snapshot ("a", "o", "c") entries plus
// PathDB metadata and freezer init, producing a database directly bootable
// by a stock geth node — no `geth init` required. WriteGenesisBlock embeds
// the chain config + alloc spec into the database so geth's first-open
// path validates against the persisted state without re-running genesis.
//
// # Wiring into Generator
//
// The package's init() registers itself as the default Writer factory. A
// blank import is therefore sufficient for state-actor's CLI (geth path):
//
//	import _ "github.com/ethereum/state-actor/client/geth"
//	gen, err := generator.New(cfg) // uses geth writer
//
// To select the factory explicitly (e.g. when multiple client packages are
// imported) use NewWriterFactory:
//
//	import "github.com/ethereum/state-actor/client/geth"
//	gen, err := generator.NewWithWriter(cfg, geth.NewWriterFactory())
//
// # Layering
//
// This package depends on:
//   - github.com/ethereum/state-actor/generator (for Config, Writer interface, WriterStats)
//   - github.com/ethereum/state-actor/genesis   (for the Genesis JSON parser type)
//
// The generator and genesis packages do NOT import this package — that
// would create a generator → client/geth → generator import cycle. Tests
// in package generator that need a registered writer factory pull this
// package in via a blank import.
package geth
