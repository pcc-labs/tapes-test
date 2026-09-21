// Package claude reads Claude Code transcripts.
//
// Claude Code keeps its history at ~/.claude/projects/<slugged-cwd>/<session
// id>.jsonl, one file per session, and the lines are already the shape the
// tapes deriver projects. There is nothing to rewrite: this package finds
// the files, drops the lines that are not transcript, and reads the session
// id, cwd and version off the records themselves.
//
// Kept: every record that carries a uuid. Those are the transcript proper
// (user, assistant, attachment, system), and they are chained to each other
// through parentUuid, so keeping all of them keeps the chain whole.
//
// Dropped: the bookkeeping lines Claude Code writes beside the transcript
// (ai-title, last-prompt, mode, permission-mode, queue-operation,
// file-history-snapshot, cost-state and the rest). None of them carries a
// uuid, so nothing in the transcript points at one, and dropping them
// breaks nothing.
package claude

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pcc-labs/tapes-test/internal/history"
)

// HarnessID is what these sessions are filed under in tapes.
const HarnessID = "claude-code"

// Summary counts one run, in the order Render prints them.
type Summary struct {
	Read         int
	SkippedEmpty int
	BadLines     int
}

// Render is the one-line report for a run.
func (s Summary) Render() string {
	parts := []string{fmt.Sprintf("read %d session(s)", s.Read)}
	if s.SkippedEmpty > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped: no records", s.SkippedEmpty))
	}
	if s.BadLines > 0 {
		parts = append(parts, fmt.Sprintf("%d unparseable line(s) ignored", s.BadLines))
	}
	return strings.Join(parts, ", ")
}

// DefaultRoot is where Claude Code keeps its transcripts: under
// CLAUDE_CONFIG_DIR when that is set, otherwise ~/.claude.
func DefaultRoot() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "projects")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".claude/projects"
	}
	return filepath.Join(home, ".claude", "projects")
}

// Transcripts lists session files under root, oldest path first, modified
// within the last sinceDays days (0 means everything).
func Transcripts(root string, sinceDays int) ([]string, error) {
	var cutoff time.Time
	if sinceDays > 0 {
		cutoff = time.Now().Add(-time.Duration(sinceDays) * 24 * time.Hour)
	}
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// journal.jsonl sits beside the sessions and is not one.
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") || d.Name() == "journal.jsonl" {
			return nil
		}
		if !cutoff.IsZero() {
			info, err := d.Info()
			if err != nil || info.ModTime().Before(cutoff) {
				return nil
			}
		}
		out = append(out, path)
		return nil
	})
	sort.Strings(out)
	return out, err
}

// Load reads every transcript under root modified in the window, in path
// order.
func Load(root string, sinceDays int) ([]history.Session, Summary, error) {
	var summary Summary
	paths, err := Transcripts(root, sinceDays)
	if err != nil {
		return nil, summary, err
	}
	var sessions []history.Session
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return nil, summary, err
		}
		session, ok := Convert(f, &summary)
		f.Close()
		if !ok {
			continue
		}
		// The file is named for the session; the records are the fallback,
		// and the name wins only when they carry nothing.
		if session.ID == "" {
			session.ID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
		}
		sessions = append(sessions, session)
		summary.Read++
	}
	return sessions, summary, nil
}

// Convert reads one transcript's JSONL. ok is false when there is nothing
// to keep; summary says why.
func Convert(r io.Reader, summary *Summary) (history.Session, bool) {
	session := history.Session{Harness: HarnessID}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec history.Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			summary.BadLines++
			continue
		}
		if str(rec["uuid"]) == "" {
			continue // bookkeeping, not transcript
		}
		if session.ID == "" {
			session.ID = str(rec["sessionId"])
		}
		if session.Cwd == "" {
			session.Cwd = str(rec["cwd"])
		}
		if session.Version == "" {
			session.Version = str(rec["version"])
		}
		if str(rec["type"]) == "assistant" {
			if m := str(obj(rec["message"])["model"]); m != "" {
				session.Model = m
			}
		}
		session.Records = append(session.Records, rec)
	}
	if len(session.Records) == 0 {
		summary.SkippedEmpty++
		return history.Session{}, false
	}
	return session, true
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}
