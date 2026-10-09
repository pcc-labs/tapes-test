package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/pcc-labs/tapes-test/internal/claude"
	"github.com/pcc-labs/tapes-test/internal/codex"
	"github.com/pcc-labs/tapes-test/internal/history"
)

// A run reads the agent histories on this machine: Codex, Claude Code, or
// both. Neither is required, so the tool works for someone who has only
// ever used one; only finding neither is an error.

// source is one agent's history on this machine.
type source struct {
	name string // "Codex", "Claude Code"
	root string
	// files lists the session files in the window, for `check`.
	files func(root string, sinceDays int) ([]string, error)
	// load reads them, and returns its own one-line report.
	load func(root string, sinceDays int) ([]history.Session, string, error)
}

// roots is where to look, and which of the two to look at.
type roots struct {
	codex   string
	claude  string
	harness string // "", "codex" or "claude"
}

// addHistoryFlags registers the flags that say where the history is and
// which of it to read.
func addHistoryFlags(fs *flag.FlagSet) *roots {
	r := &roots{}
	fs.StringVar(&r.codex, "codex-root", codex.DefaultRoot(), "")
	fs.StringVar(&r.claude, "claude-root", claude.DefaultRoot(), "")
	fs.StringVar(&r.harness, "harness", "", "")
	return r
}

// all is both sources in report order, narrowed by --harness, whether or
// not the history is actually there.
func (r *roots) all() ([]source, error) {
	codexSource := source{
		name:  "Codex",
		root:  r.codex,
		files: codex.Rollouts,
		load: func(root string, sinceDays int) ([]history.Session, string, error) {
			s, summary, err := codex.Load(root, sinceDays)
			return s, summary.Render(), err
		},
	}
	claudeSource := source{
		name:  "Claude Code",
		root:  r.claude,
		files: claude.Transcripts,
		load: func(root string, sinceDays int) ([]history.Session, string, error) {
			s, summary, err := claude.Load(root, sinceDays)
			return s, summary.Render(), err
		},
	}
	switch strings.ToLower(strings.TrimSpace(r.harness)) {
	case "", "both", "all":
		return []source{codexSource, claudeSource}, nil
	case "codex":
		return []source{codexSource}, nil
	case "claude", "claude-code", "claudecode":
		return []source{claudeSource}, nil
	default:
		return nil, fmt.Errorf("unknown --harness %q: use codex, claude, or leave it off for both", r.harness)
	}
}

// present is the sources whose history is on this machine. Finding none is
// the error, and it names every place that was looked at.
func (r *roots) present() ([]source, error) {
	all, err := r.all()
	if err != nil {
		return nil, err
	}
	var found []source
	for _, s := range all {
		if _, err := os.Stat(s.root); err == nil {
			found = append(found, s)
		}
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("no agent history found (looked in %s). If yours is kept elsewhere, pass --codex-root DIR or --claude-root DIR", r.paths())
	}
	return found, nil
}

// paths is every root that was looked at, for an error message.
func (r *roots) paths() string {
	all, err := r.all()
	if err != nil {
		return r.codex
	}
	var out []string
	for _, s := range all {
		out = append(out, s.root)
	}
	return strings.Join(out, " and ")
}

// names is the sources in a sentence: "Codex and Claude Code".
func names(srcs []source) string {
	var out []string
	for _, s := range srcs {
		out = append(out, s.name)
	}
	return strings.Join(out, " and ")
}
