// Package history is the shape every harness reader hands back: one
// session of Claude-shaped transcript records, whichever agent wrote it.
//
// The tapes deriver projects Claude Code transcript records (type
// user/assistant, a message with content blocks, uuid chained to
// parentUuid). Claude Code writes them already; the codex package rewrites
// Codex rollouts into them. Both arrive here.
package history

// Record is one Claude-shaped transcript line.
type Record map[string]any

// Session is one agent session ready to upload.
type Session struct {
	// Harness is the tapes harness id: "codex" or "claude-code".
	Harness string
	ID      string
	Cwd     string
	Version string
	Model   string
	Records []Record
}
