package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAllNarrowsByHarness(t *testing.T) {
	for _, tc := range []struct {
		harness string
		want    string
	}{
		{"", "Codex and Claude Code"},
		{"both", "Codex and Claude Code"},
		{"codex", "Codex"},
		{"claude", "Claude Code"},
		{"Claude-Code", "Claude Code"},
	} {
		r := &roots{harness: tc.harness}
		all, err := r.all()
		if err != nil {
			t.Fatalf("--harness %q: %v", tc.harness, err)
		}
		if got := names(all); got != tc.want {
			t.Errorf("--harness %q = %q, want %q", tc.harness, got, tc.want)
		}
	}
	if _, err := (&roots{harness: "cursor"}).all(); err == nil {
		t.Error("an unknown --harness should be an error")
	}
}

// Either history alone is enough; only finding neither is the error.
func TestPresentTakesWhicheverExists(t *testing.T) {
	dir := t.TempDir()
	codexRoot := filepath.Join(dir, "codex")
	claudeRoot := filepath.Join(dir, "claude")
	if err := os.MkdirAll(claudeRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	r := &roots{codex: codexRoot, claude: claudeRoot}
	found, err := r.present()
	if err != nil {
		t.Fatalf("Claude Code alone should be enough: %v", err)
	}
	if got := names(found); got != "Claude Code" {
		t.Errorf("found %q, want only Claude Code", got)
	}

	if err := os.MkdirAll(codexRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	found, _ = r.present()
	if got := names(found); got != "Codex and Claude Code" {
		t.Errorf("found %q, want both", got)
	}

	none := &roots{codex: filepath.Join(dir, "nope"), claude: filepath.Join(dir, "nope2")}
	if _, err := none.present(); err == nil {
		t.Error("no history at all should be an error")
	}
}
