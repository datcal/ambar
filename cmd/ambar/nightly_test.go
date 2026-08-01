package main

import (
	"testing"
	"time"
)

// untilNext decides when the nightly scan runs, so it is worth being exact about.
func TestUntilNext(t *testing.T) {
	loc := time.FixedZone("TEST", 2*60*60)
	at := 5 * time.Hour // 05:00

	cases := []struct {
		now  time.Time
		want time.Duration
		why  string
	}{
		{time.Date(2026, 7, 31, 23, 0, 0, 0, loc), 6 * time.Hour, "late evening waits for the morning"},
		{time.Date(2026, 7, 31, 4, 30, 0, 0, loc), 30 * time.Minute, "just before the hour"},
		{time.Date(2026, 7, 31, 5, 30, 0, 0, loc), 23*time.Hour + 30*time.Minute, "just after: tomorrow, not in a moment"},
		// Exactly on the hour must go to tomorrow, or a process started at 05:00:00 would
		// enqueue immediately and then again a second later.
		{time.Date(2026, 7, 31, 5, 0, 0, 0, loc), 24 * time.Hour, "on the hour is tomorrow"},
	}

	for _, tc := range cases {
		if got := untilNext(tc.now, at); got != tc.want {
			t.Errorf("untilNext(%s) = %v, want %v (%s)", tc.now.Format(time.RFC3339), got, tc.want, tc.why)
		}
	}
}

func TestClockOf(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{5 * time.Hour, "05:00"},
		{6*time.Hour + 30*time.Minute, "06:30"},
		{0, "00:00"},
	} {
		if got := clockOf(tc.in); got != tc.want {
			t.Errorf("clockOf(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
