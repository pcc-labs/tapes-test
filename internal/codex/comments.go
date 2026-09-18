package codex

import (
	"bufio"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// Comment is one browser comment: a note the person pinned to an element of
// a page in Codex's in-app browser. Codex pastes them into the prompt under
// "# Browser comments", each with the page, the element, and the note.
type Comment struct {
	SessionID string // Codex's session id
	Cwd       string
	When      time.Time
	Page      string // URL path, "/skills"
	Target    string // the element's text, when it has any
	Text      string // what the person wrote
}

// Feedback is one run's browser comments and how much of each session they
// were.
type Feedback struct {
	Comments []Comment
	// Sessions is how many sessions were read; WithComments how many held at
	// least one comment; Mostly how many were mostly comments (at least half
	// of what the person sent).
	Sessions, WithComments, Mostly int
}

var (
	commentText = regexp.MustCompile(`(?s)\nComment:\n(.*?)(?:\n\n|\n## |\z)`)
	commentPage = regexp.MustCompile(`\nPage URL: (\S+)`)
	commentOn   = regexp.MustCompile(`\nTarget: "([^"\n]*)`)
)

// BrowserComments reads the browser comments out of every rollout under
// root modified in the window. Codex's own review sessions are skipped, and
// a comment Codex repeats in the same session (a resumed thread replays its
// history) counts once.
func BrowserComments(root string, sinceDays int) (Feedback, error) {
	var fb Feedback
	paths, err := Rollouts(root, sinceDays)
	if err != nil {
		return fb, err
	}
	perSession := map[string]*sessionFeedback{}
	var order []string
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return fb, err
		}
		sf := readFeedback(f)
		f.Close()
		if sf.id == "" || sf.model == autoReviewModel {
			continue
		}
		// A resumed session's later rollout replays the earlier one.
		prev, seen := perSession[sf.id]
		if !seen {
			order = append(order, sf.id)
		}
		if !seen || len(sf.comments) >= len(prev.comments) {
			perSession[sf.id] = sf
		}
	}
	for _, id := range order {
		sf := perSession[id]
		fb.Sessions++
		if len(sf.comments) == 0 {
			continue
		}
		fb.WithComments++
		if sf.commentTurns*2 >= sf.userTurns {
			fb.Mostly++
		}
		fb.Comments = append(fb.Comments, sf.comments...)
	}
	return fb, nil
}

type sessionFeedback struct {
	id, cwd, model          string
	userTurns, commentTurns int
	comments                []Comment
}

func readFeedback(r io.Reader) *sessionFeedback {
	sf := &sessionFeedback{}
	seen := map[string]bool{}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for scanner.Scan() {
		var rec map[string]any
		if json.Unmarshal(scanner.Bytes(), &rec) != nil {
			continue
		}
		payload := obj(rec["payload"])
		switch str(rec["type"]) {
		case "session_meta":
			sf.id = str(payload["session_id"])
			if sf.id == "" {
				sf.id = str(payload["id"])
			}
			sf.cwd = str(payload["cwd"])
			continue
		case "turn_context":
			if m := str(payload["model"]); m != "" {
				sf.model = m
			}
			continue
		}
		if str(payload["type"]) != "message" || str(payload["role"]) != "user" {
			continue
		}
		when, _ := time.Parse(time.RFC3339Nano, str(rec["timestamp"]))
		// One message is one turn, however many blocks Codex split it into
		// (the note, its screenshot, the page evidence beside it).
		typed, commented := false, false
		for _, c := range textBlocks(payload["content"]) {
			text := strings.TrimSpace(str(obj(c)["text"]))
			switch {
			case strings.HasPrefix(text, "# Browser comments"):
				commented = true
				sf.comments = append(sf.comments, parseComments(text, sf, when, seen)...)
			case !injected(text):
				typed = true
			}
		}
		if typed || commented {
			sf.userTurns++
		}
		if commented {
			sf.commentTurns++
		}
	}
	return sf
}

// injected is text Codex adds to a user message on its own: context
// blocks, AGENTS.md, the note in front of a page screenshot.
func injected(text string) bool {
	return text == "" || strings.HasPrefix(text, "<") || strings.HasPrefix(text, "# AGENTS.md") ||
		strings.HasPrefix(text, "The next image is")
}

func parseComments(text string, sf *sessionFeedback, when time.Time, seen map[string]bool) []Comment {
	var out []Comment
	for _, part := range strings.Split(text, "## User Comment")[1:] {
		m := commentText.FindStringSubmatch(part)
		if m == nil {
			continue
		}
		note := strings.TrimSpace(m[1])
		if note == "" || seen[note] {
			continue
		}
		seen[note] = true
		cm := Comment{SessionID: sf.id, Cwd: sf.cwd, When: when, Text: note}
		if p := commentPage.FindStringSubmatch(part); p != nil {
			cm.Page = pagePath(p[1])
		}
		if t := commentOn.FindStringSubmatch(part); t != nil {
			cm.Target = strings.TrimSpace(t[1])
		}
		out = append(out, cm)
	}
	return out
}

// pagePath is a URL's path, "/" for the root, the URL itself when it does
// not parse.
func pagePath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if u.Path == "" {
		return "/"
	}
	// "/sessions/01a0a6f2-880c-…" is one page, whichever session it shows.
	parts := strings.Split(u.Path, "/")
	for i, p := range parts {
		if idSegment.MatchString(p) {
			parts[i] = ":id"
		}
	}
	return strings.Join(parts, "/")
}

// idSegment is a path segment that names a record rather than a page: a
// uuid, a long hex or numeric id.
var idSegment = regexp.MustCompile(`^(?:[0-9a-f]{8}-[0-9a-f-]{8,}|[0-9a-f]{16,}|[0-9]{5,})$`)
