// tapes-skills-demo turns a month of agent history into recommended skills.
//
// It reads Codex and Claude Code, whichever of the two is on the machine,
// and both when both are.
//
//	tapes-skills-demo                  import, derive, write ./tapes-skills
//	tapes-skills-demo --since-days 90  a wider window
//	tapes-skills-demo --harness claude only one of the two
//	tapes-skills-demo --ollama         the same, with local models only
//	tapes-skills-demo sessions         what was imported
//	tapes-skills-demo search "query"   semantic search over the imported work
//	tapes-skills-demo suggest          show the clusters without generating
//	tapes-skills-demo down             stop the stack and delete its data
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
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pcc-labs/tapes-test/internal/history"
	"github.com/pcc-labs/tapes-test/internal/stack"
	"github.com/pcc-labs/tapes-test/internal/tapes"
)

const usage = `usage: tapes-skills-demo [command] [flags]

commands:
  run        import your agent history, derive it, write skills (default)
  check      say whether a run would work, and what to fix if not
  sessions   list imported sessions
  search     semantic search over imported sessions
  suggest    show what your sessions look like and the skills they could be
  skill      write a skill: by suggestion number, or from session ids
  down       stop the stack and delete its data
  version    print the version

Codex and Claude Code are both read, whichever of the two is on the machine.

run flags:
  --since-days N    sessions modified in the last N days (default 30; 0 = all)
  --out DIR         where <slug>/SKILL.md is written (default tapes-skills)
  --harness NAME    read only one: codex, or claude (default both)
  --codex-root DIR  Codex sessions tree (default ~/.codex/sessions)
  --claude-root DIR Claude Code projects tree (default ~/.claude/projects)
  --ollama          use a local Ollama instead of OpenAI: nothing leaves the
                    machine, but skills are slower and rougher
  --yes             write every suggested skill without asking

check flags:
  --harness, --codex-root, --claude-root, --ollama   as for run

sessions flags:
  --limit N        rows to print (default 50; 0 = all)

search flags:
  -k N             hits to return (default 5)
  -q               print only session ids, one per line, best first

suggest flags:
  --since-days N, --codex-root DIR, --claude-root DIR   as for run

skill flags:
  --out DIR        where <slug>/SKILL.md is written (default tapes-skills)
  --since-days N   the window the suggestion numbers came from (default 30)

  tapes-skills-demo skill 1 3
  tapes-skills-demo skill $(tapes-skills-demo search -q "how I fixed auth")
`

// version is set by the release build.
var version = "dev"

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
	case "check":
		err = check(ctx, args)
	case "down":
		err = down(ctx)
	case "version", "--version":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		err = fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
	if errors.Is(err, flag.ErrHelp) {
		return // the flag set already printed the usage
	}
	if err != nil {
		fail(err)
		os.Exit(1)
	}
}

// newFlags is a subcommand's flag set: -h and --help print the usage above
// and exit cleanly, instead of Go's bare flag list and an error.
func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	fs.Usage = func() { fmt.Print(usage) }
	return fs
}

func run(ctx context.Context, args []string) error {
	fs := newFlags("run")
	sinceDays := fs.Int("since-days", 30, "")
	out := fs.String("out", "tapes-skills", "")
	roots := addHistoryFlags(fs)
	ollama := fs.Bool("ollama", false, "")
	yes := fs.Bool("yes", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	loadDotEnv()
	if !*ollama {
		if err := checkOpenAIKey(ctx); err != nil {
			return err
		}
	}
	found, err := roots.present()
	if err != nil {
		return err
	}
	st, err := stack.New(*ollama)
	if err != nil {
		return err
	}

	say("1/5 reading the last %d day(s) of history from %s", *sinceDays, names(found))
	var sessions []history.Session
	for _, src := range found {
		read, line, err := src.load(src.root, *sinceDays)
		if err != nil {
			return err
		}
		note("%s: %s", src.name, line)
		sessions = append(sessions, read...)
	}
	if len(sessions) == 0 {
		return fmt.Errorf("nothing to import from %s; widen --since-days, or point at the history with --codex-root / --claude-root", roots.paths())
	}

	say("2/5 starting tapes (postgres, tapes, derive workers, skills and search cassettes)")
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
			HarnessID:        s.Harness,
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

	say("4/5 deriving sessions, four at a time (the longest session sets the pace)")
	if err := waitForQueue(ctx, st); err != nil {
		return err
	}

	say("5/5 what your sessions look like")
	c, err := detect(ctx, client, readFeedback(roots.codex, *sinceDays))
	if err != nil {
		return err
	}
	c.show(os.Stderr, errUI, *sinceDays)
	chosen, err := c.pick(*yes)
	if err != nil {
		return err
	}
	if len(chosen) == 0 {
		if len(c.suggestions) > 0 {
			note("nothing written; `tapes-skills-demo skill N` writes one later")
		}
	} else {
		say("writing %d skill(s)", len(chosen))
		// A hosted model writes several skills at once. A local one is a single
		// GPU on a 30s budget per call, so it takes them one at a time.
		parallel := 3
		if *ollama {
			parallel = 1
		}
		written, failed, err := c.write(ctx, client, *out, chosen, parallel)
		if err != nil {
			return err
		}
		switch {
		case written > 0:
			say("skills are in ./%s", *out)
		case failed > 0:
			say("writing the skills failed (above)")
			if *ollama {
				note("local models often run out the cassette's 30s budget; an OpenAI key is the reliable path")
			}
		}
	}
	note("browse what was imported:  tapes-skills-demo sessions")
	note("search it:                 tapes-skills-demo search \"how did I fix auth\"")
	note("stack is up at %s; `tapes-skills-demo down` removes it and its data", stack.API)
	return nil
}

// each runs fn(0..n-1) with at most limit in flight and returns the first
// error, after every started call has finished.
func each(n, limit int, fn func(i int) error) error {
	var (
		wg    sync.WaitGroup
		once  sync.Once
		first error
		slots = make(chan struct{}, max(1, limit))
	)
	for i := range n {
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			if err := fn(i); err != nil {
				once.Do(func() { first = err })
			}
		}()
	}
	wg.Wait()
	return first
}

// slugDir is one directory name from a slug the server chose. A slug is
// never a path: "../.." from a cassette that was tampered with, or simply
// wrong, would otherwise write outside --out.
func slugDir(slug string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		}
		return '-'
	}, slug)
	clean = strings.Trim(clean, "-")
	if clean == "" {
		return "skill"
	}
	return truncate(clean, 80)
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
	dest := filepath.Join(out, slugDir(skill.Slug), "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(dest, []byte(markdown), 0o644); err != nil {
		return "", err
	}
	return dest, nil
}

// checkOpenAIKey fails before any work starts when the key is missing or
// OpenAI rejects it; otherwise a bad key surfaces minutes later, as every
// skill failing to generate. Anything short of a rejection (offline, a
// proxy in the way) is left for the run itself to meet.
func checkOpenAIKey(ctx context.Context) error {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return errors.New("no OPENAI_API_KEY: export it, or put OPENAI_API_KEY=sk-... in ./.env (or pass --ollama to stay fully local)")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.openai.com/v1/models", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return errors.New("OpenAI rejected OPENAI_API_KEY (401). Check the key at https://platform.openai.com/api-keys; a .env in this directory is only used when the shell has not exported one")
	}
	return nil
}

// check reports whether a run would work, one line per requirement, and
// fails if any does not hold.
func check(ctx context.Context, args []string) error {
	fs := newFlags("check")
	roots := addHistoryFlags(fs)
	ollama := fs.Bool("ollama", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	loadDotEnv()
	failed := 0
	report := func(name string, err error, ok string) {
		if err != nil {
			failed++
			fmt.Printf("%s  %s: %v\n", outUI.badTag.Render("FAIL"), outUI.strong.Render(name), err)
			return
		}
		fmt.Printf("%s    %s: %s\n", outUI.okTag.Render("ok"), outUI.strong.Render(name), ok)
	}
	report("docker", stack.Preflight(), "installed, running, has compose")
	if *ollama {
		report("models", nil, "local Ollama; no key needed")
	} else {
		report("openai key", checkOpenAIKey(ctx), "set and accepted")
	}
	// Each history is reported on its own line, and only finding none of
	// them fails the check: one of the two is enough to run.
	all, err := roots.all()
	if err != nil {
		return err
	}
	histories := 0
	for _, src := range all {
		name := strings.ToLower(src.name) + " history"
		if _, err := os.Stat(src.root); err != nil {
			fmt.Printf("%s     %s: not on this machine (%s)\n", outUI.dim.Render("--"), outUI.strong.Render(name), src.root)
			continue
		}
		paths, err := src.files(src.root, 30)
		if err == nil && len(paths) == 0 {
			err = fmt.Errorf("no sessions from the last 30 days under %s; try --since-days 90 when you run", src.root)
		}
		if err == nil {
			histories++
		}
		report(name, err, fmt.Sprintf("%d session file(s) from the last 30 days in %s", len(paths), src.root))
	}
	if histories == 0 {
		failed++
		fmt.Printf("%s  %s: nothing to read from %s\n", outUI.badTag.Render("FAIL"), outUI.strong.Render("history"), roots.paths())
	}
	client := tapes.New(stack.API, stack.Ingest)
	if client.Ping(ctx) {
		report("stack", nil, "already up at "+stack.API)
	} else {
		report("stack", nil, "not started yet; `tapes-skills-demo` starts it")
	}
	if failed > 0 {
		return fmt.Errorf("%d check(s) failed; fix those and run `tapes-skills-demo check` again", failed)
	}
	fmt.Println(outUI.okTag.Render("ready:") + " run `tapes-skills-demo`")
	return nil
}

func sessions(ctx context.Context, args []string) error {
	fs := newFlags("sessions")
	limit := fs.Int("limit", 50, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	client := tapes.New(stack.API, stack.Ingest)
	rows, err := client.Sessions(ctx, 0)
	if err != nil {
		return fmt.Errorf("cannot read %s (is the stack up? run tapes-skills-demo first): %w", client.API, err)
	}
	if len(rows) == 0 {
		fmt.Println("no sessions imported yet")
		return nil
	}
	fmt.Println(outUI.dim.Render(fmt.Sprintf("%-10s  %5s  %-20s  %-48s  %s", "DATE", "TURNS", "PROJECT", "TITLE", "ID")))
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
			outUI.dim.Render(r.ID))
	}
	return nil
}

func search(ctx context.Context, args []string) error {
	fs := newFlags("search")
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
		fmt.Printf("%s  %s  %s\n", outUI.score.Render(fmt.Sprintf("%.2f", h.Score)), h.StartedAt.Local().Format("2006-01-02"), outUI.dim.Render(h.SessionID))
		p, s := oneLine(h.UserPrompt), oneLine(h.Snippet)
		if p != "" {
			fmt.Printf("      %s %s\n", outUI.dim.Render("prompt:"), truncate(p, 100))
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
//
// One failed read is not the end: the database restarts, Docker hiccups. It
// gives up after a run of failures, or when the queue has not moved for
// stallAfter, which is several times the longest session seen in practice.
func waitForQueue(ctx context.Context, st *stack.Stack) error {
	const (
		maxFailures = 10
		stallAfter  = 15 * time.Minute
	)
	time.Sleep(3 * time.Second)
	failures, last, moved := 0, -1, time.Now()
	for {
		n, err := st.QueueDepth(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			failures++
			if failures == maxFailures {
				return fmt.Errorf("lost contact with the tapes database (%v). Is Docker still running? Nothing is lost: run `tapes-skills-demo` again and it picks up where it stopped", err)
			}
			n = last
		default:
			failures = 0
		}
		if n != last {
			last, moved = n, time.Now()
		}
		if time.Since(moved) > stallAfter {
			return fmt.Errorf("deriving has not moved in %s with %d session(s) left. See why with: docker compose -f %s logs derive tapes", stallAfter, n, st.File())
		}
		if err != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}
		if n == 0 {
			note("done                    ")
			return nil
		}
		fmt.Fprintf(os.Stderr, "   %d session(s) left\r", n)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func username() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "unknown"
}

// truncate keeps the first n characters, never splitting one.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
