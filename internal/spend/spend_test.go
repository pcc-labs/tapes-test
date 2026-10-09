package spend

import (
	"math"
	"testing"
	"time"

	"github.com/papercomputeco/tapes/pkg/sessions"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestCostPricesEachTokenType(t *testing.T) {
	p := sessions.Pricing{Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5}
	c := Call{Prompt: 1_000_000, Completion: 100_000, CacheRead: 600_000, CacheWrite: 200_000}
	// fresh 200k @10 + write 200k @12.5 + read 600k @1 + out 100k @50
	want := 2 + 2.5 + 0.6 + 5
	if got := Cost(p, c); !near(got, want) {
		t.Errorf("Cost = %v, want %v", got, want)
	}
}

func TestSwitchCostWritesTheWholeInput(t *testing.T) {
	p := sessions.Pricing{Input: 2, Output: 10, CacheRead: 0.2, CacheWrite: 2.5}
	c := Call{Prompt: 1_000_000, Completion: 100_000, CacheRead: 900_000}
	want := 2.5 + 1.0
	if got := SwitchCost(p, c); !near(got, want) {
		t.Errorf("SwitchCost = %v, want %v", got, want)
	}
	// Some harnesses report prompt_tokens without the cached part.
	c = Call{Prompt: 100_000, CacheRead: 900_000, CacheWrite: 100_000}
	if got := SwitchCost(p, c); !near(got, 2.5) {
		t.Errorf("SwitchCost with cache beyond prompt = %v, want 2.5", got)
	}
}

func TestIsReadOnly(t *testing.T) {
	cases := []struct {
		name  string
		tools []Tool
		want  bool
	}{
		{"no tools", nil, false},
		{"read tools", []Tool{{Name: "Read"}, {Name: "Grep"}}, true},
		{"edit", []Tool{{Name: "Read"}, {Name: "Edit"}}, false},
		{"git status", []Tool{{Name: "Bash", Command: "git status --short"}}, true},
		{"codex exec", []Tool{{Name: "exec_command", Command: "cat README.md"}}, true},
		{"pipe", []Tool{{Name: "Bash", Command: "cat x | head"}}, false},
		{"chain", []Tool{{Name: "Bash", Command: "cd x; ls"}}, false},
		{"substitution", []Tool{{Name: "Bash", Command: "ls $(pwd)"}}, false},
		{"redirect", []Tool{{Name: "Bash", Command: "ls > out"}}, false},
		{"rm", []Tool{{Name: "Bash", Command: "rm -rf x"}}, false},
		{"prefix only", []Tool{{Name: "Bash", Command: "lsof -i :80"}}, false},
		{"empty command", []Tool{{Name: "Bash"}}, false},
		{"mcp", []Tool{{Name: "mcp__linear__list_issues"}}, false},
	}
	for _, tc := range cases {
		if got := IsReadOnly(tc.tools); got != tc.want {
			t.Errorf("%s: IsReadOnly = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestClassify(t *testing.T) {
	read := []Tool{{Name: "Read"}}
	if s, ok := Classify(Call{CallKind: "offshoot:title", Completion: 9000}, 400); !ok || s != Overhead {
		t.Errorf("offshoot = %q,%v, want overhead", s, ok)
	}
	if s, ok := Classify(Call{CallKind: "main", Completion: 400, Tools: read}, 400); !ok || s != ReadOnly {
		t.Errorf("small read-only = %q,%v, want read_only", s, ok)
	}
	if _, ok := Classify(Call{CallKind: "main", Completion: 401, Tools: read}, 400); ok {
		t.Error("401 tokens should not be small")
	}
	if _, ok := Classify(Call{CallKind: "main", Completion: 10}, 400); ok {
		t.Error("a text-only answer is not routable")
	}
}

func TestComputeChargesTheSwitchOnce(t *testing.T) {
	pricing := sessions.PricingTable{
		"frontier": {Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5},
		"cheap":    {Input: 1, Output: 5, CacheRead: 0.1, CacheWrite: 1.25},
		"other":    {Input: 1, Output: 5, CacheRead: 0.1, CacheWrite: 1.25},
	}
	o := Options{
		Pricing:  pricing,
		Frontier: map[string]bool{"frontier": true},
		Targets:  map[Signal]string{ReadOnly: "cheap", Overhead: "cheap"},
	}
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	read := []Tool{{Name: "Read"}}
	// Two routable calls in one thread, then a frontier call that is not
	// routable, then a third routable one: the first and third pay the
	// switch, the second reads the warm cache. The list is shuffled to
	// check Compute orders it. Any call in a thread, priced or not, ends
	// the warm cache, so the two non-frontier calls sit in another session.
	calls := []Call{
		{SessionID: "s", StartedAt: t0.Add(2 * time.Minute), Model: "frontier", Completion: 5000, Prompt: 100_000, CacheRead: 90_000},
		{SessionID: "s", StartedAt: t0.Add(3 * time.Minute), Model: "frontier", Completion: 100, Prompt: 100_000, CacheRead: 90_000, Tools: read},
		{SessionID: "s", StartedAt: t0, Model: "frontier", Completion: 100, Prompt: 100_000, CacheRead: 90_000, Tools: read},
		{SessionID: "s", StartedAt: t0.Add(time.Minute), Model: "frontier", Completion: 100, Prompt: 100_000, CacheRead: 90_000, Tools: read},
		{SessionID: "t", StartedAt: t0, Model: "other", Completion: 100, Prompt: 100_000, Tools: read},
		{SessionID: "t", StartedAt: t0, Model: "<synthetic>", Completion: 100},
	}
	r := Compute(calls, o)
	if r.Calls != 6 {
		t.Errorf("Calls = %d, want 6", r.Calls)
	}
	if r.Unpriced["<synthetic>"] != 1 || len(r.Unpriced) != 1 {
		t.Errorf("Unpriced = %v", r.Unpriced)
	}
	b := r.BySignal[ReadOnly]
	if b == nil || b.Calls != 3 || b.Switches != 2 {
		t.Fatalf("read_only bucket = %+v, want 3 calls, 2 switches", b)
	}
	if r.BySignal[Overhead] != nil {
		t.Error("no overhead calls expected")
	}
	if m := r.ByModel["frontier"]; m == nil || m.Calls != 3 || !near(m.Routable, b.Routable) {
		t.Errorf("ByModel = %+v", m)
	}
	// Per routable call on the frontier: fresh 10k@10 + read 90k@1 + out 100@50 = 0.1+0.09+0.005.
	spent := 0.195
	switched := spent - (100_000*1.25+100*5)/1e6    // 0.195 - 0.1255
	warm := spent - (10_000*1+90_000*0.1+100*5)/1e6 // 0.195 - 0.0195
	if want := 2*switched + warm; !near(r.Routable, want) {
		t.Errorf("Routable = %v, want %v", r.Routable, want)
	}
	// Total: four frontier calls plus the "other" call priced at its own rate.
	frontier := 3*spent + (10_000*10+90_000*1+5000*50)/1e6
	if !near(r.FrontierCost, frontier) {
		t.Errorf("FrontierCost = %v, want %v", r.FrontierCost, frontier)
	}
	if !near(r.TotalCost, frontier+(100_000*1+100*5)/1e6) {
		t.Errorf("TotalCost = %v", r.TotalCost)
	}
	if !near(r.RoutableShare(), r.Routable/frontier) {
		t.Errorf("RoutableShare = %v", r.RoutableShare())
	}
	// Session s holds every frontier call; t holds none, so it has no row.
	if s := r.BySession["s"]; s == nil || s.Calls != 3 || !near(s.Routable, r.Routable) || !near(s.FrontierCost, frontier) {
		t.Errorf("BySession[s] = %+v", s)
	}
	if r.BySession["t"] != nil {
		t.Errorf("BySession[t] = %+v, want none", r.BySession["t"])
	}
}

func TestComputeSkipsWhenTheSwitchEatsTheSavings(t *testing.T) {
	pricing := sessions.PricingTable{
		"frontier": {Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5},
		"cheap":    {Input: 9, Output: 45, CacheRead: 0.9, CacheWrite: 11.25},
	}
	o := Options{Pricing: pricing, Frontier: map[string]bool{"frontier": true}, Targets: map[Signal]string{ReadOnly: "cheap"}}
	// Mostly cached on the frontier: writing it all at the target's rate
	// costs more than the cache reads it replaces.
	c := Call{SessionID: "s", Model: "frontier", Completion: 10, Prompt: 100_000, CacheRead: 99_000, Tools: []Tool{{Name: "Read"}}}
	r := Compute([]Call{c}, o)
	if r.NotWorthSwitching != 1 || r.Routable != 0 || len(r.BySignal) != 0 {
		t.Errorf("result = %+v", r)
	}
}

func TestDefaultsNormalizeModelNames(t *testing.T) {
	// Wire names carry date suffixes and dashes; the tables use the
	// normalized form.
	c := Call{SessionID: "s", Model: "claude-fable-5-1", CallKind: "offshoot:title", Completion: 50, Prompt: 2000}
	r := Compute([]Call{c}, Options{})
	if r.ByModel["claude-fable-5.1"] == nil || r.BySignal[Overhead] == nil {
		t.Errorf("fable-5-1 should route as overhead: %+v", r)
	}
	for _, m := range Targets {
		if _, ok := DefaultPricing()[m]; !ok {
			t.Errorf("target %s has no price", m)
		}
	}
	for m := range Frontier {
		if _, ok := DefaultPricing()[m]; !ok {
			t.Errorf("frontier model %s has no price", m)
		}
	}
}
