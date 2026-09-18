package recommend

import (
	"fmt"
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
