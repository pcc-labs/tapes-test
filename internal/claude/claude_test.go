package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// transcript is the shape Claude Code writes: the transcript records carry
// a uuid and chain through parentUuid, and the bookkeeping lines beside
// them do not.
const transcript = `{"type":"ai-title","aiTitle":"Fix the build","sessionId":"sess-1"}
{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"sess-1","cwd":"/home/me/app","version":"2.1.278","message":{"role":"user","content":[{"type":"text","text":"fix the build"}]}}
{"type":"queue-operation","operation":"enqueue","sessionId":"sess-1"}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"sess-1","cwd":"/home/me/app","version":"2.1.278","message":{"role":"assistant","model":"claude-opus-5","content":[{"type":"tool_use","id":"c1","name":"Bash","input":{"command":"make"}}]}}
{"type":"attachment","uuid":"x1","parentUuid":"a1","sessionId":"sess-1"}
{"type":"user","uuid":"u2","parentUuid":"x1","sessionId":"sess-1","cwd":"/home/me/app","version":"2.1.278","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"ok"}]}}
{"type":"assistant","uuid":"a2","parentUuid":"u2","sessionId":"sess-1","cwd":"/home/me/app","version":"2.1.278","message":{"role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"Done."}]}}
{"type":"cost-state","totalCostUSD":0.12}
not json
`

func TestConvert(t *testing.T) {
	var summary Summary
	s, ok := Convert(strings.NewReader(transcript), &summary)
	if !ok {
		t.Fatal("expected a session")
	}
	if s.Harness != HarnessID {
		t.Errorf("harness = %q, want %q", s.Harness, HarnessID)
	}
	if s.ID != "sess-1" || s.Cwd != "/home/me/app" || s.Version != "2.1.278" || s.Model != "claude-opus-5" {
		t.Errorf("meta: %+v", s)
	}
	if summary.BadLines != 1 {
		t.Errorf("bad lines = %d, want 1", summary.BadLines)
	}

	// Every record with a uuid is kept, including the attachment the next
	// record's parentUuid points at; everything without one is dropped.
	var uuids []string
	for _, r := range s.Records {
		uuids = append(uuids, r["uuid"].(string))
	}
	if got, want := strings.Join(uuids, " "), "u1 a1 x1 u2 a2"; got != want {
		t.Fatalf("records = %q, want %q", got, want)
	}

	// The chain is whole: every parentUuid resolves to a kept record.
	kept := map[string]bool{}
	for _, u := range uuids {
		kept[u] = true
	}
	for _, r := range s.Records {
		parent, ok := r["parentUuid"].(string)
		if ok && !kept[parent] {
			t.Errorf("record %v points at dropped parent %q", r["uuid"], parent)
		}
	}
}

func TestConvertSkipsEmpty(t *testing.T) {
	var summary Summary
	// Bookkeeping only: nothing carries a uuid, so there is no transcript.
	body := `{"type":"ai-title","aiTitle":"x"}
{"type":"cost-state","totalCostUSD":0.1}`
	if _, ok := Convert(strings.NewReader(body), &summary); ok || summary.SkippedEmpty != 1 {
		t.Error("a file with no transcript records should be skipped")
	}
}

func TestLoad(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "-home-me-app")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(project, "sess-1.jsonl"): transcript,
		// journal.jsonl sits beside the sessions and is not one.
		filepath.Join(project, "journal.jsonl"): `{"type":"started","key":"v2:abc","agentId":"a1"}`,
	}
	for name, body := range files {
		if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sessions, summary, err := Load(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ID != "sess-1" || summary.Read != 1 {
		t.Fatalf("sessions = %+v, summary = %+v; want the one transcript", sessions, summary)
	}
}

// A transcript whose records carry no sessionId is still that session: the
// file is named for it.
func TestLoadFallsBackToFilename(t *testing.T) {
	root := t.TempDir()
	body := `{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`
	if err := os.WriteFile(filepath.Join(root, "sess-9.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	sessions, _, err := Load(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ID != "sess-9" {
		t.Errorf("sessions = %+v; want the id from the filename", sessions)
	}
}

func TestDefaultRootFollowsConfigDir(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/tmp/cc")
	if got, want := DefaultRoot(), filepath.Join("/tmp/cc", "projects"); got != want {
		t.Errorf("DefaultRoot() = %q, want %q", got, want)
	}
}
