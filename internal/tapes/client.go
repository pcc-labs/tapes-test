// Package tapes is the slice of the tapes read and ingest APIs this tool
// uses: list sessions, read a session's traces, list and generate skills,
// search spans, and upload a transcript.
package tapes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Client talks to one tapes deployment.
type Client struct {
	API    string // read API, e.g. http://127.0.0.1:18081
	Ingest string // ingest API, e.g. http://127.0.0.1:18082
	HTTP   *http.Client
}

// New returns a client with a generous timeout: skill generation is one
// model call per cluster and can take a minute.
func New(api, ingest string) *Client {
	return &Client{API: api, Ingest: ingest, HTTP: &http.Client{Timeout: 3 * time.Minute}}
}

// StatusError is a non-2xx response.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	body := e.Body
	if len(body) > 200 {
		body = body[:200]
	}
	return fmt.Sprintf("HTTP %d: %s", e.Code, body)
}

// Session is one row of GET /v1/sessions.
type Session struct {
	ID               string    `json:"id"`
	HarnessID        string    `json:"harness_id"`
	HarnessSessionID string    `json:"harness_session_id"`
	Cwd              string    `json:"cwd"`
	StartedAt        time.Time `json:"started_at"`
	LastSeenAt       time.Time `json:"last_seen_at"`
	AuthSubject      string    `json:"auth_subject"`
	DisplayTitle     string    `json:"display_title"`
	Rollup           struct {
		Status    string `json:"status"`
		TurnCount int    `json:"turn_count"`
		Model     string `json:"model"`
		Usage     struct {
			InputTokens  int64   `json:"input_tokens"`
			OutputTokens int64   `json:"output_tokens"`
			CostUSD      float64 `json:"cost_usd"`
		} `json:"usage"`
	} `json:"rollup"`
}

// Sessions pages through GET /v1/sessions, newest first, stopping after
// limit rows (0 = all).
func (c *Client) Sessions(ctx context.Context, limit int) ([]Session, error) {
	var out []Session
	cursor := ""
	for {
		page := 200
		if limit > 0 && limit-len(out) < page {
			page = limit - len(out)
		}
		q := url.Values{"limit": {strconv.Itoa(page)}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var resp struct {
			Items      []Session `json:"items"`
			NextCursor string    `json:"next_cursor"`
		}
		if err := c.getJSON(ctx, c.API+"/v1/sessions?"+q.Encode(), &resp); err != nil {
			return out, err
		}
		out = append(out, resp.Items...)
		if resp.NextCursor == "" || (limit > 0 && len(out) >= limit) {
			return out, nil
		}
		cursor = resp.NextCursor
	}
}

// Trace is one turn of GET /v1/sessions/{id}/traces.
type Trace struct {
	Trace struct {
		UserPrompt      string `json:"user_prompt"`
		ResponsePreview string `json:"response_preview"`
	} `json:"trace"`
	Spans []struct {
		Kind  string `json:"kind"`
		Name  string `json:"name"`
		Model string `json:"model"`
	} `json:"spans"`
}

// Traces reads a session's derived turns.
func (c *Client) Traces(ctx context.Context, sessionID string) ([]Trace, error) {
	var resp struct {
		Traces []Trace `json:"traces"`
	}
	err := c.getJSON(ctx, c.API+"/v1/sessions/"+url.PathEscape(sessionID)+"/traces", &resp)
	return resp.Traces, err
}

// Skill is one row of the skills cassette.
type Skill struct {
	ID          string   `json:"id"`
	Slug        string   `json:"slug"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	SourceIDs   []string `json:"originatingSessionIds"`
}

// Skills lists the deployment's skills; empty when there is no skills
// cassette.
func (c *Client) Skills(ctx context.Context) ([]Skill, error) {
	var resp struct {
		Items []Skill `json:"items"`
	}
	err := c.getJSON(ctx, c.API+"/v1/cassettes/skills", &resp)
	var se *StatusError
	if errors.As(err, &se) && se.Code == http.StatusNotFound {
		return nil, nil
	}
	return resp.Items, err
}

// GenerateSkill asks the skills cassette to write a skill from sessions and
// returns the id and slug it stored.
func (c *Client) GenerateSkill(ctx context.Context, sessionIDs []string) (Skill, error) {
	body := map[string]any{"sessionIds": sessionIDs, "hint": map[string]any{"type": "workflow"}}
	var skill Skill
	err := c.postJSON(ctx, c.API+"/v1/cassettes/skills/generate", body, &skill)
	return skill, err
}

// SkillMarkdown downloads a skill as SKILL.md.
func (c *Client) SkillMarkdown(ctx context.Context, id string) (string, error) {
	b, err := c.get(ctx, c.API+"/v1/cassettes/skills/"+url.PathEscape(id)+"/skill.md")
	return string(b), err
}

// Hit is one result of the search cassette.
type Hit struct {
	SessionID  string    `json:"session_id"`
	TraceID    string    `json:"trace_id"`
	Score      float64   `json:"score"`
	StartedAt  time.Time `json:"started_at"`
	UserPrompt string    `json:"user_prompt"`
	Snippet    string    `json:"snippet"`
}

// Search runs semantic search over captured spans.
func (c *Client) Search(ctx context.Context, query string, topK int) ([]Hit, error) {
	q := url.Values{"query": {query}, "top_k": {strconv.Itoa(topK)}}
	var resp struct {
		Results []Hit `json:"results"`
	}
	err := c.getJSON(ctx, c.API+"/v1/cassettes/search/spans?"+q.Encode(), &resp)
	return resp.Results, err
}

// Transcript is the body of POST /v1/ingest/transcript for a main session
// transcript.
type Transcript struct {
	HarnessID        string
	HarnessSessionID string
	HarnessVersion   string
	Cwd              string
	AuthSubject      string
	Records          any
}

// UploadTranscript appends one transcript. The server dedups on content, so
// re-uploading is a no-op.
func (c *Client) UploadTranscript(ctx context.Context, t Transcript) error {
	body := map[string]any{
		"session": map[string]any{
			"org_id":             "",
			"auth_subject":       t.AuthSubject,
			"harness_id":         t.HarnessID,
			"harness_session_id": t.HarnessSessionID,
			"harness_version":    t.HarnessVersion,
			"cwd":                t.Cwd,
		},
		"records": t.Records,
	}
	return c.postJSON(ctx, c.Ingest+"/v1/ingest/transcript", body, nil)
}

// Ping is true when the read API answers.
func (c *Client) Ping(ctx context.Context) bool {
	_, err := c.get(ctx, c.API+"/ping")
	return err == nil
}

func (c *Client) get(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	return c.do(req)
}

func (c *Client) getJSON(ctx context.Context, u string, out any) error {
	b, err := c.get(ctx, u)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func (c *Client) postJSON(ctx context.Context, u string, in, out any) error {
	payload, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	b, err := c.do(req)
	if err != nil || out == nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func (c *Client) do(req *http.Request) ([]byte, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &StatusError{Code: resp.StatusCode, Body: string(b)}
	}
	return b, nil
}
