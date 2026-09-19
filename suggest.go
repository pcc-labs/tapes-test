package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/pcc-labs/tapes-skills-demo/internal/codex"
	"github.com/pcc-labs/tapes-skills-demo/internal/recommend"
	"github.com/pcc-labs/tapes-skills-demo/internal/stack"
	"github.com/pcc-labs/tapes-skills-demo/internal/tapes"
)

// corpus is everything derived about the imported sessions: the detector's
// suggestions and the sessions they cite.
type corpus struct {
	suggestions []recommend.Suggestion
	sessions    map[string]tapes.Session // by tapes session id
	asked       map[string]string        // by tapes session id: the first real prompt
	byHarness   map[string]string        // Codex session id -> tapes session id
	feedback    codex.Feedback
	withTurns   int
	skills      int
}

// detect reads every derived session from the API and runs the detector,
// the console's Skills-page pass. fb is the browser comments read from the
// Codex history, which the read API holds only cut short.
func detect(ctx context.Context, client *tapes.Client, fb codex.Feedback) (*corpus, error) {
	rows, err := client.Sessions(ctx, 0)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s (is the stack up? run tapes-skills-demo first): %w", client.API, err)
	}
	var picked []tapes.Session
	seen := map[string]bool{} // one entry per harness session, whatever it was filed under
	for _, row := range rows {
		key := row.HarnessSessionID
		if key == "" {
			key = row.ID
		}
		if seen[key] || row.Rollup.TurnCount == 0 {
			continue
		}
		seen[key] = true
		picked = append(picked, row)
	}
	traces := make([][]tapes.Trace, len(picked))
	err = each(len(picked), 8, func(i int) (err error) {
		traces[i], err = client.Traces(ctx, picked[i].ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	c := &corpus{sessions: map[string]tapes.Session{}, asked: map[string]string{}, byHarness: map[string]string{}, feedback: fb}
	var sessions []recommend.Session
	for i, row := range picked {
		c.sessions[row.ID] = row
		c.asked[row.ID] = firstAsk(traces[i])
		if row.HarnessSessionID != "" {
			c.byHarness[row.HarnessSessionID] = row.ID
		}
		tokens := recommend.SessionTokens(traceText(traces[i]))
		if len(tokens) < recommend.MinTokens {
			continue
		}
		title := row.DisplayTitle
		if title == "" {
			title = row.ID
		}
		sessions = append(sessions, recommend.Session{
			ID:      row.ID,
			Title:   title,
			Author:  row.AuthSubject,
			Project: filepath.Base(row.Cwd),
			Tokens:  tokens,
		})
	}
	existing, err := client.Skills(ctx)
	if err != nil {
		return nil, err
	}
	skills := make([]recommend.Skill, 0, len(existing))
	for _, sk := range existing {
		skills = append(skills, recommend.Skill{
			ID: sk.ID, Slug: sk.Slug, Name: sk.Name, Description: sk.Description,
			Tags: sk.Tags, SourceSessionIDs: sk.SourceIDs,
		})
	}
	c.withTurns, c.skills = len(sessions), len(skills)
	c.suggestions = recommend.Detect(sessions, skills, recommend.MinSessions)
	if s, ok := c.commentSuggestion(); ok {
		c.suggestions = append([]recommend.Suggestion{s}, c.suggestions...)
	}
	if !anyNew(c.suggestions) {
		// Word overlap across a month of one person's prompts is thin. Fall
		// back to the basic pass: the projects they kept coming back to.
		c.suggestions = append(c.suggestions, recommend.ByProject(sessions, skills, recommend.MinSessions)...)
	}
	return c, nil
}

// commentSuggestion is a skill from the sessions the person reviewed in the
// browser, when there are enough of them: the rules their comments keep
// restating. Sessions a skill already came from still count; comments are
// the person's own words, not a topic the library can already cover.
func (c *corpus) commentSuggestion() (recommend.Suggestion, bool) {
	counts := map[string]int{}
	for _, cm := range c.feedback.Comments {
		if id, ok := c.byHarness[cm.SessionID]; ok {
			counts[id]++
		}
	}
	if len(counts) < recommend.MinSessions {
		return recommend.Suggestion{}, false
	}
	ids := make([]string, 0, len(counts))
	total := 0
	for id, n := range counts {
		ids = append(ids, id)
		total += n
	}
	sort.Slice(ids, func(i, j int) bool {
		if counts[ids[i]] != counts[ids[j]] {
			return counts[ids[i]] > counts[ids[j]]
		}
		return ids[i] < ids[j]
	})
	return recommend.Suggestion{
		Kind:         "new",
		Title:        "Your browser feedback",
		Why:          fmt.Sprintf("%d browser comments across %d sessions.", total, len(ids)),
		TypeHint:     "workflow",
		SessionIDs:   ids,
		FromComments: true,
	}, true
}

func anyNew(ss []recommend.Suggestion) bool {
	for _, s := range ss {
		if isNew(s) {
			return true
		}
	}
	return false
}

func traceText(traces []tapes.Trace) []recommend.TraceText {
	out := make([]recommend.TraceText, 0, len(traces))
	for _, t := range traces {
		tt := recommend.TraceText{UserPrompt: t.Trace.UserPrompt, ResponsePreview: t.Trace.ResponsePreview}
		for _, sp := range t.Spans {
			switch {
			case sp.Kind == "tool":
				tt.ToolNames = append(tt.ToolNames, sp.Name)
			case sp.Kind == "llm" && sp.Model != "":
				tt.Models = append(tt.Models, sp.Model)
			}
		}
		out = append(out, tt)
	}
	return out
}

// firstAsk is the first prompt a person typed. Codex opens many turns with
// pasted scaffolding (browser comments, ambient UI state, plugin lists,
// its own review preamble) that says nothing about the work.
//
// A browser comment's own words sit past the read API's prompt cutoff, but
// the page it was left on does not, so such a session reads as "browser
// comment on /skills" when nothing better was typed.
func firstAsk(traces []tapes.Trace) string {
	onPage := ""
	for _, t := range traces {
		if strings.HasPrefix(t.Trace.UserPrompt, "The following is the Codex agent history") {
			continue
		}
		if p := oneLine(recommend.Ask(t.Trace.UserPrompt)); p != "" {
			return p
		}
		if onPage == "" && strings.HasPrefix(t.Trace.UserPrompt, "# Browser comments") {
			onPage = browserPage(t.Trace.UserPrompt)
		}
	}
	return onPage
}

// browserPage is "browser comment on <path>" from a pasted browser comment,
// empty when it names no page.
func browserPage(prompt string) string {
	_, rest, ok := strings.Cut(prompt, "Page URL: ")
	if !ok {
		return ""
	}
	raw, _, _ := strings.Cut(rest, "\n")
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	page := u.Path
	if page == "" {
		page = "/"
	}
	return "browser comment on " + page
}

// isNew is true for a suggestion that would become a skill. The other
// kind cites sessions that repeat work a skill in the library already
// covers; the console evaluates the skill against them, and here they are
// shown so the person knows the skill exists.
func isNew(s recommend.Suggestion) bool { return s.Kind == "new" }

// show prints the suggestions strip: what the sessions looked like and the
// skills they could become, numbered for pick and `skill N`.
func (c *corpus) show(w io.Writer, ui *palette, sinceDays int) {
	fmt.Fprintln(w)
	fmt.Fprintf(w, "   %s\n", ui.dim.Render(fmt.Sprintf("%d sessions with turns in the last %d day(s), %d skill(s) already in the library",
		c.withTurns, sinceDays, c.skills)))
	if len(c.suggestions) == 0 {
		fmt.Fprintf(w, "\n   %s\n", ui.strong.Render("Nothing repeated enough to be worth a skill."))
		fmt.Fprintf(w, "   %s\n", ui.dim.Render("A suggestion needs three or more sessions doing the same kind of work; --since-days 90 widens the window."))
		fmt.Fprintf(w, "\n   %s\n", ui.strong.Render("Make one from work you remember instead:"))
		fmt.Fprintf(w, "   %s\n", `tapes-skills-demo search "how I fixed auth"`)
		fmt.Fprintf(w, "   %s\n", `tapes-skills-demo skill $(tapes-skills-demo search -q "how I fixed auth")`)
		return
	}
	c.showFeedback(w, ui)
	fmt.Fprintf(w, "\n   %s\n", ui.strong.Render("Your sessions look like this:"))
	for n, s := range c.suggestions {
		fmt.Fprintln(w)
		tag := ui.newTag.Render("new    ")
		if !isNew(s) {
			tag = ui.oldTag.Render("have it")
		}
		title := s.Title
		if !isNew(s) && s.Skill != nil {
			title = s.Skill.Name
		}
		fmt.Fprintf(w, "   %s  %s  %s  %s\n", ui.strong.Render(fmt.Sprintf("%d.", n+1)), tag, ui.strong.Render(title),
			ui.dim.Render(fmt.Sprintf("%d sessions", len(s.SessionIDs))))
		if s.FromComments {
			fmt.Fprintf(w, "       %s\n", ui.dim.Render(s.Why+" A skill of the rules they keep restating."))
			for _, cm := range c.latestComments(3) {
				fmt.Fprintf(w, "       · %s  %s\n", cm.When.Local().Format("Jan 02"), truncate(oneLine(cm.Text), 80))
			}
			continue
		}
		switch {
		case isNew(s):
			fmt.Fprintf(w, "       %s\n", ui.dim.Render("about: "+strings.Join(s.Topic, ", ")))
		case s.Skill != nil:
			fmt.Fprintf(w, "       %s\n", ui.dim.Render("you already have this skill ("+s.Skill.Slug+"); these sessions repeat its work"))
		}
		for _, line := range c.evidence(s, 3) {
			fmt.Fprintf(w, "       %s\n", line)
		}
		if extra := len(s.SessionIDs) - 3; extra > 0 {
			fmt.Fprintf(w, "       %s\n", ui.dim.Render(fmt.Sprintf("… and %d more", extra)))
		}
	}
	var offers []string
	for n, s := range c.suggestions {
		if isNew(s) {
			offers = append(offers, fmt.Sprintf("%d. %s", n+1, s.Title))
		}
	}
	if len(offers) > 0 {
		fmt.Fprintf(w, "\n   %s\n", ui.strong.Render("Maybe these skills:"))
		for _, o := range offers {
			fmt.Fprintf(w, "   %s\n", o)
		}
	} else {
		fmt.Fprintf(w, "\n   %s\n", ui.strong.Render("Every piece of repeated work already has a skill."))
	}
}

// firstNew is the number of the first suggestion that would become a skill,
// 0 when there is none.
func (c *corpus) firstNew() int {
	for n, s := range c.suggestions {
		if isNew(s) {
			return n + 1
		}
	}
	return 0
}

// showFeedback is the browser comments: how much of the work they were,
// where they were left, and the latest in the person's words.
func (c *corpus) showFeedback(w io.Writer, ui *palette) {
	fb := c.feedback
	if len(fb.Comments) == 0 {
		return
	}
	fmt.Fprintf(w, "\n   %s\n", ui.strong.Render("Browser comments"))
	fmt.Fprintf(w, "   %d comments in %d of your %d sessions; %d of them were mostly comments.\n",
		len(fb.Comments), fb.WithComments, fb.Sessions, fb.Mostly)

	type count struct {
		key string
		n   int
	}
	byProject := map[string]map[string]int{}
	perProject := map[string]int{}
	for _, cm := range fb.Comments {
		project := filepath.Base(cm.Cwd)
		if byProject[project] == nil {
			byProject[project] = map[string]int{}
		}
		byProject[project][cm.Page]++
		perProject[project]++
	}
	ranked := func(m map[string]int) []count {
		out := make([]count, 0, len(m))
		for k, n := range m {
			out = append(out, count{k, n})
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].n != out[j].n {
				return out[i].n > out[j].n
			}
			return out[i].key < out[j].key
		})
		return out
	}
	fmt.Fprintln(w)
	for _, p := range ranked(perProject) {
		var pages []string
		for _, pg := range ranked(byProject[p.key])[:min(3, len(byProject[p.key]))] {
			pages = append(pages, fmt.Sprintf("%s %d", truncate(pg.key, 40), pg.n))
		}
		fmt.Fprintf(w, "   %-24s %s  %s\n", truncate(p.key, 24), ui.strong.Render(fmt.Sprintf("%3d", p.n)), ui.dim.Render(strings.Join(pages, " · ")))
	}
	fmt.Fprintf(w, "\n   %s\n", ui.dim.Render("In your words, latest first:"))
	for _, cm := range c.latestComments(8) {
		fmt.Fprintf(w, "   %s  %-18s %s\n", ui.dim.Render(cm.When.Local().Format("Jan 02")), truncate(cm.Page, 18), truncate(oneLine(cm.Text), 80))
	}
}

// latestComments is up to n browser comments, newest first.
func (c *corpus) latestComments(n int) []codex.Comment {
	out := append([]codex.Comment(nil), c.feedback.Comments...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].When.After(out[j].When) })
	return out[:min(n, len(out))]
}

// newest is up to n of ids, most recent first: what a skill is written
// from, so it reflects how the work is done now.
func (c *corpus) newest(ids []string, n int) []string {
	sorted := append([]string(nil), ids...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return c.sessions[sorted[i]].StartedAt.After(c.sessions[sorted[j]].StartedAt)
	})
	return sorted[:min(n, len(sorted))]
}

// evidence is up to n cited sessions, newest first: date, project, and how
// the session started.
func (c *corpus) evidence(s recommend.Suggestion, n int) []string {
	rows := make([]tapes.Session, 0, len(s.SessionIDs))
	for _, id := range s.SessionIDs {
		if r, ok := c.sessions[id]; ok {
			rows = append(rows, r)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].StartedAt.After(rows[j].StartedAt) })
	var out []string
	for _, r := range rows[:min(n, len(rows))] {
		asked := c.asked[r.ID]
		if asked == "" {
			asked = oneLine(recommend.Ask(r.DisplayTitle))
		}
		if asked == "" {
			asked = "(pasted context, nothing typed)"
		}
		out = append(out, fmt.Sprintf("· %s  %-20s  %s",
			r.StartedAt.Local().Format("Jan 02"),
			truncate(filepath.Base(r.Cwd), 20),
			truncate(asked, 70)))
	}
	return out
}

// pick asks which suggestions to write. Enter takes them all. Without a
// terminal on stdin (an agent, a pipe, cron) it takes them all and says so,
// so the one-command run never waits on a prompt.
func (c *corpus) pick(yes bool) ([]int, error) {
	var all []int
	for n, s := range c.suggestions {
		if isNew(s) {
			all = append(all, n)
		}
	}
	if len(all) == 0 {
		return nil, nil
	}
	if yes {
		return all, nil
	}
	if fi, err := os.Stdin.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		note("no terminal to ask on, so writing all %d; pass numbers to `tapes-skills-demo skill` to pick later", len(all))
		return all, nil
	}
	fmt.Fprintf(os.Stderr, "\n   Write which? %s ", noteText.Render("[Enter = all, numbers like 1,3, or none]"))
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return c.parsePick(strings.TrimSpace(line), all)
}

// parsePick turns "1,3" into suggestion indexes; "" is all, "none" is none.
func (c *corpus) parsePick(answer string, all []int) ([]int, error) {
	switch strings.ToLower(answer) {
	case "", "all", "a", "y", "yes":
		return all, nil
	case "none", "n", "no", "0", "q":
		return nil, nil
	}
	var out []int
	for _, f := range strings.FieldsFunc(answer, func(r rune) bool { return r == ',' || r == ' ' }) {
		n, err := strconv.Atoi(f)
		if err != nil || n < 1 || n > len(c.suggestions) {
			return nil, fmt.Errorf("%q is not a suggestion number between 1 and %d", f, len(c.suggestions))
		}
		if !isNew(c.suggestions[n-1]) {
			note("%d is covered by a skill you already have; nothing to write", n)
			continue
		}
		out = append(out, n-1)
	}
	return out, nil
}

// write generates the chosen suggestions, parallel at most, and reports how
// many landed and how many the skills cassette failed on.
func (c *corpus) write(ctx context.Context, client *tapes.Client, out string, chosen []int, parallel int) (written, failed int, err error) {
	var mu sync.Mutex
	err = each(len(chosen), parallel, func(i int) error {
		s := c.suggestions[chosen[i]]
		var dest string
		var err error
		if s.FromComments {
			dest, err = writeFeedbackSkill(ctx, c.latestComments(maxFeedbackComments), out)
		} else {
			dest, err = writeSkill(ctx, client, out, c.newest(s.SessionIDs, 3))
		}
		mu.Lock()
		defer mu.Unlock()
		var ge *generateError
		if errors.As(err, &ge) {
			note("%d. %s: generate failed: %v", chosen[i]+1, s.Title, err)
			failed++
			return nil
		}
		if err != nil {
			return err
		}
		note("%d. %s → %s", chosen[i]+1, s.Title, dest)
		written++
		return nil
	})
	return written, failed, err
}

// suggest shows the strip without writing anything.
func suggest(ctx context.Context, args []string) error {
	fs := newFlags("suggest")
	sinceDays := fs.Int("since-days", 30, "")
	root := fs.String("codex-root", codex.DefaultRoot(), "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	client := tapes.New(stack.API, stack.Ingest)
	c, err := detect(ctx, client, readFeedback(*root, *sinceDays))
	if err != nil {
		return err
	}
	c.show(os.Stdout, outUI, *sinceDays)
	if n := c.firstNew(); n > 0 {
		fmt.Printf("\n   %s\n", outUI.dim.Render(fmt.Sprintf("write one:  tapes-skills-demo skill %d", n)))
	}
	return nil
}

// skill writes skills from what the person names: suggestion numbers from
// the strip, or session ids (usually the ones `search -q` printed).
func skill(ctx context.Context, args []string) error {
	fs := newFlags("skill")
	out := fs.String("out", "tapes-skills", "")
	sinceDays := fs.Int("since-days", 30, "")
	root := fs.String("codex-root", codex.DefaultRoot(), "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	loadDotEnv()
	if len(fs.Args()) == 0 {
		return errors.New("skill needs suggestion numbers from `tapes-skills-demo suggest`, or session ids: tapes-skills-demo skill $(tapes-skills-demo search -q \"query\")")
	}
	client := tapes.New(stack.API, stack.Ingest)
	if _, err := strconv.Atoi(fs.Args()[0]); err == nil {
		c, err := detect(ctx, client, readFeedback(*root, *sinceDays))
		if err != nil {
			return err
		}
		chosen, err := c.parsePick(strings.Join(fs.Args(), ","), nil)
		if err != nil {
			return err
		}
		if len(chosen) == 0 {
			return nil
		}
		written, failed, err := c.write(ctx, client, *out, chosen, 3)
		if err != nil {
			return err
		}
		if written == 0 && failed > 0 {
			return errors.New("no skill written")
		}
		return nil
	}
	ids := fs.Args()
	note("generating one skill from %d session(s)", len(ids))
	dest, err := writeSkill(ctx, client, *out, ids)
	if err != nil {
		return fmt.Errorf("generate failed (is the stack up?): %w", err)
	}
	fmt.Println(dest)
	return nil
}

// readFeedback is the browser comments in the window. Unreadable history
// means no comments section, not a failed run.
func readFeedback(root string, sinceDays int) codex.Feedback {
	fb, err := codex.BrowserComments(root, sinceDays)
	if err != nil {
		note("could not read browser comments from %s: %v", root, err)
	}
	return fb
}
