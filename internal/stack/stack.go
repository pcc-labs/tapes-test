// Package stack drives the local docker compose deployment. The compose file
// is embedded so the binary works from any directory, including after a
// plain `go install`; it is written to the user's cache dir before every
// docker call so `docker compose` always sees the copy this build shipped.
package stack

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

//go:embed compose.yaml
var composeYAML []byte

const (
	API    = "http://127.0.0.1:18081"
	Ingest = "http://127.0.0.1:18082"
)

// Stack is one compose project on this machine.
type Stack struct {
	file string
}

// New writes the compose file into place and returns a handle to it.
func New() (*Stack, error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, fmt.Errorf("docker is not installed: https://docs.docker.com/desktop/")
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
	return &Stack{file: file}, nil
}

// File is the compose file's path, for `docker compose -f … logs`.
func (s *Stack) File() string { return s.file }

func (s *Stack) cmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "docker", append([]string{"compose", "-f", s.file}, args...)...)
	cmd.Env = os.Environ()
	return cmd
}

// Up starts the containers, pulling images quietly. Output goes to stderr
// minus compose's per-container "Running" lines, which say nothing on a
// re-run.
func (s *Stack) Up(ctx context.Context) error {
	cmd := s.cmd(ctx, "up", "-d", "--quiet-pull")
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" || strings.Contains(line, "Running") {
			continue
		}
		fmt.Fprintln(os.Stderr, "  ", line)
	}
	if err != nil {
		return fmt.Errorf("docker compose up: %w", err)
	}
	return nil
}

// Down stops the containers and deletes their data.
func (s *Stack) Down(ctx context.Context) error {
	cmd := s.cmd(ctx, "down", "-v")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
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
