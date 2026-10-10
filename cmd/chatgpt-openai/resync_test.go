package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func resyncRequest(h *handler, history []message) *httptest.ResponseRecorder {
	b, _ := json.Marshal(map[string]any{"messages": history})
	r := httptest.NewRequest("POST", "/v1/sessions/resync", bytes.NewReader(b))
	r.Header.Set("X-Session-Id", "resync")
	w := httptest.NewRecorder()
	h.routes().ServeHTTP(w, r)
	return w
}
func TestResyncSameSessionImportsExecutedCallsInertly(t *testing.T) {
	f := &fakeBackend{}
	h := testHandler(f)
	req := weatherReq()
	calls := []toolCall{{ID: "completed", Type: "function", Function: callFunction{Name: "get_weather", Arguments: `{"city":"Hanoi"}`}}}
	recorded := append(append([]message{}, req.Messages...), message{Role: "assistant", ToolCalls: calls})
	history := append(append([]message{}, recorded...), message{Role: "tool", ToolCallID: "completed", Content: "already executed: 25 C"})
	h.sessions["resync"] = &session{URL: "https://chatgpt.com/c/uncertain", Messages: recorded, Dirty: true}
	if w := resyncRequest(h, history); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if !h.sessions["resync"].Dirty || len(f.prompts) != 0 {
		t.Fatal("arming cleared dirty/submitted")
	}
	req.Messages = history
	if w := request(h, "resync", req); w.Code != 409 || len(f.prompts) != 0 {
		t.Fatal("uncertain request resubmitted")
	}
	req.Messages = append(append([]message{}, history...), message{Role: "user", Content: "Summarize the completed result, no further calls"})
	req.Tools = nil
	req.ToolChoice = "none"
	f.answers = []string{"ACK", "summary"}
	out := assistant(t, request(h, "resync", req))
	if out.Content != "summary" || len(f.prompts) != 2 || f.urls[0] != "" || !strings.HasPrefix(f.prompts[0], historyIntro) || !strings.Contains(f.prompts[0], "inert historical transcript") || strings.Contains(f.prompts[1], "already executed") {
		t.Fatal("resync did not separate inert history/new request", out)
	}
	if h.sessions["resync"].Dirty || h.sessions["resync"].ResyncFrom != "https://chatgpt.com/c/uncertain" {
		t.Fatal("resync journal wrong")
	}
}
func TestResyncRejectsMissingDuplicateAndUnknownResults(t *testing.T) {
	for _, delta := range [][]message{nil, {{Role: "tool", ToolCallID: "wrong", Content: "result"}}, {{Role: "assistant", ToolCallID: "one", Content: "result"}}, {{Role: "tool", ToolCallID: "one", Content: ""}}} {
		f := &fakeBackend{}
		h := testHandler(f)
		base := []message{{Role: "assistant", ToolCalls: []toolCall{{ID: "one"}}}}
		h.sessions["resync"] = &session{URL: "https://chatgpt.com/c/stored", Messages: base, Dirty: true}
		w := resyncRequest(h, append(append([]message{}, base...), delta...))
		if w.Code != 409 || len(f.prompts) != 0 || len(h.sessions["resync"].ResyncMessages) != 0 {
			t.Fatal("invalid resync armed")
		}
	}
}

func TestFailedResyncContinuesAsOrdinaryDelta(t *testing.T) {
	f := &fakeBackend{answers: []string{"NOT ACK"}}
	h := testHandler(f)
	base := []message{{Role: "assistant", ToolCalls: []toolCall{{ID: "one"}}}}
	history := append(append([]message{}, base...), message{Role: "tool", ToolCallID: "one", Content: "completed"})
	h.sessions["resync"] = &session{URL: "https://chatgpt.com/c/old", Messages: base, Dirty: true}
	if resyncRequest(h, history).Code != 200 {
		t.Fatal("could not arm")
	}
	req := completionsRequest{Messages: append(history, message{Role: "user", Content: "new"})}
	if request(h, "resync", req).Code != 502 || !h.sessions["resync"].Dirty {
		t.Fatal("failed import accepted")
	}
	// The failed import chunk was never parsed as calls, and the single-use
	// arming is consumed: the retry must NOT rerun the resync machinery, but
	// it may continue as an ordinary delta (replay-phase interrupts resume),
	// carrying the tool result as context into a fresh final turn.
	w2 := request(h, "resync", req)
	if w2.Code != 200 || len(f.prompts) != 2 {
		t.Fatal("failed resync neither continued safely nor completed", w2.Code, len(f.prompts))
	}
	if len(h.sessions["resync"].ResyncMessages) != 0 || h.sessions["resync"].Dirty {
		t.Fatal("consumed resync arming resurrected or journal left dirty")
	}
	if len(h.sessions["resync"].ResyncMessages) != 0 {
		t.Fatal("consumed resync arming resurrected")
	}
}

func TestResyncAuthoritativeCompletedTranscriptWithoutPendingCalls(t *testing.T) {
	f := &fakeBackend{answers: []string{"ACK", "next reply"}}
	h := testHandler(f)
	base := []message{{Role: "system", Content: "sys prompt"}}
	h.sessions["resync"] = &session{URL: "https://chatgpt.com/c/old", Messages: base, Dirty: true}

	authoritative := []message{
		{Role: "system", Content: "sys prompt"},
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "", ToolCalls: []toolCall{{ID: "call_1", Type: "function", Function: callFunction{Name: "read", Arguments: `{"file":"a.txt"}`}}}},
		{Role: "tool", ToolCallID: "call_1", Content: "file content"},
		{Role: "assistant", Content: "I read the file."},
	}

	// Arm resync with completed transcript
	w := resyncRequest(h, authoritative)
	if w.Code != 200 {
		t.Fatalf("failed to arm resync: %d %s", w.Code, w.Body.String())
	}
	if !h.sessions["resync"].Dirty || len(h.sessions["resync"].ResyncMessages) != len(authoritative) {
		t.Fatal("resync not properly armed")
	}

	// Now send a new user request extending the authoritative transcript
	req := completionsRequest{
		Messages: append(append([]message{}, authoritative...), message{Role: "user", Content: "now what?"}),
	}
	out := assistant(t, request(h, "resync", req))
	if out.Content != "next reply" {
		t.Fatalf("unexpected answer: %+v", out)
	}
	if h.sessions["resync"].Dirty {
		t.Fatal("session should not be dirty after successful resync")
	}
}

func TestResyncRejectsUncompletedToolCallsInAuthoritativeTranscript(t *testing.T) {
	f := &fakeBackend{}
	h := testHandler(f)
	base := []message{{Role: "system", Content: "sys prompt"}}
	h.sessions["resync"] = &session{URL: "https://chatgpt.com/c/old", Messages: base, Dirty: true}

	// Transcript with dangling tool call (no tool result)
	incomplete := []message{
		{Role: "system", Content: "sys prompt"},
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "", ToolCalls: []toolCall{{ID: "call_1", Type: "function", Function: callFunction{Name: "read", Arguments: `{"file":"a.txt"}`}}}},
	}
	w := resyncRequest(h, incomplete)
	if w.Code != 409 {
		t.Fatalf("incomplete transcript should be rejected: %d %s", w.Code, w.Body.String())
	}
}
