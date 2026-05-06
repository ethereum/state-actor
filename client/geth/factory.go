package geth

import (
	"context"
	"fmt"

	"github.com/nerolation/state-actor/generator"
)

// init registers state-actor's default writer factory (used by binary-trie
// mode through generator.New) and the default MPT generator (used by MPT
// mode through Generator.Generate).
//
// Importing this package — even with a blank import:
//
//	import _ "github.com/nerolation/state-actor/client/geth"
//
// is enough to make generator.New(cfg).Generate() work without further
// wiring for both modes. main.go and e2e tests opt in this way.
//
// MPT mode: generator.Generator delegates to Populate, which opens its
// own production Pebble + temp scratch and runs the two-phase pipeline
// end-to-end. Generator's own writer field is nil for MPT-mode runs.
//
// Binary-trie mode: generator.NewWithWriter constructs a Writer via the
// factory below; the binary pipeline in generator/binary_stack_trie.go
// drives it directly.
func init() {
	generator.RegisterDefaultWriterFactory(NewWriterFactory())
	generator.RegisterDefaultMPTGenerator(populateForGenerator)
}

// populateForGenerator is the MPTGeneratorFunc registered with the
// generator package. It adapts Populate (which takes a context + Options)
// to the generator's MPTGeneratorFunc signature.
func populateForGenerator(cfg generator.Config) (*generator.Stats, error) {
	return Populate(context.Background(), cfg, Options{})
}

// NewWriterFactory returns a generator.WriterFactory that constructs a geth
// Writer for the given config. Useful for callers that want to pick the
// factory explicitly (e.g. via generator.NewWithWriter) rather than relying
// on the default registered by init().
func NewWriterFactory() generator.WriterFactory {
	return func(cfg generator.Config) (generator.Writer, error) {
		w, err := NewWriter(cfg.DBPath, cfg.BatchSize, cfg.Workers)
		if err != nil {
			return nil, fmt.Errorf("create geth writer: %w", err)
		}
		return w, nil
	}
}
