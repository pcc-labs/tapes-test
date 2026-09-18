package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/pcc-labs/tapes-skill-report/internal/recommend"
	"github.com/pcc-labs/tapes-skill-report/internal/tapes"
)

func trace(prompt string) tapes.Trace {
	var t tapes.Trace
	t.Trace.UserPrompt = prompt
	return t
}

func TestFirstAskSkipsScaffolding(t *testing.T) {
	got := firstAsk([]tapes.Trace{
		trace("<in-app-browser-context source=\"ambient-ui-state\">"),
		trace("The following is the Codex agent history whose request action you are assessing."),
		trace("# Files mentioned by the user:\n## notes.md"),
		trace("these badges are too much"),
	})
	if got != "these badges are too much" {
		t.Errorf("firstAsk = %q", got)
	}
}

func TestFirstAskNamesTheCommentedPage(t *testing.T) {
	got := firstAsk([]tapes.Trace{trace("# Browser comments:\n## User Comment 1\nBrowser\nNode position: (1, 2)\nPage URL: http://localhost:6162/skills?page=1\nFrame: top document")})
	if got != "browser comment on /skills" {
		t.Errorf("firstAsk = %q", got)
	}
}

func fixture() *corpus {
	return &corpus{
		suggestions: []recommend.Suggestion{
			{Kind: "evaluate", Title: "Evaluate Ship It", SessionIDs: []string{"a", "b"}, Skill: &recommend.Skill{Slug: "ship-it", Name: "Ship It"}},
			{Kind: "new", Title: "Release Changelog Tag", SessionIDs: []string{"c", "d", "e"}, Topic: []string{"release", "changelog", "tag"}},
		},
		sessions:  map[string]tapes.Session{},
		asked:     map[string]string{},
		withTurns: 5,
		skills:    1,
	}
}

func TestShowOffersOnlyNewSkills(t *testing.T) {
	var buf bytes.Buffer
	c := fixture()
	c.show(&buf, newPalette(nil), 30)
	out := buf.String()
	for _, want := range []string{"Your sessions look like this:", "have it  Ship It", "new      Release Changelog Tag", "Maybe these skills:\n   2. Release Changelog Tag"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if n := c.firstNew(); n != 2 {
		t.Errorf("firstNew = %d, want 2", n)
	}
}

func TestParsePick(t *testing.T) {
	c := fixture()
	all := []int{1}
	cases := map[string][]int{"": {1}, "all": {1}, "none": nil, "2": {1}, "1,2": {1}, "1": nil}
	for answer, want := range cases {
		got, err := c.parsePick(answer, all)
		if err != nil || len(got) != len(want) || (len(want) > 0 && got[0] != want[0]) {
			t.Errorf("parsePick(%q) = %v, %v; want %v", answer, got, err, want)
		}
	}
	if _, err := c.parsePick("9", all); err == nil {
		t.Error("parsePick(\"9\") should fail: there are two suggestions")
	}
}
