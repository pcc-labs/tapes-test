package deckui

import (
	"testing"

	"github.com/papercomputeco/tapes/pkg/deck"

	"github.com/pcc-labs/tapes-test/internal/spend"
)

// TestSummarizeSessionsTotalsRoutableForShownSessions pins that the
// ROUTABLE tile follows the deck's filters: only sessions in the slice
// count, whatever else the database holds.
func TestSummarizeSessionsTotalsRoutableForShownSessions(t *testing.T) {
	defer func() { routable = nil }()

	shown := []deck.SessionSummary{{ID: "a"}, {ID: "b"}, {ID: "c"}}

	routable = nil
	if stats := summarizeSessions(shown); stats.RoutableKnown {
		t.Error("RoutableKnown with no database, want false")
	}

	routable = map[string]*spend.SessionSpend{
		"a":      {FrontierCost: 10, Routable: 1.5, Calls: 3},
		"b":      {FrontierCost: 5, Routable: 0.5, Calls: 1},
		"hidden": {FrontierCost: 100, Routable: 40, Calls: 9},
	}
	stats := summarizeSessions(shown)
	if !stats.RoutableKnown || stats.Routable != 2 || stats.RoutableFrontier != 15 || stats.RoutableCalls != 4 {
		t.Errorf("stats = known %v, routable %v of %v frontier, %d calls; want 2 of 15, 4 calls",
			stats.RoutableKnown, stats.Routable, stats.RoutableFrontier, stats.RoutableCalls)
	}
	if got := routableShare(stats.Routable, stats.RoutableFrontier); got != "13.3% of frontier" {
		t.Errorf("routableShare = %q", got)
	}
	if got := routableShare(0, 0); got != "no frontier spend" {
		t.Errorf("routableShare(0, 0) = %q", got)
	}
}
