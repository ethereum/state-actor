package oracle

import (
	"bytes"
	"testing"

	"github.com/holiman/uint256"
)

func TestParseBalance(t *testing.T) {
	tests := []struct {
		in   string
		want *uint256.Int
	}{
		{"0", uint256.NewInt(0)},
		{"1", uint256.NewInt(1)},
		{"1wei", uint256.NewInt(1)},
		{"100gwei", uint256.NewInt(100_000_000_000)},
		{"1ether", uint256.NewInt(1_000_000_000_000_000_000)},
		{"1eth", uint256.NewInt(1_000_000_000_000_000_000)},
		{"  2 Ether ", uint256.NewInt(2_000_000_000_000_000_000)},
		{"0x10", uint256.NewInt(16)},
	}
	for _, tc := range tests {
		got, err := ParseBalance(tc.in)
		if err != nil {
			t.Errorf("ParseBalance(%q): %v", tc.in, err)
			continue
		}
		if got.Cmp(tc.want) != 0 {
			t.Errorf("ParseBalance(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestParseBalance_Errors(t *testing.T) {
	for _, bad := range []string{"", "abc", "-1", "ether"} {
		if _, err := ParseBalance(bad); err == nil {
			t.Errorf("ParseBalance(%q): expected error, got nil", bad)
		}
	}
}

func TestParseCREATE2DeploySpec(t *testing.T) {
	s := "initcode=0xdeadbeef,salt-start=10,salt-count=500"
	got, err := ParseCREATE2DeploySpec(s)
	if err != nil {
		t.Fatalf("ParseCREATE2DeploySpec: %v", err)
	}
	if !bytes.Equal(got.Initcode, []byte{0xde, 0xad, 0xbe, 0xef}) {
		t.Errorf("Initcode = %x, want deadbeef", got.Initcode)
	}
	if got.SaltStart != 10 {
		t.Errorf("SaltStart = %d, want 10", got.SaltStart)
	}
	if got.SaltCount != 500 {
		t.Errorf("SaltCount = %d, want 500", got.SaltCount)
	}
	if got.DeployedCode != nil {
		t.Errorf("DeployedCode should be nil")
	}
}

func TestParseCREATE2DeploySpec_WithOverride(t *testing.T) {
	s := "initcode=0xff,salt-start=0,salt-count=1,deployed-code=0x6000"
	got, err := ParseCREATE2DeploySpec(s)
	if err != nil {
		t.Fatalf("ParseCREATE2DeploySpec: %v", err)
	}
	if !bytes.Equal(got.DeployedCode, []byte{0x60, 0x00}) {
		t.Errorf("DeployedCode = %x, want 6000", got.DeployedCode)
	}
}

func TestParseCREATE2DeploySpec_OmittedSaltStartDefaultsZero(t *testing.T) {
	s := "initcode=0xff,salt-count=1"
	got, err := ParseCREATE2DeploySpec(s)
	if err != nil {
		t.Fatalf("ParseCREATE2DeploySpec: %v", err)
	}
	if got.SaltStart != 0 {
		t.Errorf("SaltStart = %d, want 0", got.SaltStart)
	}
}

func TestParseCREATE2DeploySpec_Errors(t *testing.T) {
	for _, bad := range []string{
		"",                                     // empty
		"initcode=0xff",                        // missing salt-count
		"salt-count=1",                         // missing initcode
		"initcode=0xff,salt-count=1,foo=bar",   // unknown key
		"initcode=0xZZ,salt-count=1",           // bad hex
		"initcode=0xff,salt-count=abc",         // bad uint
		"initcode=0xff,initcode=0xee,salt-count=1", // duplicate
	} {
		if _, err := ParseCREATE2DeploySpec(bad); err == nil {
			t.Errorf("ParseCREATE2DeploySpec(%q): expected error, got nil", bad)
		}
	}
}
