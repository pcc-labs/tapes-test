// tapes-skill-report turns a month of Codex history into recommended skills.
//
//	tapes-skill-report                  import, derive, write ./tapes-skills
//	tapes-skill-report --since-days 90  a wider window
//	tapes-skill-report --ollama         the same, with local models only
//	tapes-skill-report sessions         what was imported
//	tapes-skill-report search "query"   semantic search over the imported work
//	tapes-skill-report suggest          show the clusters without generating
//	tapes-skill-report down             stop the stack and delete its data
//
// Needs Docker and an OpenAI key (OPENAI_API_KEY, or a .env in the current
// directory). Everything runs on this machine; the outbound calls are skill
// generation and span embeddings, both to OpenAI, or to a local Ollama with
// --ollama. Re-running is safe: the server dedups sessions already imported,
// and only new clusters produce new skills.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/pcc-labs/tapes-skill-report/internal/codex"
	"github.com/pcc-labs/tapes-skill-report/internal/recommend"
	"github.com/pcc-labs/tapes-skill-report/internal/stack"
	"github.com/pcc-labs/tapes-skill-report/internal/tapes"
)

const usage = `usage: tapes-skill-report [command] [flags]

commands:
  run        import Codex history, derive it, write skills (default)
  sessions   list imported sessions
  search     semantic search over imported sessions
  suggest    print the repeated-work clusters without generating skills
  skill      write one skill from the sessions you name
  down       stop the stack and delete its data

run flags:
  --since-days N   rollouts modified in the last N days (default 30; 0 = all)
  --out DIR        where <slug>/SKILL.md is written (default tapes-skills)
  --codex-root DIR Codex sessions tree (default ~/.codex/sessions)
  --ollama         use a local Ollama instead of OpenAI: nothing leaves the
                   machine, but skills are slower and rougher

sessions flags:
  --limit N        rows to print (default 50; 0 = all)

search flags:
  -k N             hits to return (default 5)
  -q               print only session ids, one per line, best first

skill flags:
  --out DIR        where <slug>/SKILL.md is written (default tapes-skills)

  tapes-skill-report skill $(tapes-skill-report search -q "how I fixed auth")
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	args := os.Args[1:]
	cmd := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "run":
		err = run(ctx, args)
	case "sessions":
		err = sessions(ctx, args)
	case "search":
		err = search(ctx, args)
	case "suggest":
		err = suggest(ctx, args)
	case "skill":
		err = skill(ctx, args)
	case "down":
		err = down(ctx)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		err = fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "tapes-skill-report: %v\n", err)
		os.Exit(1)
	}
}

func say(format string, a ...any)  { fmt.Fprintf(os.Stderr, "\n== "+format+"\n", a...) }
func note(format string, a ...any) { fmt.Fprintf(os.Stderr, "   "+format+"\n", a...) }

func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	sinceDays := fs.Int("since-days", 30, "")
	out := fs.String("out", "tapes-skills", "")
	root := fs.String("codex-root", codex.DefaultRoot(), "")
	ollama := fs.Bool("ollama", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	loadDotEnv()
	if !*ollama && os.Getenv("OPENAI_API_KEY") == "" {
		return errors.New("no OPENAI_API_KEY: export it, or put OPENAI_API_KEY=sk-... in ./.env (or pass --ollama to stay fully local)")
	}
	if _, err := os.Stat(*root); err != nil {
		return fmt.Errorf("no Codex history at %s", *root)
	}
	st, err := stack.New(*ollama)
	if err != nil {
		return err
	}

	say("1/5 reading the last %d day(s) of Codex history", *sinceDays)
	sessions, summary, err := codex.Load(*root, *sinceDays)
	if err != nil {
		return err
	}
	note("%s", summary.Render())
	if len(sessions) == 0 {
		return fmt.Errorf("nothing to import from %s; widen --since-days or check --codex-root", *root)
	}

	say("2/5 starting tapes (postgres, tapes, skills and search cassettes)")
	if err := st.Up(ctx); err != nil {
		return err
	}
	client := tapes.New(stack.API, stack.Ingest)
	if err := waitFor(ctx, client.Ping, 90*time.Second); err != nil {
		return fmt.Errorf("tapes did not come up; try: docker compose -f %s logs tapes", st.File())
	}
	if *ollama {
		// Before the import, so the search cassette's first embedding call
		// finds its model.
		skillModel := os.Getenv("TAPES_SKILL_MODEL")
		if skillModel == "" {
			skillModel = stack.DefaultSkillModel
		}
		note("using Ollama in %s; the first run downloads the models (about 3 GB)", st.Ollama())
		for _, model := range []string{stack.EmbeddingModel, skillModel} {
			note("pulling %s", model)
			if err := st.PullModel(ctx, model); err != nil {
				return err
			}
		}
	}

	say("3/5 importing %d session(s)", len(sessions))
	subject := "local:" + username()
	for i, s := range sessions {
		err := client.UploadTranscript(ctx, tapes.Transcript{
			HarnessID:        "codex",
			HarnessSessionID: s.ID,
			HarnessVersion:   s.Version,
			Cwd:              s.Cwd,
			AuthSubject:      subject,
			Records:          s.Records,
		})
		if err != nil {
			return fmt.Errorf("upload %s: %w", s.ID, err)
		}
		fmt.Fprintf(os.Stderr, "   %d/%d\r", i+1, len(sessions))
	}
	note("done            ")

	say("4/5 deriving sessions (roughly 5-60s each; a month is usually a few minutes)")
	if err := waitForQueue(ctx, st); err != nil {
		return err
	}

	say("5/5 generating skills")
	before := skillsWritten(*out)
	written, covered, failed, err := generate(ctx, client, *out)
	if err != nil {
		return err
	}
	// A month of one person's history often holds no three similar sessions.
	// Count what THIS run wrote: an earlier run's skills are still in the
	// directory.
	switch {
	case written > 0:
		say("skills are in ./%s", *out)
	case failed > 0:
		say("found %d group(s) of repeated work, but writing the skills failed (above)", failed)
		if *ollama {
			note("local models often run out the cassette's 30s budget; an OpenAI key is the reliable path")
		}
	case before > 0:
		say("no new skills: the repeated work in the last %d day(s) is already covered by ./%s", *sinceDays, *out)
	case covered > 0:
		say("nothing new: %d group(s) match a skill you already have", covered)
	default:
		say("no skills this time: nothing in the last %d day(s) repeated enough to be worth one", *sinceDays)
		note("try a wider window:  tapes-skill-report --since-days 90")
	}
	note("browse what was imported:  tapes-skill-report sessions")
	note("search it:                 tapes-skill-report search \"how did I fix auth\"")
	note("stack is up at %s; `tapes-skill-report down` removes it and its data", stack.API)
	return nil
}

// generate detects clusters and writes a SKILL.md for each new one. It
// returns how many it wrote, how many clusters an existing skill covers, and
// how many the skills cassette failed on.
func generate(ctx context.Context, client *tapes.Client, out string) (written, covered, failed int, err error) {
	suggestions, byID, err := detect(ctx, client)
	if err != nil {
		return 0, 0, 0, err
	}
	for _, s := range suggestions {
		if s.Kind != "new" {
			covered++
			continue
		}
		ids := s.SessionIDs[:min(3, len(s.SessionIDs))]
		dest, err := writeSkill(ctx, client, out, ids)
		var ge *generateError
		if errors.As(err, &ge) {
			note("generate failed for %q: %v", s.Title, err)
			failed++
			continue
		}
		if err != nil {
			return written, covered, failed, err
		}
		note("wrote %s  (from %d sessions, e.g. %s)", dest, len(s.SessionIDs), truncate(oneLine(byID[ids[0]].Title), 60))
		written++
	}
	return written, covered, failed, nil
}

// generateError is the skills cassette declining or failing to write a
// skill, as opposed to the file not landing on disk.
type generateError struct{ err error }

func (e *generateError) Error() string { return e.err.Error() }
func (e *generateError) Unwrap() error { return e.err }

// writeSkill generates one skill from the given sessions and writes it to
// <out>/<slug>/SKILL.md, returning that path.
func writeSkill(ctx context.Context, client *tapes.Client, out string, ids []string) (string, error) {
	skill, err := client.GenerateSkill(ctx, ids)
	if err != nil {
		return "", &generateError{err}
	}
	markdown, err := client.SkillMarkdown(ctx, skill.ID)
	if err != nil {
		return "", err
	}
	dest := filepath.Join(out, skill.Slug, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(dest, []byte(markdown), 0o644); err != nil {
		return "", err
	}
	return dest, nil
}

// skill writes one skill from sessions the user picked, usually the ids
// `search -q` printed.
func skill(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("skill", flag.ContinueOnError)
	out := fs.String("out", "tapes-skills", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ids := fs.Args()
	if len(ids) == 0 {
		return errors.New("skill needs session ids: tapes-skill-report skill $(tapes-skill-report search -q \"query\")")
	}
	client := tapes.New(stack.API, stack.Ingest)
	note("generating one skill from %d session(s)", len(ids))
	dest, err := writeSkill(ctx, client, *out, ids)
	if err != nil {
		return fmt.Errorf("generate failed (is the stack up?): %w", err)
	}
	fmt.Println(dest)
	return nil
}

// detect reads every derived session from the API and runs the detector.
func detect(ctx context.Context, client *tapes.Client) ([]recommend.Suggestion, map[string]recommend.Session, error) {
	rows, err := client.Sessions(ctx, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot read %s: %w", client.API, err)
	}
	var sessions []recommend.Session
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
		traces, err := client.Traces(ctx, row.ID)
		if err != nil {
			return nil, nil, err
		}
		tokens := recommend.SessionTokens(traceText(traces))
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
		return nil, nil, err
	}
	skills := make([]recommend.Skill, 0, len(existing))
	for _, sk := range existing {
		skills = append(skills, recommend.Skill{
			ID: sk.ID, Slug: sk.Slug, Name: sk.Name, Description: sk.Description,
			Tags: sk.Tags, SourceSessionIDs: sk.SourceIDs,
		})
	}
	note("%d sessions with turns, %d existing skills", len(sessions), len(skills))
	byID := map[string]recommend.Session{}
	for _, s := range sessions {
		byID[s.ID] = s
	}
	return recommend.Detect(sessions, skills, recommend.MinSessions), byID, nil
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

func suggest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("suggest", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	client := tapes.New(stack.API, stack.Ingest)
	suggestions, byID, err := detect(ctx, client)
	if err != nil {
		return err
	}
	if len(suggestions) == 0 {
		fmt.Println("no group of 3 or more sessions repeated the same work")
		return nil
	}
	for n, s := range suggestions {
		fmt.Printf("%d. [%s] %s\n   %s\n", n+1, s.Kind, s.Title, s.Why)
		for _, id := range s.SessionIDs[:min(6, len(s.SessionIDs))] {
			sess := byID[id]
			fmt.Printf("   - %s  %s: %s\n", id, sess.Project, truncate(sess.Title, 60))
		}
		if len(s.SessionIDs) > 6 {
			fmt.Printf("   - … %d more\n", len(s.SessionIDs)-6)
		}
		if s.Skill != nil {
			fmt.Printf("   skill: %s (%s)\n", s.Skill.Slug, s.Skill.ID)
		}
		fmt.Println()
	}
	return nil
}

func sessions(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sessions", flag.ContinueOnError)
	limit := fs.Int("limit", 50, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	client := tapes.New(stack.API, stack.Ingest)
	rows, err := client.Sessions(ctx, 0)
	if err != nil {
		return fmt.Errorf("cannot read %s (is the stack up? run tapes-skill-report first): %w", client.API, err)
	}
	if len(rows) == 0 {
		fmt.Println("no sessions imported yet")
		return nil
	}
	fmt.Printf("%-10s  %5s  %-20s  %-48s  %s\n", "DATE", "TURNS", "PROJECT", "TITLE", "ID")
	seen := map[string]bool{} // one row per harness session, whatever it was filed under
	printed := 0
	for _, r := range rows {
		if key := r.HarnessSessionID; key != "" {
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		if *limit > 0 && printed == *limit {
			break
		}
		printed++
		fmt.Printf("%-10s  %5d  %-20s  %-48s  %s\n",
			r.StartedAt.Local().Format("2006-01-02"),
			r.Rollup.TurnCount,
			truncate(filepath.Base(r.Cwd), 20),
			truncate(oneLine(r.DisplayTitle), 48),
			r.ID)
	}
	return nil
}

func search(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	top := fs.Int("k", 5, "")
	quiet := fs.Bool("q", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	query := strings.Join(fs.Args(), " ")
	if query == "" {
		return errors.New("search needs a query")
	}
	client := tapes.New(stack.API, stack.Ingest)
	// Ask for twice as many: a harness session filed under two ids returns
	// every hit twice, and the duplicates are dropped below.
	hits, err := client.Search(ctx, query, *top*2)
	if err != nil {
		return fmt.Errorf("search failed (is the stack up?): %w", err)
	}
	if len(hits) == 0 {
		fmt.Fprintln(os.Stderr, "no hits; embeddings run in the background after import, so try again in a minute")
		return nil
	}
	// One entry per harness session, whatever it was filed under.
	harness := map[string]string{}
	if rows, err := client.Sessions(ctx, 0); err == nil {
		for _, r := range rows {
			if r.HarnessSessionID != "" {
				harness[r.ID] = r.HarnessSessionID
			}
		}
	}
	key := func(h tapes.Hit) string {
		if k := harness[h.SessionID]; k != "" {
			return k
		}
		return h.SessionID
	}
	seen := map[string]bool{}
	if *quiet {
		// The shape `skill` takes, so the two compose through $(...).
		for _, h := range hits {
			if len(seen) < *top && !seen[key(h)] {
				seen[key(h)] = true
				fmt.Println(h.SessionID)
			}
		}
		return nil
	}
	for _, h := range hits {
		turn := key(h) + "\x00" + h.UserPrompt + "\x00" + h.Snippet
		if len(seen) == *top || seen[turn] {
			continue
		}
		seen[turn] = true
		fmt.Printf("%.2f  %s  %s\n", h.Score, h.StartedAt.Local().Format("2006-01-02"), h.SessionID)
		p, s := oneLine(h.UserPrompt), oneLine(h.Snippet)
		if p != "" {
			fmt.Printf("      prompt: %s\n", truncate(p, 100))
		}
		if s != "" && s != p {
			fmt.Printf("      %s\n", truncate(s, 100))
		}
	}
	return nil
}

func down(ctx context.Context) error {
	st, err := stack.New(false)
	if err != nil {
		return err
	}
	return st.Down(ctx)
}

// loadDotEnv reads KEY=value lines from ./.env into the environment,
// without overriding what the shell already exported. docker compose does
// the same for a .env beside the compose file, but the compose file lives
// in the cache dir, so the tool passes the values through its own
// environment.
func loadDotEnv() {
	f, err := os.Open(".env")
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(strings.TrimPrefix(key, "export "))
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if os.Getenv(key) == "" {
			os.Setenv(key, value)
		}
	}
}

func waitFor(ctx context.Context, ok func(context.Context) bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if ok(ctx) {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("timeout")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// waitForQueue polls the derive queue until it drains. The first poll waits
// a few seconds so the uploads have been marked dirty.
func waitForQueue(ctx context.Context, st *stack.Stack) error {
	time.Sleep(5 * time.Second)
	for {
		n, err := st.QueueDepth(ctx)
		if err != nil {
			return fmt.Errorf("reading derive queue: %w", err)
		}
		if n == 0 {
			note("done                    ")
			return nil
		}
		fmt.Fprintf(os.Stderr, "   %d session(s) left\r", n)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

func skillsWritten(out string) int {
	n := 0
	filepath.WalkDir(out, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == "SKILL.md" {
			n++
		}
		return nil
	})
	return n
}

func username() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "unknown"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
