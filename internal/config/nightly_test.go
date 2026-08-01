package config

import (
	"testing"
	"time"
)

// The nightly scan's clock parsing (M16). One scan a night is the only scheduled job in the
// application, so the one knob it has should be hard to get wrong.
func TestEnvClock(t *testing.T) {
	cases := []struct {
		raw     string
		set     bool
		want    time.Duration
		wantErr bool
	}{
		{set: false, want: 5 * time.Hour},              // unset: the default
		{raw: "05:00", set: true, want: 5 * time.Hour}, //
		{raw: "06:30", set: true, want: 6*time.Hour + 30*time.Minute},
		{raw: "00:00", set: true, want: 0}, // midnight is a real answer
		{raw: "off", set: true, want: -1},  // and disabling is a different one
		{raw: "OFF", set: true, want: -1},
		{raw: "", set: true, want: -1},
		{raw: "5", set: true, wantErr: true},
		{raw: "25:00", set: true, wantErr: true},
		{raw: "5pm", set: true, wantErr: true},
	}

	for _, tc := range cases {
		if tc.set {
			t.Setenv("AMBAR_NIGHTLY_SCAN", tc.raw)
		} else {
			t.Setenv("AMBAR_NIGHTLY_SCAN", "05:00")
			// Unset it again: t.Setenv restores on cleanup, and LookupEnv is what envClock reads.
			t.Cleanup(func() {})
		}

		got, err := envClock("AMBAR_NIGHTLY_SCAN", 5*time.Hour)
		if tc.wantErr {
			if err == nil {
				t.Errorf("envClock(%q) = %v, want an error", tc.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("envClock(%q): %v", tc.raw, err)
			continue
		}
		if tc.set && got != tc.want {
			t.Errorf("envClock(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}
