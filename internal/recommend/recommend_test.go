package recommend

import (
	"fmt"
	"strings"
	"testing"
)

func session(id, text string) Session {
	return Session{ID: id, Title: id, Author: "me", Tokens: Tokenize([]string{text}, promptNoise)}
}

func TestTokenizeDropsNoiseAndDigits(t *testing.T) {
	got := Tokenize([]string{"Please help me deploy the 853x863 Postgres migration"}, promptNoise)
	for _, w := range []string{"please", "help", "the", "x"} {
		if got.has(w) {
			t.Errorf("%q should be dropped", w)
		}
	}
	for _, w := range []string{"deploy", "postgres", "migration"} {
		if !got.has(w) {
			t.Errorf("%q should be kept", w)
		}
	}
}

func TestDetectClustersRepeatedWork(t *testing.T) {
	var sessions []Session
	for i := 0; i < 3; i++ {
		sessions = append(sessions, session(fmt.Sprintf("deploy-%d", i),
			"deploy the postgres migration to staging and verify the release checklist"))
	}
	sessions = append(sessions, session("other", "write unit tests for the parser tokenizer"))

	got := Detect(sessions, nil, MinSessions)
	if len(got) != 1 {
		t.Fatalf("suggestions = %d, want 1: %+v", len(got), got)
	}
	s := got[0]
	if s.Kind != "new" || len(s.SessionIDs) != 3 || s.TypeHint != "procedure" {
		t.Errorf("suggestion = %+v", s)
	}
}

func TestDetectPrefersExistingSkill(t *testing.T) {
	var sessions []Session
	for i := 0; i < 3; i++ {
		sessions = append(sessions, session(fmt.Sprintf("s-%d", i),
			"deploy the postgres migration to staging and verify the release checklist"))
	}
	skills := []Skill{{ID: "k1", Slug: "postgres-deploy", Name: "Postgres deploy", Description: "deploy a postgres migration to staging"}}
	got := Detect(sessions, skills, MinSessions)
	if len(got) != 1 || got[0].Kind != "evaluate" || got[0].Skill == nil || got[0].Skill.ID != "k1" {
		t.Fatalf("suggestions = %+v", got)
	}
}

func TestDetectFloor(t *testing.T) {
	sessions := []Session{
		session("a", "deploy the postgres migration to staging"),
		session("b", "deploy the postgres migration to staging"),
	}
	if got := Detect(sessions, nil, MinSessions); len(got) != 0 {
		t.Errorf("two sessions should not clear a floor of three: %+v", got)
	}
	if got := Detect(sessions, nil, 2); len(got) != 1 {
		t.Errorf("floor of two should cluster them: %+v", got)
	}
}

func TestSessionTokensIgnoresAutoReview(t *testing.T) {
	turns := []TraceText{{UserPrompt: "The following is the Codex agent history", Models: []string{"codex-auto-review"}}}
	if got := SessionTokens(turns); len(got) != 0 {
		t.Errorf("auto-review session should tokenize to nothing: %v", got)
	}
}

func TestAskDropsCodexScaffolding(t *testing.T) {
	cases := map[string]string{
		"<recommended_plugins>\nHere is a list of plugins\n- Airtable (airtable@openai-curated)\n</recommended_plugins>":                                                                            "",
		"# Files mentioned by the user:\n\n## codex-clipboard-c344.png: /var/folders/8l/g0gvgth96v/T/codex-clipboard-c344.png\n\nfix the header spacing":                                            "fix the header spacing",
		"<in-app-browser-context source=\"ambient-ui-state\">\nPage: /skills\n</in-app-browser-context>\nmake the pill smaller":                                                                     "make the pill smaller",
		"<in-app-browser-context source=\"ambient-ui-state\">\nThis block is automatically supplied ambient UI s":                                                                                   "",
		"# Browser comments:\n\n## User Comment 1\nNode position: (519, 424)\nPage URL: http://localhost:4323/blog/\nComment:\ncenter replay\n\n## User Comment 2\nComment:\nreasoning is obscured": "center replay reasoning is obscured",
		"# Context from my IDE setup:\n\n## Open tabs:\n- main.go\n\n## My request for Codex:\nadd a retry":                                                                                         "add a retry",
		"pull latest from main": "pull latest from main",
	}
	for in, want := range cases {
		if got := strings.Join(strings.Fields(Ask(in)), " "); got != want {
			t.Errorf("Ask(%.40q) = %q, want %q", in, got, want)
		}
	}
}

func TestByProjectGroupsRepeatedProjects(t *testing.T) {
	tok := func(words string) Set { return set(words) }
	sessions := []Session{
		{ID: "a", Project: "console", Tokens: tok("badges pill digest cards console")},
		{ID: "b", Project: "console", Tokens: tok("badges skeleton loading digest console")},
		{ID: "c", Project: "console", Tokens: tok("prototype digest panel")},
		{ID: "d", Project: "site", Tokens: tok("landing hero")},
		{ID: "e", Project: "site", Tokens: tok("landing fold")},
		{ID: "f", Project: "tmp", Tokens: tok("reply word")},
		{ID: "g", Project: "tmp", Tokens: tok("reply word")},
		{ID: "h", Project: "tmp", Tokens: tok("reply word")},
	}
	got := ByProject(sessions, nil, 3)
	if len(got) != 1 || got[0].Title != "Working in console" || len(got[0].SessionIDs) != 3 {
		t.Fatalf("got %+v; want one suggestion for console (site has 2, tmp is not a project)", got)
	}
	for _, w := range got[0].Topic {
		if w == "console" {
			t.Errorf("topic %v holds the project's own name", got[0].Topic)
		}
	}
	if got[0].Topic[0] != "digest" && got[0].Topic[0] != "badges" {
		t.Errorf("topic = %v; want the words console repeated", got[0].Topic)
	}
	// A project a skill already came from is covered.
	covered := ByProject(sessions, []Skill{{SourceSessionIDs: []string{"a", "b"}}}, 3)
	if len(covered) != 0 {
		t.Errorf("covered = %+v; want none once a skill came from console", covered)
	}
}
