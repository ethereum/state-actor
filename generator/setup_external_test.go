// Package generator_test exists solely to host side-effect imports needed by
// the generator package's test binary. Putting them here (in the external
// test package) instead of in package generator avoids a generator →
// client/geth → generator import cycle while still triggering client/geth's
// init() registration before any test runs.
package generator_test

// Importing client/geth registers it as the default WriterFactory via
// generator.RegisterDefaultWriterFactory. After this init() runs,
// generator.New(cfg) succeeds for tests that don't supply an explicit factory.
import (
	_ "github.com/ethereum/state-actor/client/geth"
)
