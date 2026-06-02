package ethrex

// Options carries ethrex-specific knobs that don't fit naturally on
// generator.Config (which is shared across client backends).
//
// Reserved for future post-write boot validation wiring. runImpl ignores
// all fields today; they are still parsed so callers can pre-set them.
type Options struct {
	// EthrexBin is an absolute path to an ethrex binary to spawn for a
	// post-write boot validation. Empty disables the validation.
	EthrexBin string

	// SkipBootValidation skips the post-write ethrex boot smoke.
	SkipBootValidation bool
}
