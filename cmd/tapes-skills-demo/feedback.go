package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/pcc-labs/tapes-test/internal/codex"
	"github.com/pcc-labs/tapes-test/internal/stack"
)

// The skills cassette writes a skill from whole sessions, so what it
// describes is what those sessions were about. A skill from browser comments
// has to read the comments themselves: they are the person's review, and the
// rules they restate are the skill. One model call over the comment list.

const maxFeedbackComments = 150

const feedbackPrompt = `These are browser comments a person left while reviewing UI an AI coding agent built. Each is one note pinned to one element of one page.

Write a SKILL.md an AI coding agent loads before building or changing UI for this person, so they stop having to leave these comments.

- Keep only rules the comments support: a preference two or more comments share, or one stated as a general rule. Drop one-off instructions about a single element.
- Write each rule as an instruction to the agent, in plain words, then quote one to three of the comments behind it, verbatim, in italics.
- Group the rules under short headings (layout and spacing, visual weight and color, components, copy, states and feedback, or whatever the comments are about).
- No preamble, no generic advice the comments do not support.

Start with this frontmatter and nothing before it:
---
name: <kebab-case-name>
description: Use when building or changing UI for this person. <one sentence on what the rules cover>
---

Comments, newest first, one per line as [page] comment:
%s`

// writeFeedbackSkill writes a skill of the rules the comments restate to
// <out>/<name>/SKILL.md and returns that path.
func writeFeedbackSkill(ctx context.Context, comments []codex.Comment, out string) (string, error) {
	if len(comments) > maxFeedbackComments {
		comments = comments[:maxFeedbackComments]
	}
	// The element's own text stays out: small models quote it back as if
	// the person had written it.
	var list strings.Builder
	for _, cm := range comments {
		fmt.Fprintf(&list, "- [%s] %s\n", cm.Page, oneLine(cm.Text))
	}
	markdown, err := complete(ctx, fmt.Sprintf(feedbackPrompt, list.String()))
	if err != nil {
		return "", &generateError{err}
	}
	markdown = strings.TrimSpace(fence.ReplaceAllString(markdown, ""))
	// Models like to sign off with a note about what they left out.
	if i := trailingNote.FindStringIndex(markdown); i != nil {
		markdown = strings.TrimSpace(markdown[:i[0]])
	}
	if !strings.HasPrefix(markdown, "---") {
		return "", &generateError{errors.New("the model did not return a SKILL.md")}
	}
	name := "browser-feedback"
	if m := frontmatterName.FindStringSubmatch(markdown); m != nil {
		name = m[1]
	}
	dest := filepath.Join(out, name, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	return dest, os.WriteFile(dest, []byte(markdown+"\n"), 0o644)
}

var (
	fence           = regexp.MustCompile("(?m)^```[a-z]*\\s*$")
	trailingNote    = regexp.MustCompile(`(?m)^\s*\**(?:Note|Notes)\**:`)
	frontmatterName = regexp.MustCompile(`(?m)^name:\s*"?([a-z0-9][a-z0-9-]*)"?\s*$`)
)

// complete sends one prompt to the provider the stack runs on: OpenAI with
// OPENAI_API_KEY, otherwise the local Ollama.
func complete(ctx context.Context, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	model := os.Getenv("TAPES_SKILL_MODEL")
	if key := os.Getenv("OPENAI_API_KEY"); key != "" && !stack.OllamaMode() {
		if model == "" {
			model = "gpt-4o-mini"
		}
		var resp struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		body := map[string]any{"model": model, "messages": []any{map[string]string{"role": "user", "content": prompt}}}
		if err := postModel(ctx, "https://api.openai.com/v1/chat/completions", key, body, &resp); err != nil {
			return "", err
		}
		if len(resp.Choices) == 0 {
			return "", errors.New("OpenAI returned no answer")
		}
		return resp.Choices[0].Message.Content, nil
	}
	if model == "" {
		model = stack.DefaultSkillModel
	}
	var resp struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	body := map[string]any{"model": model, "stream": false, "messages": []any{map[string]string{"role": "user", "content": prompt}}}
	if err := postModel(ctx, "http://127.0.0.1:11434/api/chat", "", body, &resp); err != nil {
		return "", fmt.Errorf("no OPENAI_API_KEY, and Ollama did not answer: %w", err)
	}
	return resp.Message.Content, nil
}

func postModel(ctx context.Context, url, key string, in, out any) error {
	payload, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(b), 200))
	}
	return json.Unmarshal(b, out)
}
