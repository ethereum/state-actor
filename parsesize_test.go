package main

import "testing"

// TestParseSize covers the CLI size parser. Pre-#82-followup it had no
// dedicated tests; the --target-size=0 silent-skip bug surfaced as the
// motivating regression for adding this file. Cases:
//
//   - well-formed suffixed values (case-insensitive)
//   - well-formed plain-bytes
//   - fractional suffixed values
//   - empty / garbage / negative input
//   - zero (suffixed AND plain) — both must error per the post-#82
//     "--target-size must be positive" guard
func TestParseSize(t *testing.T) {
	tests := []struct {
		in      string
		want    uint64
		wantErr bool
	}{
		{"5GB", 5 << 30, false},
		{"500MB", 500 << 20, false},
		{"1024", 1024, false},
		{"1tb", 1 << 40, false},      // case-insensitive
		{"1KB", 1 << 10, false},
		{"5.5GB", uint64(5.5 * (1 << 30)), false},
		{"0", 0, true},               // plain-bytes 0 rejected
		{"0GB", 0, true},              // per-suffix rejection
		{"-5GB", 0, true},
		{"abc", 0, true},
		{"", 0, true},                 // empty rejected by ParseUint
		{"5XB", 0, true},              // unknown suffix → ParseUint fails
	}
	for _, tc := range tests {
		got, err := parseSize(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseSize(%q) err=%v, wantErr=%v", tc.in, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && got != tc.want {
			t.Errorf("parseSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
