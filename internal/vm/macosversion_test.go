package vm

import (
	"errors"
	"strings"
	"testing"

	"github.com/bojanrajkovic/runny/internal/tart"
)

func TestParseMacOSMajor(t *testing.T) {
	tests := []struct {
		version string
		want    int
		wantErr bool
	}{
		{version: "26.0", want: 26},
		{version: "26", want: 26},
		{version: "15.6.1", want: 15},
		{version: "27.0", want: 27},
		{version: "", wantErr: true},
		{version: "garbage", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			got, err := parseMacOSMajor(tt.version)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseMacOSMajor(%q): want error, got %d", tt.version, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseMacOSMajor(%q): %v", tt.version, err)
			}
			if got != tt.want {
				t.Errorf("parseMacOSMajor(%q) = %d, want %d", tt.version, got, tt.want)
			}
		})
	}
}

func TestCheckASIFHost(t *testing.T) {
	tests := []struct {
		version string
		// wantSentinel is true only for a genuine below-floor refusal --
		// checkASIFHost wraps tart.ErrASIFUnsupportedOnHost there so it
		// shares one sentinel with Hyper-V's own refusal, but a version
		// string that fails to parse is a different failure entirely, not a
		// host it refused.
		wantSentinel bool
		wantErr      bool
	}{
		{version: "26.0"},
		{version: "27.0"},
		{version: "26"},
		{version: "15.6.1", wantErr: true, wantSentinel: true},
		{version: "", wantErr: true},
		{version: "garbage", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			err := checkASIFHost(tt.version)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("checkASIFHost(%q): %v", tt.version, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkASIFHost(%q): want error, got nil", tt.version)
			}
			if got := errors.Is(err, tart.ErrASIFUnsupportedOnHost); got != tt.wantSentinel {
				t.Errorf("checkASIFHost(%q): errors.Is tart.ErrASIFUnsupportedOnHost = %v, want %v (err: %v)", tt.version, got, tt.wantSentinel, err)
			}
		})
	}
	// A refusal below the floor names both the host's actual version and the
	// floor it fell short of, per the design's error-message requirement.
	err := checkASIFHost("15.6.1")
	if !strings.Contains(err.Error(), "15.6.1") || !strings.Contains(err.Error(), "26") {
		t.Errorf("checkASIFHost(%q) error = %q, want it to name both the host version and the floor", "15.6.1", err)
	}
}
