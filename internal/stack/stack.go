// Package stack drives the local docker compose deployment. The compose
// files are embedded so the binary works from any directory, including after
// a plain `go install`; they are written to the user's cache dir before every
// docker call so `docker compose` always sees the copy this build shipped.
// compose.yaml is the OpenAI stack; compose.ollama.yaml layers over it for
// --ollama.
package stack

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

//go:embed compose.yaml
var composeYAML []byte

//go:embed compose.ollama.yaml
var composeOllamaYAML []byte

const (
	API    = "http://127.0.0.1:18081"
	Ingest = "http://127.0.0.1:18082"

	// EmbeddingModel is what the search cassette embeds with under Ollama.
	EmbeddingModel = "embeddinggemma"
	// DefaultSkillModel writes skills under Ollama unless TAPES_SKILL_MODEL
	// names another.
	DefaultSkillModel = "llama3.2"

	hostOllama = "http://127.0.0.1:11434"
)

// Stack is one compose project on this machine.
type Stack struct {
	dir  string
	file string
	// ollamaURL is where the cassettes reach Ollama, empty in OpenAI mode.
	ollamaURL string
	// container is true when Ollama runs in compose rather than on the host.
	container bool
}

// New writes the compose files into place and returns a handle to them.
// With ollama, the cassettes use a local Ollama instead of OpenAI: the
// host's if one answers, otherwise a container.
func New(ollama bool) (*Stack, error) {
	if err := Preflight(); err != nil {
		return nil, err
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	dir = filepath.Join(dir, "tapes-skill-report")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	file := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(file, composeYAML, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.ollama.yaml"), composeOllamaYAML, 0o644); err != nil {
		return nil, err
	}
	s := &Stack{dir: dir, file: file}
	if ollama {
		// Containers reach a host Ollama through host.docker.internal. That
		// works on Docker Desktop; on Linux the host's Ollama must listen
		// beyond loopback (OLLAMA_HOST=0.0.0.0).
		if hostOllamaUp() {
			s.ollamaURL = "http://host.docker.internal:11434"
		} else {
			s.ollamaURL, s.container = "http://ollama:11434", true
		}
	}
	return s, nil
}

// Preflight checks Docker is installed, running, and has compose, and says
// what to do about whichever is missing.
func Preflight() error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker is not installed. Install Docker Desktop, start it, and run this again: https://docs.docker.com/desktop/")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		return fmt.Errorf("docker is installed but not running. Start Docker Desktop, wait for it to finish starting, and run this again")
	}
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		return fmt.Errorf("this docker has no `docker compose`. Update Docker Desktop, or install the compose plugin: https://docs.docker.com/compose/install/")
	}
	return nil
}

// explain turns compose output into the fix for the failures a first run
// actually meets; empty when it recognises nothing.
func explain(output string) string {
	switch {
	case strings.Contains(output, "port is already allocated"), strings.Contains(output, "address already in use"):
		return "ports 18081/18082 are taken by something else. If it is an old copy of this stack, run `tapes-skill-report down`; otherwise stop whatever is listening there"
	case strings.Contains(output, "authorization token has expired"), strings.Contains(output, "public.ecr.aws") && strings.Contains(output, "denied"):
		return "Docker has a stale login for public.ecr.aws. Run `docker logout public.ecr.aws` and try again"
	case strings.Contains(output, "no space left on device"):
		return "Docker is out of disk space. Free some with `docker system prune` and try again"
	case strings.Contains(output, "TLS handshake timeout"), strings.Contains(output, "no such host"), strings.Contains(output, "i/o timeout"):
		return "could not download the images. Check your network (or VPN) and try again"
	}
	return ""
}

// OllamaMode is true when the stack on this machine was started with
// --ollama, so a model call outside the cassettes should go there too.
func OllamaMode() bool {
	dir, err := os.UserCacheDir()
	if err != nil {
		return false
	}
	mode, err := os.ReadFile(filepath.Join(dir, "tapes-skill-report", "mode"))
	return err == nil && strings.TrimSpace(string(mode)) == "ollama"
}

func hostOllamaUp() bool {
	client := http.Client{Timeout: time.Second}
	resp, err := client.Get(hostOllama + "/api/version")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Ollama describes where Ollama runs, for the progress line; empty in
// OpenAI mode.
func (s *Stack) Ollama() string {
	switch {
	case s.ollamaURL == "":
		return ""
	case s.container:
		return "a container"
	default:
		return "this machine's " + hostOllama
	}
}

// File is the compose file's path, for `docker compose -f … logs`.
func (s *Stack) File() string { return s.file }

func (s *Stack) cmd(ctx context.Context, args ...string) *exec.Cmd {
	base := []string{"compose", "-f", s.file}
	if s.ollamaURL != "" {
		base = append(base, "-f", filepath.Join(s.dir, "compose.ollama.yaml"))
	}
	if s.container {
		base = append(base, "--profile", "ollama-container")
	}
	cmd := exec.CommandContext(ctx, "docker", append(base, args...)...)
	cmd.Env = os.Environ()
	if s.ollamaURL != "" {
		cmd.Env = append(cmd.Env, "TAPES_OLLAMA_URL="+s.ollamaURL)
	}
	return cmd
}

// hasData is true when an earlier run left a database behind.
func (s *Stack) hasData(ctx context.Context) bool {
	out, err := s.cmd(ctx, "volumes", "-q").Output()
	return err == nil && strings.Contains(string(out), "postgres-data")
}

func (s *Stack) mode() string {
	if s.ollamaURL != "" {
		return "ollama"
	}
	return "openai"
}

// Up starts the containers, pulling images quietly. Output goes to stderr
// minus compose's per-container "Running" lines, which say nothing on a
// re-run.
func (s *Stack) Up(ctx context.Context) error {
	// The two providers embed into different vector spaces, so search over
	// a mix of them ranks nonsense. One stack, one provider.
	modeFile := filepath.Join(s.dir, "mode")
	prev, err := os.ReadFile(modeFile)
	if err != nil && s.hasData(ctx) {
		prev = []byte("openai") // a stack from before --ollama existed
	}
	if len(prev) > 0 && string(prev) != s.mode() {
		return fmt.Errorf("this stack was built with %s; run `tapes-skill-report down` before switching to %s", prev, s.mode())
	}
	cmd := s.cmd(ctx, "up", "-d", "--quiet-pull")
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err = cmd.Run()
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" || strings.Contains(line, "Running") {
			continue
		}
		fmt.Fprintln(os.Stderr, "  ", line)
	}
	if err != nil {
		if why := explain(buf.String()); why != "" {
			return fmt.Errorf("could not start tapes: %s", why)
		}
		return fmt.Errorf("docker compose up: %w", err)
	}
	return os.WriteFile(modeFile, []byte(s.mode()), 0o644)
}

// PullModel makes sure Ollama has a model, downloading it the first time.
func (s *Stack) PullModel(ctx context.Context, model string) error {
	var cmd *exec.Cmd
	if s.container {
		cmd = s.cmd(ctx, "exec", "-T", "ollama", "ollama", "pull", model)
	} else {
		body := strings.NewReader(fmt.Sprintf(`{"model":%q,"stream":false}`, model))
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, hostOllama+"/api/pull", body)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("ollama pull %s: %w", model, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("ollama pull %s: HTTP %d", model, resp.StatusCode)
		}
		return nil
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ollama pull %s: %w: %s", model, err, lastLine(out))
	}
	return nil
}

func lastLine(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return lines[len(lines)-1]
}

// Down stops the containers and deletes their data.
func (s *Stack) Down(ctx context.Context) error {
	// --remove-orphans takes the Ollama container too, whichever mode the
	// stack was started in.
	cmd := s.cmd(ctx, "down", "-v", "--remove-orphans")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	os.Remove(filepath.Join(s.dir, "mode"))
	return nil
}

// QueueDepth is how many sessions still wait for derivation. Postgres is
// not published on the host, so this goes through the container.
func (s *Stack) QueueDepth(ctx context.Context) (int, error) {
	cmd := s.cmd(ctx, "exec", "-T", "postgres", "psql", "-qtA", "-U", "tapes", "-d", "tapes",
		"-c", "select count(*) from derive_queue")
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	var n int
	if _, err := fmt.Sscan(strings.TrimSpace(string(out)), &n); err != nil {
		return 0, fmt.Errorf("unexpected psql output %q", out)
	}
	return n, nil
}
