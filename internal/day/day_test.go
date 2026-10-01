package day

import (
	"testing"
	"time"
)

func TestOf(t *testing.T) {
	tests := []struct {
		utc  string
		want string
	}{
		// Winter (CET, UTC+1): 23:30 UTC is already 00:30 next day.
		{"2026-01-15T22:59:00Z", "2026-01-15"},
		{"2026-01-15T23:00:00Z", "2026-01-16"},
		{"2026-01-15T23:30:00Z", "2026-01-16"},
		// Summer (CEST, UTC+2).
		{"2026-07-15T21:59:00Z", "2026-07-15"},
		{"2026-07-15T22:00:00Z", "2026-07-16"},
		{"2026-07-15T22:30:00Z", "2026-07-16"},
	}
	for _, tt := range tests {
		ts, err := time.Parse(time.RFC3339, tt.utc)
		if err != nil {
			t.Fatal(err)
		}
		if got := Of(ts); got != tt.want {
			t.Errorf("Of(%s) = %s, want %s", tt.utc, got, tt.want)
		}
	}
}

func TestPretty(t *testing.T) {
	if got := Pretty("2026-10-01"); got != "Thu 1 Oct 2026" {
		t.Errorf("Pretty = %q", got)
	}
	if got := Pretty("bogus"); got != "bogus" {
		t.Errorf("Pretty(bogus) = %q", got)
	}
}
