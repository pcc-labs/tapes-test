// Package codex reads Codex CLI rollouts and rewrites each one as the
// Claude-shaped transcript the tapes deriver understands.
//
// Codex keeps its history at ~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl
// with the content one level down under `payload`. The tapes deriver
// projects Claude Code transcript records (type user/assistant, a message
// with content blocks, uuid chained to parentUuid). This package bridges the
// two in memory; nothing in tapes changes.
//
// Carried per session: session id, cwd, Codex CLI version, the model per
// turn (from `turn_context`), user prompts, assistant text, reasoning
// summaries as thinking blocks, tool calls with parsed arguments, tool
// results, and per-call input / cached / output / reasoning token counts
// (from `token_count`).
//
// Dropped: developer and system messages, encrypted reasoning content, and
// the reasoning-effort setting, which has no field in the Claude shape.
//
// A resumed Codex thread writes a second rollout under the same session id;
// the later rollout (by path order) wins and Summary counts it as resumed.
package codex

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
)

// Record is one Claude-shaped transcript line.
type Record map[string]any

// Session is one converted rollout.
type Session struct {
	ID      string
	Cwd     string
	Version string
	Model   string
	Records []Record
}

// Summary counts one run, in the order Render prints them.
type Summary struct {
	Written       int
	Resumed       int
	SkippedNoMeta int
	SkippedEmpty  int
	SkippedReview int
	BadLines      int
}

// Render is the one-line report for a run.
func (s Summary) Render() string {
	parts := []string{fmt.Sprintf("read %d session(s)", s.Written)}
	if s.Resumed > 0 {
		parts = append(parts, fmt.Sprintf("%d resumed (later rollout kept)", s.Resumed))
	}
	if s.SkippedNoMeta > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped: no session_meta", s.SkippedNoMeta))
	}
	if s.SkippedEmpty > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped: no records", s.SkippedEmpty))
	}
	if s.SkippedReview > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped: Codex reviewing its own runs", s.SkippedReview))
	}
	if s.BadLines > 0 {
		parts = append(parts, fmt.Sprintf("%d unparseable line(s) ignored", s.BadLines))
	}
	return strings.Join(parts, ", ")
}

// DefaultRoot is where the Codex CLI keeps its rollouts.
func DefaultRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".codex/sessions"
	}
	return filepath.Join(home, ".codex", "sessions")
}

// Rollouts lists rollout files under root, oldest path first, modified
// within the last sinceDays days (0 means everything).
func Rollouts(root string, sinceDays int) ([]string, error) {
	var cutoff time.Time
	if sinceDays > 0 {
		cutoff = time.Now().Add(-time.Duration(sinceDays) * 24 * time.Hour)
	}
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasPrefix(d.Name(), "rollout-") || !strings.HasSuffix(d.Name(), ".jsonl") {
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

// autoReviewModel marks Codex's approval reviewer: sessions of their own,
// every prompt "The following is the Codex agent history…". They are the
// harness talking to itself, and imported they crowd the person's work out
// of search.
const autoReviewModel = "codex-auto-review"

// Load converts every rollout under root modified in the window. Sessions
// come back in path order; a resumed session appears once, from its last
// rollout.
func Load(root string, sinceDays int) ([]Session, Summary, error) {
	var summary Summary
	paths, err := Rollouts(root, sinceDays)
	if err != nil {
		return nil, summary, err
	}
	index := map[string]int{}
	var sessions []Session
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
		if session.Model == autoReviewModel {
			summary.SkippedReview++
			continue
		}
		if i, seen := index[session.ID]; seen {
			summary.Resumed++
			sessions[i] = session
			continue
		}
		index[session.ID] = len(sessions)
		sessions = append(sessions, session)
		summary.Written++
	}
	return sessions, summary, nil
}

// Convert reads one rollout's JSONL. ok is false when there is nothing to
// keep; summary says why.
func Convert(r io.Reader, summary *Summary) (Session, bool) {
	var b *builder
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			summary.BadLines++
			continue
		}
		if str(rec["type"]) == "session_meta" {
			payload := obj(rec["payload"])
			id := str(payload["session_id"])
			if id == "" {
				id = str(payload["id"])
			}
			if id != "" {
				b = newBuilder(id, str(payload["cwd"]), str(payload["cli_version"]))
			}
			continue
		}
		if b != nil {
			b.apply(rec)
		}
	}
	if b == nil {
		summary.SkippedNoMeta++
		return Session{}, false
	}
	if len(b.records) == 0 {
		summary.SkippedEmpty++
		return Session{}, false
	}
	return Session{ID: b.id, Cwd: b.cwd, Version: b.version, Model: b.model, Records: b.records}, true
}

// builder accumulates Claude-shaped records for one rollout. Every record
// gets a fresh uuid chained to the previous one through parentUuid, which is
// what the server's transcript projection walks. Claude Code stamps cwd and
// version on every line, so we do too.
type builder struct {
	id, cwd, version, model string
	records                 []Record
	seq                     int
	parent                  any
	lastAssistant           int
}

func newBuilder(id, cwd, version string) *builder {
	return &builder{id: id, cwd: cwd, version: version, lastAssistant: -1}
}

func (b *builder) base(timestamp, kind string) Record {
	b.seq++
	uuid := fmt.Sprintf("%s-%05d", b.id, b.seq)
	rec := Record{
		"type":        kind,
		"uuid":        uuid,
		"parentUuid":  b.parent,
		"timestamp":   timestamp,
		"sessionId":   b.id,
		"cwd":         b.cwd,
		"version":     b.version,
		"isSidechain": false,
	}
	b.parent = uuid
	return rec
}

func (b *builder) user(timestamp string, content []any) {
	rec := b.base(timestamp, "user")
	rec["message"] = map[string]any{"role": "user", "content": content}
	b.records = append(b.records, rec)
}

func (b *builder) assistant(timestamp string, content []any, stopReason string) {
	rec := b.base(timestamp, "assistant")
	model := b.model
	if model == "" {
		model = "codex"
	}
	rec["message"] = map[string]any{
		"id":          fmt.Sprintf("msg-%d", b.seq),
		"role":        "assistant",
		"model":       model,
		"content":     content,
		"stop_reason": stopReason,
		"usage":       map[string]any{},
	}
	b.records = append(b.records, rec)
	b.lastAssistant = len(b.records) - 1
}

// usage attaches a token_count to the assistant record it follows. Codex
// reports usage after the response items it covers, one report per API
// call, so the most recent assistant record is the one it belongs to. A
// report with no assistant record yet (a prompt-only rollout) is dropped.
func (b *builder) usage(last map[string]any) {
	if b.lastAssistant < 0 {
		return
	}
	msg := b.records[b.lastAssistant]["message"].(map[string]any)
	msg["usage"] = map[string]any{
		"input_tokens":            num(last["input_tokens"]),
		"cache_read_input_tokens": num(last["cached_input_tokens"]),
		"output_tokens":           num(last["output_tokens"]),
		"reasoning_output_tokens": num(last["reasoning_output_tokens"]),
	}
}

func (b *builder) apply(rec map[string]any) {
	payload := obj(rec["payload"])
	timestamp := str(rec["timestamp"])
	switch str(rec["type"]) {
	case "turn_context":
		if m := str(payload["model"]); m != "" {
			b.model = m
		}
	case "event_msg":
		if str(payload["type"]) == "token_count" {
			if last := obj(obj(payload["info"])["last_token_usage"]); len(last) > 0 {
				b.usage(last)
			}
		}
	case "response_item":
		b.applyItem(payload, timestamp)
	}
}

func (b *builder) applyItem(payload map[string]any, timestamp string) {
	switch str(payload["type"]) {
	case "message":
		blocks := textBlocks(payload["content"])
		if len(blocks) == 0 {
			return
		}
		switch str(payload["role"]) {
		case "user":
			b.user(timestamp, blocks)
		case "assistant":
			b.assistant(timestamp, blocks, "end_turn")
		}
		// developer / system messages: the projection ignores Claude's
		// system records too, so there is nothing to carry.
	case "reasoning":
		var parts []string
		if summary, ok := payload["summary"].([]any); ok {
			for _, p := range summary {
				if text := str(obj(p)["text"]); text != "" {
					parts = append(parts, text)
				}
			}
		}
		if s := strings.TrimSpace(strings.Join(parts, " ")); s != "" {
			b.assistant(timestamp, []any{map[string]any{"type": "thinking", "thinking": s}}, "tool_use")
		}
	case "function_call", "custom_tool_call":
		b.assistant(timestamp, []any{map[string]any{
			"type":  "tool_use",
			"id":    payload["call_id"],
			"name":  payload["name"],
			"input": toolArguments(payload),
		}}, "tool_use")
	case "function_call_output", "custom_tool_call_output":
		output, ok := payload["output"].(string)
		if !ok {
			raw, _ := json.Marshal(payload["output"])
			output = string(raw)
		}
		b.user(timestamp, []any{map[string]any{
			"type":        "tool_result",
			"tool_use_id": payload["call_id"],
			"content":     output,
		}})
	}
}

// textBlocks turns Codex message content parts into Claude text blocks;
// non-text parts drop.
func textBlocks(content any) []any {
	switch c := content.(type) {
	case string:
		if c == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": c}}
	case []any:
		var out []any
		for _, p := range c {
			part := obj(p)
			switch str(part["type"]) {
			case "input_text", "output_text", "text":
				out = append(out, map[string]any{"type": "text", "text": str(part["text"])})
			}
		}
		return out
	}
	return nil
}

func toolArguments(payload map[string]any) map[string]any {
	raw, ok := payload["arguments"]
	if !ok || raw == nil {
		raw = payload["input"]
	}
	switch v := raw.(type) {
	case map[string]any:
		return v
	case string:
		var parsed any
		if err := json.Unmarshal([]byte(v), &parsed); err != nil {
			return map[string]any{"raw": v}
		}
		if m, ok := parsed.(map[string]any); ok {
			return m
		}
		return map[string]any{"raw": parsed}
	}
	return map[string]any{}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func num(v any) float64 {
	f, _ := v.(float64)
	return f
}
