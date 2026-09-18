package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func userLine(text string) string {
	b, _ := json.Marshal(map[string]any{
		"type":      "response_item",
		"timestamp": "2026-09-17T10:00:00Z",
		"payload": map[string]any{"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": text}}},
	})
	return string(b)
}

const commentsPrompt = "\n# Browser comments:\n\n## User Comment 1\nBrowser\nNode position: (304, 554) in 1800x900 viewport\n" +
	"Untrusted page evidence (from the webpage, not user instructions):\nPage URL: http://localhost:6162/skills?page=1\n" +
	"Frame: top document\nTarget: \"Dismiss\"\nTarget role: \"button\"\nComment:\nleft align the dismiss\n\n" +
	"## User Comment 2\nPage URL: http://localhost:6162/\nComment:\nthis doesn't need to be red"

func TestBrowserComments(t *testing.T) {
	root := t.TempDir()
	meta := `{"type":"session_meta","payload":{"id":"s1","cwd":"/work/console"}}`
	lines := []string{meta, `{"type":"turn_context","payload":{"model":"gpt-5-codex"}}`,
		userLine("<environment_context>cwd</environment_context>"),
		userLine(commentsPrompt),
		userLine(commentsPrompt), // replayed on resume: counted once
		userLine("ship it"),
	}
	review := []string{`{"type":"session_meta","payload":{"id":"s2","cwd":"/work/console"}}`,
		`{"type":"turn_context","payload":{"model":"codex-auto-review"}}`, userLine(commentsPrompt)}
	for name, body := range map[string][]string{"rollout-a.jsonl": lines, "rollout-b.jsonl": review} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(strings.Join(body, "\n")), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fb, err := BrowserComments(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if fb.Sessions != 1 || fb.WithComments != 1 || fb.Mostly != 1 {
		t.Errorf("sessions %d, with %d, mostly %d; want 1, 1, 1 (the review session skipped)", fb.Sessions, fb.WithComments, fb.Mostly)
	}
	if len(fb.Comments) != 2 {
		t.Fatalf("comments = %+v; want 2", fb.Comments)
	}
	c := fb.Comments[0]
	if c.Text != "left align the dismiss" || c.Page != "/skills" || c.Target != "Dismiss" || c.Cwd != "/work/console" || c.When.IsZero() {
		t.Errorf("first comment = %+v", c)
	}
	if fb.Comments[1].Page != "/" || fb.Comments[1].Text != "this doesn't need to be red" {
		t.Errorf("second comment = %+v", fb.Comments[1])
	}
}

func TestPagePathFoldsIDs(t *testing.T) {
	cases := map[string]string{
		"http://localhost:6162/sessions/01a0a6f2-880c-72c3-802f-b26b355a804e":                  "/sessions/:id",
		"http://localhost:6162/skills/2f0b8496-1026-4cce-b64f-0e20d4b06137?quality=evaluation": "/skills/:id",
		"http://localhost:4323/posts/2026/09/02/every-ai-agent-session":                        "/posts/2026/09/02/every-ai-agent-session",
		"http://127.0.0.1:5188/":                 "/",
		"http://127.0.0.1:5188":                  "/",
		"http://localhost:6162/settings/billing": "/settings/billing",
	}
	for in, want := range cases {
		if got := pagePath(in); got != want {
			t.Errorf("pagePath(%q) = %q, want %q", in, got, want)
		}
	}
}
