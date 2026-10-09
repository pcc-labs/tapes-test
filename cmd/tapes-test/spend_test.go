package main

import (
	"testing"
	"time"
)

func TestParseCallsReadsPostgresJSON(t *testing.T) {
	// json_agg prints timestamptz in ISO 8601 with a +00:00 offset and
	// tools as an array of {name, command}, command null for non-shell tools.
	out := `[{"session_id":"a","thread_id":"","started_at":"2026-02-28T23:56:18.003+00:00","model":"claude-fable-5-1","call_kind":"main","prompt":9480,"completion":164,"cache_read":9088,"cache_write":0,"tools":[{"name":"Read","command":null},{"name":"Bash","command":"git status"}]}]
`
	calls, err := parseCalls(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("got %d calls", len(calls))
	}
	c := calls[0]
	if !c.StartedAt.Equal(time.Date(2026, 2, 28, 23, 56, 18, 3_000_000, time.UTC)) {
		t.Errorf("StartedAt = %v", c.StartedAt)
	}
	if c.Prompt != 9480 || c.CacheRead != 9088 || c.Completion != 164 {
		t.Errorf("tokens = %+v", c)
	}
	if len(c.Tools) != 2 || c.Tools[0].Name != "Read" || c.Tools[0].Command != "" || c.Tools[1].Command != "git status" {
		t.Errorf("tools = %+v", c.Tools)
	}
	if _, err := parseCalls("ERROR: boom"); err == nil {
		t.Error("garbage should not parse")
	}
}

func TestMoneyAndCommas(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{0, "$0.00"}, {1234.5, "$1,234.50"}, {0.999, "$1.00"}, {-12.345, "-$12.35"}, {1234567.891, "$1,234,567.89"},
	} {
		if got := money(tc.in); got != tc.want {
			t.Errorf("money(%v) = %s, want %s", tc.in, got, tc.want)
		}
	}
	if got := commas(1000000); got != "1,000,000" {
		t.Errorf("commas = %s", got)
	}
}
