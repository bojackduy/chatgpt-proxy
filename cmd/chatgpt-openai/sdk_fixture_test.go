package main

import (
	"encoding/json"
	"testing"
)

// Captured from installed OpenCode 1.18.35 through an isolated local endpoint,
// not handwritten SDK guesses. Text values are stable substitution tokens;
// the harmless fixture path is relocated. Roles, ordering and encodings remain.
const capturedSDKRequests = `[
 {"lane":"title","messages":[{"role":"system","content":"sanitized-text-0"},{"role":"user","content":"sanitized-text-1"},{"role":"user","content":"sanitized-text-2"}]},
 {"lane":"main","messages":[{"role":"system","content":"sanitized-text-3"},{"role":"user","content":"sanitized-text-4"}]},
 {"lane":"main","messages":[{"role":"system","content":"sanitized-text-3"},{"role":"user","content":"sanitized-text-4"},{"role":"assistant","content":"","tool_calls":[{"id":"call_fixture_read","type":"function","function":{"name":"read","arguments":"{\"filePath\":\"/isolated/fixture.txt\"}"}}]},{"role":"tool","tool_call_id":"call_fixture_read","content":"sanitized-text-6"}]}
]`

func TestCapturedSDKTitleAndToolRepresentations(t *testing.T) {
	var captured []struct {
		Lane     string    `json:"lane"`
		Messages []message `json:"messages"`
	}
	if err := json.Unmarshal([]byte(capturedSDKRequests), &captured); err != nil {
		t.Fatal(err)
	}
	// Without lane isolation the real title request differs at the very first
	// system message; neither tool count nor the shared session ID identifies it.
	if got := firstMismatch(captured[1].Messages, captured[0].Messages); got == nil || got.Index != 0 || got.Field != "content" {
		t.Fatalf("title mismatch %+v", got)
	}
	expected := append([]message{}, captured[2].Messages[:3]...)
	expected[2].ToolCalls = append([]toolCall(nil), expected[2].ToolCalls...)
	// Fixture endpoint returned spaced JSON; real SDK serialized minified JSON.
	expected[2].ToolCalls[0].Function.Arguments = `{"filePath": "/isolated/fixture.txt"}`
	if got := firstMismatch(captured[2].Messages, expected); got != nil {
		t.Fatalf("SDK JSON formatting %+v", got)
	}
	// Same shared header, separate lane: auxiliary cannot become main's prefix.
	f := &fakeBackend{}
	h := testHandler(f)
	assistant(t, laneRequest(h, "captured-session", captured[0].Lane, completionsRequest{Messages: captured[0].Messages}))
	a := assistant(t, laneRequest(h, "captured-session", captured[1].Lane, completionsRequest{Messages: captured[1].Messages}))
	url := h.sessions["captured-session"].URL
	msgs := append(append([]message{}, captured[1].Messages...), a, message{Role: "user", Content: "followup"})
	assistant(t, laneRequest(h, "captured-session", "title", completionsRequest{Messages: captured[0].Messages}))
	assistant(t, laneRequest(h, "captured-session", "main", completionsRequest{Messages: msgs}))
	if h.sessions["captured-session"].URL != url {
		t.Fatal("captured title request lost main URL")
	}
}
