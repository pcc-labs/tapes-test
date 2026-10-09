package deckui

import (
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/textinput"
	"github.com/charmbracelet/x/ansi"

	"github.com/papercomputeco/tapes/pkg/deck"
)

// sessionListFixture mirrors the shape that exposed the alignment bug: a mix
// of short labels and labels far longer than any sane column cap.
func sessionListFixture() []deck.SessionSummary {
	labels := []string{
		"Task: Review the current working tree after refactoring the deck",
		"Split paper-console and paper-chat into separate projects",
		"Add paper pull up skill",
		"Checkout and pull main branch",
		"Block /demos and /cline in robots.txt",
		"Capture Claude fixture recordings for tapes so the deck has data",
		"Find Jason's API sorting sessions",
	}
	start := time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)

	sessions := make([]deck.SessionSummary, 0, len(labels))
	for i, label := range labels {
		sessions = append(sessions, deck.SessionSummary{
			ID:           strings.Repeat("a", 8),
			Label:        label,
			Model:        "claude-opus-5",
			Project:      "paper-forest",
			Status:       "completed",
			StartTime:    start.Add(time.Duration(i) * time.Hour),
			Duration:     time.Duration(i+1) * time.Minute,
			InputTokens:  int64(1000 * (i + 1)),
			OutputTokens: int64(500 * (i + 1)),
			TotalCost:    float64(i+1) * 1.25,
			MessageCount: i + 1,
		})
	}
	return sessions
}

func newSessionListModel(width int) deckModel {
	input := textinput.New()
	return deckModel{
		width:       width,
		height:      40,
		overview:    &deck.Overview{Sessions: sessionListFixture()},
		searchInput: input,
	}
}

// dataRows returns the rendered session rows, stripped of ANSI, skipping the
// title/rule/header chrome and the trailing pagination notice.
func dataRows(t *testing.T, rendered string) []string {
	t.Helper()

	rows := []string{}
	seenHeader := false
	for _, line := range strings.Split(rendered, "\n") {
		plain := ansi.Strip(line)
		if !seenHeader {
			if strings.Contains(plain, "label") && strings.Contains(plain, "tokens") {
				seenHeader = true
			}
			continue
		}
		if strings.TrimSpace(plain) == "" || strings.Contains(plain, "showing ") {
			continue
		}
		rows = append(rows, plain)
	}
	if len(rows) == 0 {
		t.Fatalf("no data rows found in:\n%s", rendered)
	}
	return rows
}

// TestSessionListColumnsAlign is the regression test for the wide-terminal bug:
// the label column was capped but never truncated to that cap, so any label
// longer than the cap shoved every following column to the right on that row.
// Every column is padded to a fixed width, so aligned rows all share one width.
func TestSessionListColumnsAlign(t *testing.T) {
	for _, width := range []int{100, 120, 160, 200, 260, 400} {
		rows := dataRows(t, newSessionListModel(width).viewSessionList(30))

		want := ansi.StringWidth(rows[0])
		for i, row := range rows {
			if got := ansi.StringWidth(row); got != want {
				t.Errorf("width=%d: row %d is %d cells wide, want %d (columns misaligned)\nrow 0: %q\nrow %d: %q",
					width, i, got, want, rows[0], i, row)
			}
		}
	}
}

// TestSessionListFitsTerminalWidth guards the other half: when the fixed
// columns leave room, the table must not spill past the terminal edge.
func TestSessionListFitsTerminalWidth(t *testing.T) {
	for _, width := range []int{160, 200, 260, 400} {
		for i, row := range dataRows(t, newSessionListModel(width).viewSessionList(30)) {
			if got := ansi.StringWidth(row); got > width {
				t.Errorf("width=%d: row %d is %d cells wide, overflows terminal", width, i, got)
			}
		}
	}
}

// TestSessionListHeaderAlignsWithRows checks the headings sit over their own
// columns: the header was indented by 2 while rows carry a 4-cell prefix
// (cursor marker + row number + space), skewing every heading two cells left.
func TestSessionListHeaderAlignsWithRows(t *testing.T) {
	for _, width := range []int{120, 200, 400} {
		rendered := newSessionListModel(width).viewSessionList(30)

		var header string
		for _, line := range strings.Split(rendered, "\n") {
			plain := ansi.Strip(line)
			if strings.Contains(plain, "label") && strings.Contains(plain, "tokens") {
				header = plain
				break
			}
		}
		if header == "" {
			t.Fatalf("width=%d: no header row found", width)
		}

		// fitCell keeps an over-long heading from skewing the header, but a
		// column narrower than its heading would silently clip it ("in /...").
		// Each column must be wide enough to spell its heading out.
		for _, heading := range []string{"in / out", "cost", "tokens", "status", "project"} {
			if !strings.Contains(header, heading) {
				t.Errorf("width=%d: heading %q is clipped in header:\n%s", width, heading, header)
			}
		}

		row := dataRows(t, rendered)[0]
		for _, heading := range []string{"model", "dur", "tokens", "turns", "status", "date", "project"} {
			at := strings.Index(header, heading)
			if at < 0 {
				t.Fatalf("width=%d: heading %q missing from header %q", width, heading, header)
			}
			// Display column, not byte offset: rows carry multibyte glyphs.
			col := ansi.StringWidth(header[:at])

			// The cell under a heading must begin exactly at that column, so
			// the column itself is non-blank and the one before it is padding.
			if got := ansi.Cut(row, col-1, col); col > 0 && got != " " {
				t.Errorf("width=%d: heading %q at col %d starts mid-cell (col-1 = %q) in row:\n%s\n%s",
					width, heading, col, got, header, row)
			}
			if got := ansi.Cut(row, col, col+1); got == " " {
				t.Errorf("width=%d: heading %q at col %d sits over padding, not a cell, in row:\n%s\n%s",
					width, heading, col, header, row)
			}
		}
	}
}

// TestSessionListUsesAvailableLabelWidth pins the intent that a wider terminal
// shows more of each label, not less. The old cap froze it at 36 columns.
func TestSessionListUsesAvailableLabelWidth(t *testing.T) {
	narrow := dataRows(t, newSessionListModel(120).viewSessionList(30))
	wide := dataRows(t, newSessionListModel(220).viewSessionList(30))

	// The first fixture label is the longest, so it is truncated at both
	// widths; the wide render should reveal strictly more of it.
	narrowLabel := labelCell(narrow[0])
	wideLabel := labelCell(wide[0])
	if len(wideLabel) <= len(narrowLabel) {
		t.Errorf("wide terminal did not widen the label column: narrow=%q wide=%q", narrowLabel, wideLabel)
	}
}

// labelCell extracts the label text from a rendered row: everything between the
// row-number prefix and the model column.
func labelCell(row string) string {
	_, rest, found := strings.Cut(strings.TrimSpace(row), " ")
	if !found {
		return ""
	}
	label, _, _ := strings.Cut(rest, "claude-opus-5")
	return strings.TrimSpace(label)
}
