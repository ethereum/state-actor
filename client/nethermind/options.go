package nethermind

// Options carries Nethermind-specific knobs that don't fit naturally on
// generator.Config. None are currently exposed — the writer always emits the
// flat-state layout — but per-client knobs would live here if any were added.
type Options struct{}
