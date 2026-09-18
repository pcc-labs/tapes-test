package codex

import (
	"strings"
	"testing"
)

const rollout = `{"type":"session_meta","payload":{"id":"sess-1","cwd":"/home/me/app","cli_version":"0.150.0"}}
{"type":"turn_context","payload":{"model":"gpt-5-codex"}}
{"type":"response_item","timestamp":"t1","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the build"}]}}
{"type":"response_item","timestamp":"t2","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"system stuff"}]}}
{"type":"response_item","timestamp":"t3","payload":{"type":"reasoning","summary":[{"type":"summary_text","text":"Looking at"},{"type":"summary_text","text":"the Makefile"}]}}
{"type":"response_item","timestamp":"t4","payload":{"type":"function_call","call_id":"c1","name":"exec","arguments":"{\"cmd\":\"make\"}"}}
{"type":"response_item","timestamp":"t5","payload":{"type":"function_call_output","call_id":"c1","output":[{"type":"output_text","text":"ok"}]}}
{"type":"response_item","timestamp":"t6","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Done."}]}}
{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":10,"cached_input_tokens":4,"output_tokens":3,"reasoning_output_tokens":1}}}}
not json
`

func TestConvert(t *testing.T) {
	var summary Summary
	s, ok := Convert(strings.NewReader(rollout), &summary)
	if !ok {
		t.Fatal("expected a session")
	}
	if s.ID != "sess-1" || s.Cwd != "/home/me/app" || s.Version != "0.150.0" || s.Model != "gpt-5-codex" {
		t.Errorf("meta: %+v", s)
	}
	if summary.BadLines != 1 {
		t.Errorf("bad lines = %d, want 1", summary.BadLines)
	}
	kinds := []string{}
	for _, r := range s.Records {
		kinds = append(kinds, r["type"].(string))
	}
	want := "user assistant assistant user assistant"
	if got := strings.Join(kinds, " "); got != want {
		t.Fatalf("records = %q, want %q", got, want)
	}

	// The developer message is dropped, so the chain is user -> thinking.
	if s.Records[1]["parentUuid"] != s.Records[0]["uuid"] {
		t.Error("parentUuid does not chain")
	}
	thinking := s.Records[1]["message"].(map[string]any)["content"].([]any)[0].(map[string]any)
	if thinking["thinking"] != "Looking at the Makefile" {
		t.Errorf("thinking = %v", thinking["thinking"])
	}
	call := s.Records[2]["message"].(map[string]any)["content"].([]any)[0].(map[string]any)
	if call["input"].(map[string]any)["cmd"] != "make" {
		t.Errorf("tool input = %v", call["input"])
	}
	result := s.Records[3]["message"].(map[string]any)["content"].([]any)[0].(map[string]any)
	if result["tool_use_id"] != "c1" || !strings.Contains(result["content"].(string), `"ok"`) {
		t.Errorf("tool result = %v", result)
	}
	// token_count lands on the assistant record it follows.
	usage := s.Records[4]["message"].(map[string]any)["usage"].(map[string]any)
	if usage["input_tokens"] != 10.0 || usage["cache_read_input_tokens"] != 4.0 {
		t.Errorf("usage = %v", usage)
	}
	if s.Records[4]["message"].(map[string]any)["model"] != "gpt-5-codex" {
		t.Error("model not carried from turn_context")
	}
}

func TestConvertSkipsEmpty(t *testing.T) {
	var summary Summary
	if _, ok := Convert(strings.NewReader(`{"type":"response_item","payload":{}}`), &summary); ok || summary.SkippedNoMeta != 1 {
		t.Error("rollout without session_meta should be skipped")
	}
	if _, ok := Convert(strings.NewReader(`{"type":"session_meta","payload":{"id":"x"}}`), &summary); ok || summary.SkippedEmpty != 1 {
		t.Error("rollout with no records should be skipped")
	}
}
