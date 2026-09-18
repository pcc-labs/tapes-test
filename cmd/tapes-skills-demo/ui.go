package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// palette is one writer's styles. Progress goes to stderr, results to
// stdout, and each renderer checks its own writer, so `search -q | xargs`
// and an agent reading the output get plain text while a terminal gets
// color.
type palette struct {
	stepNum, strong, dim, okTag, badTag, newTag, oldTag, score lipgloss.Style
}

// newPalette styles for f; nil is a writer with no terminal, so plain text.
func newPalette(f *os.File) *palette {
	var r *lipgloss.Renderer
	if f == nil {
		r = lipgloss.NewRenderer(io.Discard)
	} else {
		r = lipgloss.NewRenderer(f)
	}
	var (
		accent = lipgloss.Color("4")
		green  = lipgloss.Color("2")
		red    = lipgloss.Color("1")
		yellow = lipgloss.Color("3")
		grey   = lipgloss.Color("8")
	)
	return &palette{
		stepNum: r.NewStyle().Bold(true).Foreground(accent),
		strong:  r.NewStyle().Bold(true),
		dim:     r.NewStyle().Foreground(grey),
		okTag:   r.NewStyle().Bold(true).Foreground(green),
		badTag:  r.NewStyle().Bold(true).Foreground(red),
		newTag:  r.NewStyle().Foreground(green),
		oldTag:  r.NewStyle().Foreground(yellow),
		score:   r.NewStyle().Foreground(accent),
	}
}

var (
	errUI = newPalette(os.Stderr)
	outUI = newPalette(os.Stdout)

	noteText = errUI.dim
)

// say prints a step or result banner on stderr. A leading "n/5 " is the
// step number and gets the accent.
func say(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	if num, rest, ok := strings.Cut(msg, " "); ok && strings.HasSuffix(num, "/5") {
		msg = errUI.stepNum.Render(num) + " " + errUI.strong.Render(rest)
	} else {
		msg = errUI.strong.Render(msg)
	}
	fmt.Fprintf(os.Stderr, "\n%s\n", msg)
}

// note prints a detail line under the current step.
func note(format string, a ...any) {
	fmt.Fprintln(os.Stderr, "   "+noteText.Render(fmt.Sprintf(format, a...)))
}

// fail prints the one-line reason a command stopped.
func fail(err error) {
	fmt.Fprintf(os.Stderr, "%s %v\n", errUI.badTag.Render("tapes-skills-demo:"), err)
}
