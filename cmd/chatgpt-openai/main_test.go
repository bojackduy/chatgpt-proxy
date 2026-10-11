package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"chatgpt-proxy/internal/chatgpt"
)

type fakeBackend struct {
	prompts, urls []string
	answers       []string
	atts          []int
	thinking      []bool
	reasoning     string
	err           error
	errorURL      string
	failAt        int
	probe         int
}

func (f *fakeBackend) AskThreadTurn(ctx context.Context, opt chatgpt.TurnOptions) (chatgpt.TurnResult, error) {
	if ctx.Err() != nil {
		return chatgpt.TurnResult{}, ctx.Err()
	}
	f.prompts = append(f.prompts, opt.Prompt)
	f.urls = append(f.urls, opt.URL)
	f.atts = append(f.atts, len(opt.Attachments))
	f.thinking = append(f.thinking, opt.Thinking)
	if f.err != nil && (f.failAt == 0 || len(f.prompts) == f.failAt) {
		return chatgpt.TurnResult{URL: f.errorURL}, f.err
	}
	url := opt.URL
	if url == "" {
		url = fmt.Sprintf("https://chatgpt.com/c/thread-%d", len(f.prompts))
	}
	i := len(f.prompts) - 1
	answer := "OK"
	if strings.HasPrefix(opt.Prompt, historyIntro) {
		answer = "ACK"
	}
	if i < len(f.answers) {
		answer = f.answers[i]
	}
	return chatgpt.TurnResult{Answer: answer, URL: url, Reasoning: f.reasoning}, nil
}
func (f *fakeBackend) LoginState(context.Context) (*chatgpt.Status, error) {
	f.probe++
	return &chatgpt.Status{LoggedIn: true, Composer: true, URL: "https://chatgpt.com/c/probe"}, f.err
}
func testHandler(f *fakeBackend) *handler {
	return &handler{client: f, model: "chatgpt-web", maxChars: 512000, gate: make(chan struct{}, 1), sessions: map[string]*session{}}
}
func request(h *handler, id string, req completionsRequest) *httptest.ResponseRecorder {
	b, _ := json.Marshal(req)
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(b))
	if id != "" {
		r.Header.Set("X-Session-Id", id)
	}
	w := httptest.NewRecorder()
	h.routes().ServeHTTP(w, r)
	return w
}
func weatherReq() completionsRequest {
	return completionsRequest{Model: "chatgpt-web", Messages: []message{{Role: "user", Content: "Call weather for Hanoi"}}, Tools: []toolDef{{Type: "function", Function: toolFunction{Name: "get_weather", Parameters: map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}, "required": []string{"city"}, "additionalProperties": false}}}}, ToolChoice: map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}}}
}

const weatherAnswer = "<opencode_bridge_call>\n{\"name\":\"get_weather\",\"arguments\":{\"city\":\"Hanoi\"}}\n</opencode_bridge_call>"

func assistant(t *testing.T, w *httptest.ResponseRecorder) message {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var v struct {
		Choices []struct {
			Message      message
			FinishReason string `json:"finish_reason"`
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Choices) != 1 {
		t.Fatal(w.Body.String())
	}
	return v.Choices[0].Message
}
func TestSingleUserToolsRoundtripPersistenceRetry(t *testing.T) {
	args := `{"city": "printf '%s\\n' 'a\\\\b_*'"}`
	answer := `<opencode\_bridge\_call>{"name":"get_weather","arguments":` + args + `}</opencode\_bridge\_call>`
	f := &fakeBackend{answers: []string{answer, "Hanoi: 25 C"}}
	h := testHandler(f)
	h.statePath = filepath.Join(t.TempDir(), "state.json")
	req := weatherReq()
	a := assistant(t, request(h, "session-a", req))
	if len(a.ToolCalls) != 1 || a.ToolCalls[0].Function.Name != "get_weather" || a.ToolCalls[0].Function.Arguments != args || contentText(a) != "" {
		t.Fatal(a)
	}
	if strings.Count(f.prompts[0], "Call weather for Hanoi") != 1 {
		t.Fatal("single user duplicated")
	}
	dup := assistant(t, request(h, "session-a", req))
	if dup.ToolCalls[0].ID != a.ToolCalls[0].ID || len(f.prompts) != 1 {
		t.Fatal("retry regenerated/submitted")
	}
	// Reload disk state before continuing, as launchd restart would.
	h2 := testHandler(f)
	h2.statePath = h.statePath
	if err := h2.load(); err != nil {
		t.Fatal(err)
	}
	req.Messages = append(req.Messages, a, message{Role: "tool", ToolCallID: a.ToolCalls[0].ID, Content: "25 C"})
	req.ToolChoice = "none"
	got := assistant(t, request(h2, "session-a", req))
	if contentText(got) != "Hanoi: 25 C" {
		t.Fatal(got)
	}
	if f.urls[1] != "https://chatgpt.com/c/thread-1" || strings.Contains(f.prompts[1], "Call weather for Hanoi") || !strings.Contains(f.prompts[1], "25 C") {
		t.Fatal(f.prompts, f.urls)
	}
}
func TestSSEContract(t *testing.T) {
	args := `{"city": "printf '%s\\n' 'a\\\\b_*'"}`
	answer := `<opencode\_bridge\_call>{"name":"get_weather","arguments":` + args + `}</opencode\_bridge\_call>`
	f := &fakeBackend{answers: []string{answer}}
	h := testHandler(f)
	req := weatherReq()
	req.Stream = true
	w := request(h, "sse", req)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatal(w)
	}
	if strings.Contains(w.Body.String(), "opencode_bridge_call") || strings.Contains(w.Body.String(), "```") || strings.Contains(w.Body.String(), "~~~") {
		t.Fatal("protocol leak")
	}
	var calls, stops int
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var v struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index    *int
						ID       string
						Function callFunction
					} `json:"tool_calls"`
				}
				Finish string `json:"finish_reason"`
			}
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &v); err != nil {
			t.Fatal(err)
		}
		for _, c := range v.Choices {
			for _, call := range c.Delta.ToolCalls {
				if call.Index == nil || *call.Index != 0 || call.ID == "" || call.Function.Arguments != args {
					t.Fatal(call)
				}
				calls++
			}
			if c.Finish == "tool_calls" {
				stops++
			}
		}
	}
	if calls != 1 || stops != 1 || !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
		t.Fatal(w.Body.String())
	}
}
func TestMalformedCalls(t *testing.T) {
	for _, answer := range []string{"<opencode_bridge_call>{bad}</opencode_bridge_call>", "<opencode_bridge_call>{\"name\":\"get_weather\",\"arguments\":\"bad\"}</opencode_bridge_call>", "<opencode_bridge_call>{\"name\":\"get_weather\",\"arguments\":null}</opencode_bridge_call>", "<opencode_bridge_call>{\"name\":\"unknown\",\"arguments\":{}}</opencode_bridge_call>", "<opencode_bridge_call>{}", "plain answer", "```tool_call\n{\"name\":\"get_weather\",\"arguments\":{\"city\":\"Hanoi\"}}\n```", "```\n" + weatherAnswer + "\n```", "prose\n" + weatherAnswer} {
		t.Run(answer, func(t *testing.T) {
			req := weatherReq()
			if answer == "<opencode_bridge_call>{}" {
				answer = `<opencode\_bridge\_call>{}`
				req.ToolChoice = "auto" // must fail even without required choice
			}
			if _, err := parseAnswer(answer, req); err == nil {
				t.Fatal("accepted invalid call")
			}
		})
	}
	for _, args := range []string{`{}`, `{"city":12}`, `{"city":"Hanoi","extra":true}`, `[]`} {
		answer := "<opencode_bridge_call>\n{\"name\":\"get_weather\",\"arguments\":" + args + "}\n</opencode_bridge_call>"
		if _, err := parseAnswer(answer, weatherReq()); err == nil {
			t.Fatalf("invalid schema arguments accepted: %s", args)
		}
	}
	req := weatherReq()
	req.ToolChoice = "none"
	if _, err := parseAnswer(weatherAnswer, req); err == nil {
		t.Fatal("none honored incorrectly")
	}
	if _, err := parseAnswer(weatherAnswer, weatherReq()); err != nil {
		t.Fatal(err)
	}
	// A completely received invalid response delivers no partial calls, is
	// durable across restart, and exact retries do not submit or execute again.
	f := &fakeBackend{answers: []string{weatherAnswer + "\n<opencode_bridge_call>{", "ACK", "continued"}}
	h := testHandler(f)
	h.statePath = filepath.Join(t.TempDir(), "state.json")
	rejected := weatherReq()
	if w := request(h, "invalid", rejected); w.Code != 502 || h.sessions["invalid"].Dirty || h.sessions["invalid"].Phase != "invalid" || len(h.sessions["invalid"].Result.Calls) != 0 {
		t.Fatal("finished malformed output treated as unknown/delivered", w.Code)
	}
	loaded := testHandler(f)
	loaded.statePath = h.statePath
	if err := loaded.load(); err != nil {
		t.Fatal(err)
	}
	if w := request(loaded, "invalid", rejected); w.Code != 502 || len(f.prompts) != 1 {
		t.Fatal("invalid retry resubmitted", w.Code)
	}
	changedTools := rejected
	changedTools.Tools = nil
	changedTools.ToolChoice = "auto"
	if w := request(loaded, "invalid", changedTools); w.Code != 409 || len(f.prompts) != 1 {
		t.Fatal("schema-only retry bypassed rejected turn", w.Code)
	}
	continued := rejected
	continued.ToolChoice = "auto"
	continued.Messages = append(append([]message{}, rejected.Messages...), message{Role: "user", Content: "Continue without tools"})
	if w := request(loaded, "invalid", continued); w.Code != 200 || loaded.sessions["invalid"].Dirty || loaded.sessions["invalid"].Phase != "" {
		t.Fatal("new ordinary user turn failed", w.Code, w.Body.String())
	}
	if f.urls[1] != "" {
		t.Fatal("new turn reused rejected thread")
	}
}
func TestSessionsReadinessAndFallback(t *testing.T) {
	f := &fakeBackend{}
	h := testHandler(f)
	req := completionsRequest{Messages: []message{{Role: "user", Content: "hello"}}}
	a := assistant(t, request(h, "a", req))
	assistant(t, request(h, "b", req))
	w := httptest.NewRecorder()
	h.routes().ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
	if w.Code != 200 || f.probe != 1 || len(f.prompts) != 2 {
		t.Fatal(w)
	}
	req.Messages = append(req.Messages, a, message{Role: "user", Content: "followup"})
	assistant(t, request(h, "a", req))
	if f.urls[2] != h.sessions["a"].URL || h.sessions["a"].URL == h.sessions["b"].URL {
		t.Fatal("sessions crossed")
	}
	request(h, "", req)
	request(h, "", req)
	if f.urls[3] != "" || f.urls[4] != "" {
		t.Fatal("fallback reused a session")
	}
}
func TestErrorsAndDirtyRetry(t *testing.T) {
	f := &fakeBackend{err: errors.New("composer not found")}
	h := testHandler(f)
	req := weatherReq()
	req.Stream = true
	w := request(h, "failed", req)
	if w.Code != 502 || !strings.Contains(w.Body.String(), "proxy_error") {
		t.Fatal(w)
	}
	if request(h, "failed", req).Code != 409 || len(f.prompts) != 1 {
		t.Fatal("uncertain turn retried")
	}
	if request(h, "empty", completionsRequest{}).Code != 400 {
		t.Fatal("empty accepted")
	}
	ready := httptest.NewRecorder()
	h.routes().ServeHTTP(ready, httptest.NewRequest("GET", "/readyz", nil))
	if ready.Code != 503 {
		t.Fatal("readiness backend failure must be 503", ready)
	}
	f.err = nil
	for _, tc := range []struct {
		url     string
		err     error
		handled bool
	}{
		{"https://chatgpt.com/c/reached", context.Canceled, true},
		{"", context.Canceled, true},
		{"", errors.New("navigate: page load error net::ERR_FAILED"), false},
	} {
		f := &fakeBackend{err: tc.err, errorURL: tc.url}
		h := testHandler(f)
		s := &session{URL: "https://chatgpt.com/c/old"}
		w := httptest.NewRecorder()
		if handled := h.serveRotation(w, httptest.NewRequest("POST", "/v1/chat/completions", nil), "rotate", "main", weatherReq(), s); handled != tc.handled || len(f.prompts) != 1 {
			t.Fatal("rotation retried uncertain turn", handled)
		}
		if tc.handled && (!s.Dirty || s.Phase != "final" || s.URL != tc.url || w.Code != 502) {
			t.Fatal("rotation lost uncertain target", s, w.Code)
		}
		if !tc.handled && (s.Dirty || s.URL != "https://chatgpt.com/c/old") {
			t.Fatal("pre-submit fallback lost snapshot")
		}
	}
}

func TestNavigateFailureDoesNotWedgeFreshSession(t *testing.T) {
	// A navigation failure before any conversation URL was reached provably
	// submitted nothing: the session must accept a retry instead of 409.
	f := &fakeBackend{err: errors.New("navigate: page load error net::ERR_FAILED")}
	h := testHandler(f)
	req := weatherReq()
	if w := request(h, "nav", req); w.Code != 502 {
		t.Fatalf("expected 502, got %d: %s", w.Code, w.Body.String())
	}
	if h.sessions["nav"].Dirty {
		t.Fatal("transient pre-submission failure wedged a fresh session")
	}
	f.err = nil
	f.answers = []string{"unused", weatherAnswer}
	a := assistant(t, request(h, "nav", req))
	if len(a.ToolCalls) != 1 || len(f.prompts) != 2 {
		t.Fatal("retry did not resubmit cleanly", a, len(f.prompts))
	}
}

func TestHistoryDivergenceResetsToReplayAndInputValidation(t *testing.T) {
	f := &fakeBackend{}
	h := testHandler(f)
	req := completionsRequest{Messages: []message{{Role: "user", Content: "original"}}}
	assistant(t, request(h, "a", req))
	old := h.sessions["a"].URL
	req.Messages[0].Content = "rewritten"
	got := assistant(t, request(h, "a", req))
	if got.Role != "assistant" {
		t.Fatal("divergence did not re-answer")
	}
	if h.sessions["a"].URL == old || h.sessions["a"].URL == "" {
		t.Fatalf("divergence reused old thread: %q -> %q", old, h.sessions["a"].URL)
	}
	if last := f.prompts[len(f.prompts)-1]; !strings.Contains(last, "rewritten") || f.urls[len(f.urls)-1] != "" {
		t.Fatal("full rewritten history was not replayed into a fresh thread")
	}
	for _, choice := range []any{"invalid", 42, map[string]any{"type": "function", "function": map[string]any{"name": "unknown"}}} {
		req.ToolChoice = choice
		if request(h, "validation", req).Code != 400 {
			t.Fatalf("accepted choice %v", choice)
		}
	}
	req.ToolChoice = nil
	req.Messages[0].Content = []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/image.png"}}}
	if request(h, "image", req).Code != 400 {
		t.Fatal("accepted non-data attachment URL")
	}
	req.Messages[0].Content = []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("pngbytes"))}}}
	w := request(h, "image2", req)
	if w.Code != 200 {
		t.Fatalf("rejected data-URL image: %d %s", w.Code, w.Body.String())
	}
	if len(f.atts) == 0 || f.atts[len(f.atts)-1] != 1 {
		t.Fatalf("image not forwarded as attachment: %v", f.atts)
	}
	lastPrompt := f.prompts[len(f.prompts)-1]
	if !strings.Contains(lastPrompt, "attached to this ChatGPT turn") || strings.Contains(lastPrompt, "pngbytes") {
		t.Fatal("image data leaked into or missing from the text prompt")
	}
}

func TestReasoningAndThinkingForwarded(t *testing.T) {
	f := &fakeBackend{reasoning: "because 2+2"}
	h := testHandler(f)
	req := completionsRequest{Messages: []message{{Role: "user", Content: "why"}}, ReasoningEffort: "high"}
	w := request(h, "reason", req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"reasoning_content":"because 2+2"`) {
		t.Fatal(w.Body.String())
	}
	if len(f.thinking) != 1 || !f.thinking[0] {
		t.Fatal("reasoning_effort did not enable Thinking", f.thinking)
	}
}

func TestMultipleCallsAndTextSSE(t *testing.T) {
	req := weatherReq()
	req.ToolChoice = "auto"
	out, err := parseAnswer(weatherAnswer+"\n"+weatherAnswer, req)
	if err != nil || len(out.Calls) != 2 || out.Calls[0].ID == out.Calls[1].ID {
		t.Fatal(out, err)
	}
	f := &fakeBackend{answers: []string{"plain reply"}}
	h := testHandler(f)
	w := request(h, "text", completionsRequest{Messages: []message{{Role: "user", Content: "hello"}}, Stream: true})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"content":"plain reply"`) || !strings.Contains(w.Body.String(), `"finish_reason":"stop"`) {
		t.Fatal(w.Body.String())
	}
}

func TestSDKHistoryNormalization(t *testing.T) {
	f := &fakeBackend{answers: []string{weatherAnswer, "23 C"}}
	h := testHandler(f)
	req := weatherReq()
	a := assistant(t, request(h, "sdk", req))
	a.Content = nil // AI SDK serializes an assistant-only tool turn with null content.
	a.ToolCalls[0].Function.Arguments = "{\n  \"city\": \"Hanoi\"\n}"
	req.Messages[0].Content = []any{map[string]any{"type": "text", "text": "Call weather for Hanoi"}}
	req.Messages = append(req.Messages, a, message{Role: "tool", ToolCallID: a.ToolCalls[0].ID, Content: "23 C"})
	req.ToolChoice = "none"
	assistant(t, request(h, "sdk", req))
	if len(f.prompts) != 2 || strings.Contains(f.prompts[1], "Call weather for Hanoi") {
		t.Fatal("SDK history representations broke the delta")
	}
}
func TestConcurrentDuplicateAndCancellation(t *testing.T) {
	f := &fakeBackend{answers: []string{weatherAnswer}}
	h := testHandler(f)
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := request(h, "same", weatherReq())
			if w.Code != 200 {
				t.Errorf("status=%d", w.Code)
			}
		}()
	}
	wg.Wait()
	if len(f.prompts) != 1 {
		t.Fatal("duplicate concurrent submissions", len(f.prompts))
	}
	h.gate <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if h.acquire(ctx) {
		t.Fatal("acquired canceled wait")
	}
	h.release()
}

func TestInvalidFunctionNamesBeforeSubmission(t *testing.T) {
	for _, name := range []string{"", "name with spaces", "a.b", strings.Repeat("a", 65)} {
		f := &fakeBackend{}
		h := testHandler(f)
		req := weatherReq()
		req.Tools[0].Function.Name = name
		if request(h, "invalid-name", req).Code != 400 || len(f.prompts) != 0 {
			t.Fatalf("invalid function name %q admitted", name)
		}
	}
}

func TestOrdinaryMarkdownIsNotExecutable(t *testing.T) {
	req := weatherReq()
	req.ToolChoice = "auto"
	text := "Example only:\n```tool_call\n{\"name\":\"get_weather\",\"arguments\":{\"city\":\"Hanoi\"}}\n```"
	out, err := parseAnswer(text, req)
	if err != nil || len(out.Calls) != 0 || out.Text != text {
		t.Fatal("generic Markdown was interpreted as a bridge request", out, err)
	}
}

func TestFailedSubmittedThreadURLPersistsForRepair(t *testing.T) {
	f := &fakeBackend{err: context.Canceled, errorURL: "https://chatgpt.com/c/uncertain"}
	h := testHandler(f)
	h.statePath = filepath.Join(t.TempDir(), "sessions.json")
	if request(h, "uncertain", weatherReq()).Code != 502 {
		t.Fatal("expected backend error")
	}
	h2 := testHandler(f)
	h2.statePath = h.statePath
	if err := h2.load(); err != nil {
		t.Fatal(err)
	}
	if s := h2.sessions["uncertain"]; !s.Dirty || s.URL != f.errorURL {
		t.Fatal("repair URL or dirty journal lost", s)
	}
	if request(h2, "uncertain", weatherReq()).Code != 409 || len(f.prompts) != 1 {
		t.Fatal("uncertain submitted thread retried")
	}
}

// maxPrompt asserts every submitted turn stayed within the per-chunk budget and
// returns the prompts.
func maxPrompt(t *testing.T, f *fakeBackend, max int) {
	t.Helper()
	for i, p := range f.prompts {
		if len(p) > max {
			t.Fatalf("turn %d sent %d bytes, exceeds per-chunk budget %d", i, len(p), max)
		}
	}
}

func TestFreshLongHistoryReplaysInChunkedTurns(t *testing.T) {
	f := &fakeBackend{}
	h := testHandler(f)
	h.maxChars = 700
	var msgs []message
	for i := range 24 {
		msgs = append(msgs, message{Role: "user", Content: fmt.Sprintf("turn %02d %s", i, strings.Repeat("x", 90))})
	}
	msgs[0].Content = "EARLY-CODEWORD-ZEBRA " + strings.Repeat("y", 90)
	msgs = append(msgs, message{Role: "user", Content: "What was the early codeword? Reply only with it."})
	total := 0
	for _, m := range msgs {
		total += len(renderMsg(m))
	}
	if total <= h.maxChars {
		t.Fatalf("test history %d not larger than one chunk %d", total, h.maxChars)
	}
	w := request(h, "chunked", completionsRequest{Messages: msgs, ToolChoice: "none"})
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if len(f.prompts) < 2 {
		t.Fatalf("long fresh history not replayed in chunks: %d turn(s)", len(f.prompts))
	}
	maxPrompt(t, f, h.maxChars)
	if !strings.Contains(f.prompts[0], "EARLY-CODEWORD-ZEBRA") {
		t.Fatal("earliest message not in the first replay chunk")
	}
	last := f.prompts[len(f.prompts)-1]
	if !strings.Contains(last, "What was the early codeword") || !strings.Contains(last, "Now produce the next assistant reply") {
		t.Fatal("final turn missing the answer instruction")
	}
	// The single-giant-paste path is gone: no prompt holds both the first and
	// the final message once chunking is required.
	for i, p := range f.prompts {
		if len(f.prompts) > 1 && strings.Contains(p, "EARLY-CODEWORD-ZEBRA") && strings.Contains(p, "What was the early codeword") {
			t.Fatalf("turn %d flattened the whole history into one paste", i)
		}
	}
	// All chunks (except none) must reuse the same thread created by the first.
	for _, u := range f.urls[1:] {
		if u == "" {
			t.Fatal("replay chunk did not continue the created thread")
		}
	}
	if h.sessions["chunked"].URL != f.urls[len(f.urls)-1] || h.sessions["chunked"].URL == "" {
		t.Fatal("final thread URL not persisted")
	}
}

func TestOversizedSingleMessageSplitToChunkBudget(t *testing.T) {
	f := &fakeBackend{}
	h := testHandler(f)
	h.maxChars = 1000
	msgs := []message{
		{Role: "user", Content: strings.Repeat("Z", 8000)},
		{Role: "user", Content: "final question"},
	}
	w := request(h, "oversized", completionsRequest{Messages: msgs, ToolChoice: "none"})
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if len(f.prompts) < 2 {
		t.Fatal("oversized single message not split")
	}
	maxPrompt(t, f, h.maxChars)
}

func TestShortFreshHistoryIsOneTurn(t *testing.T) {
	f := &fakeBackend{}
	h := testHandler(f)
	w := request(h, "short", completionsRequest{Messages: []message{{Role: "user", Content: "hello there"}}, ToolChoice: "none"})
	if w.Code != 200 || len(f.prompts) != 1 {
		t.Fatalf("short history should be one turn, got %d prompts status %d", len(f.prompts), w.Code)
	}
	maxPrompt(t, f, h.maxChars)
}

func TestDeltaContinuationSendsOnlyTheDelta(t *testing.T) {
	f := &fakeBackend{}
	h := testHandler(f)
	req := completionsRequest{Messages: []message{{Role: "user", Content: "first question"}}, ToolChoice: "none"}
	a := assistant(t, request(h, "delta", req))
	req.Messages = append(req.Messages, a, message{Role: "user", Content: "second question"})
	assistant(t, request(h, "delta", req))
	if len(f.prompts) != 2 {
		t.Fatalf("delta continuation should add exactly one turn, got %d", len(f.prompts))
	}
	if strings.Contains(f.prompts[1], "first question") || !strings.Contains(f.prompts[1], "second question") {
		t.Fatal("continuation replayed already-known history instead of the delta")
	}
}

func TestPackPiecesSplitAtRuneBoundary(t *testing.T) {
	// A limit sitting mid-rune must not corrupt the transcript.
	pieces := []piece{{text: strings.Repeat("é", 100)}} // 200 bytes, 2 bytes/runes
	groups := packPieces(pieces, 51)
	for _, g := range groups {
		for _, p := range g {
			if !utf8.ValidString(p.text) {
				t.Fatalf("split produced invalid UTF-8: %q", p.text)
			}
		}
	}
}

func TestProviderSwitchImportsOnlyCompletedGap(t *testing.T) {
	f := &fakeBackend{answers: []string{"own-last-reply", "gap recalled"}}
	h := testHandler(f)
	req := completionsRequest{Messages: []message{{Role: "user", Content: "already-known"}}}
	a := assistant(t, request(h, "switch", req))
	url := h.sessions["switch"].URL
	req.Messages = append(req.Messages, a,
		message{Role: "user", Content: "Other provider question"},
		message{Role: "assistant", ToolCalls: []toolCall{{ID: "historical-call", Type: "function", Function: callFunction{Name: "old_function", Arguments: `{"fact":"GAP-ORCHID-91"}`}}}},
		message{Role: "tool", ToolCallID: "historical-call", Content: "GAP-ORCHID-91"},
		message{Role: "assistant", Content: "Other provider completed answer"},
		message{Role: "user", Content: "Recall gap fact"})
	req.Tools = weatherReq().Tools
	req.ToolChoice = "auto"
	w := request(h, "switch", req)
	got := assistant(t, w)
	if contentText(got) != "gap recalled" || len(got.ToolCalls) != 0 || f.urls[1] != url {
		t.Fatal(got, f.urls)
	}
	p := f.prompts[1]
	for _, s := range []string{historyIntro, historyEnd, "CURRENT REQUEST", "historical-call", "Other provider completed answer", "get_weather"} {
		if !strings.Contains(p, s) {
			t.Fatalf("missing %q: %s", s, p)
		}
	}
	if strings.Contains(p, "already-known") || strings.Contains(p, "own-last-reply") {
		t.Fatal("repeated cached prefix", p)
	}
	if strings.Index(p, "Recall gap fact") < strings.Index(p, historyEnd) {
		t.Fatal("current question in historical section")
	}
	dup := request(h, "switch", req)
	if dup.Body.String() != w.Body.String() || len(f.prompts) != 2 {
		t.Fatal("retry changed completion or resubmitted")
	}
}

func TestLargeCatchupAckAndDirtyResume(t *testing.T) {
	for _, badAck := range []bool{false, true} {
		t.Run(fmt.Sprint(badAck), func(t *testing.T) {
			f := &fakeBackend{}
			h := testHandler(f)
			h.maxChars = 1100
			h.statePath = filepath.Join(t.TempDir(), "state.json")
			req := completionsRequest{Messages: []message{{Role: "user", Content: "known"}}}
			a := assistant(t, request(h, "large", req))
			url := h.sessions["large"].URL
			req.Messages = append(req.Messages, a)
			for i := range 8 {
				req.Messages = append(req.Messages, message{Role: "user", Content: fmt.Sprintf("gap question %d", i)}, message{Role: "assistant", Content: strings.Repeat("completed fact ", 30)})
			}
			req.Messages = append(req.Messages, message{Role: "user", Content: "current question"})
			if badAck {
				f.answers = []string{"OK", "ACK", weatherAnswer}
			}
			w := request(h, "large", req)
			if badAck {
				if w.Code != 502 || strings.Contains(w.Body.String(), `"tool_calls"`) {
					t.Fatal(w.Body.String())
				}
				h2 := testHandler(f)
				h2.statePath = h.statePath
				if err := h2.load(); err != nil {
					t.Fatal(err)
				}
				s := h2.sessions["large"]
				if !s.Dirty || s.URL != url || len(s.Messages) <= 2 || len(s.Messages) >= len(req.Messages) {
					t.Fatal("ack prefix/dirty persistence", s)
				}
				n := len(f.prompts)
				// Replay-phase interrupts resume instead of wedging: the failed
				// chunk's answer was never parsed as calls, so resubmission
				// cannot execute twice. Remaining fake answers are benign, so the
				// resumed import must complete cleanly with no tool_calls leak.
				w2 := request(h2, "large", req)
				if w2.Code != 200 || len(f.prompts) == n {
					t.Fatal("interrupted replay did not resume", w2.Code, w2.Body.String())
				}
				if strings.Contains(w2.Body.String(), `"tool_calls"`) || h2.sessions["large"].Dirty {
					t.Fatal("resumed replay leaked calls or stayed dirty")
				}
				return
			}
			assistant(t, w)
			maxPrompt(t, f, h.maxChars)
			if len(f.prompts) < 4 {
				t.Fatal("gap not chunked")
			}
			for i := 1; i < len(f.prompts)-1; i++ {
				if !strings.HasPrefix(f.prompts[i], historyIntro) || strings.Contains(f.prompts[i], "current question") || f.urls[i] != url {
					t.Fatal(f.prompts[i], f.urls)
				}
			}
			last := f.prompts[len(f.prompts)-1]
			if strings.Contains(last, "completed fact") || !strings.Contains(last, "CURRENT REQUEST") {
				t.Fatal(last)
			}
		})
	}
}

func TestExactPrefixRetainsMultimodalAndNameIdentity(t *testing.T) {
	oldCall := []message{{Role: "assistant", ToolCalls: []toolCall{{ID: "large-id", Function: callFunction{Name: "lookup", Arguments: `{"id":9007199254740992}`}}}}}
	editedCall := []message{{Role: "assistant", ToolCalls: []toolCall{{ID: "large-id", Function: callFunction{Name: "lookup", Arguments: `{"id":9007199254740993}`}}}}}
	if prefix(oldCall, editedCall) {
		t.Fatal("large integer edit matched cached prefix")
	}
	image := func(data string) any {
		return []any{map[string]any{"type": "text", "text": "same text"}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte(data))}}}
	}
	a := []message{{Role: "user", Content: image("original")}}
	b := []message{{Role: "user", Content: image("edited")}}
	if prefix(a, b) {
		t.Fatal("image edit matched old prefix")
	}
	b = []message{{Role: "user", Content: image("original"), Name: "changed"}}
	if prefix(a, b) {
		t.Fatal("name edit matched old prefix")
	}
	f := &fakeBackend{}
	h := testHandler(f)
	req := completionsRequest{Messages: a}
	reply := assistant(t, request(h, "media", req))
	url := h.sessions["media"].URL
	req.Messages = append(req.Messages, reply, message{Role: "user", Content: image("gap")}, message{Role: "assistant", Content: "media answer"}, message{Role: "user", Content: "latest"})
	assistant(t, request(h, "media", req))
	if f.urls[1] != url || f.atts[1] != 1 || strings.Count(f.prompts[1], "attached to this ChatGPT turn") != 1 {
		t.Fatal("missing gap attachment or replayed old image", f.atts)
	}
	req.Messages[0].Content = image("edited")
	assistant(t, request(h, "media", req))
	if f.urls[2] != "" || h.sessions["media"].URL == url {
		t.Fatal("edited image reused thread")
	}
}

func TestReasoningFalseDoesNotSelectThinking(t *testing.T) {
	f := &fakeBackend{}
	h := testHandler(f)
	assistant(t, request(h, "no-thinking", completionsRequest{Messages: []message{{Role: "user", Content: "hello"}}, Reasoning: false}))
	if f.thinking[0] {
		t.Fatal("reasoning:false selected Thinking")
	}
}

func TestCatchupCurrentToolsAndImportFailure(t *testing.T) {
	for _, mode := range []string{"valid-final", "wrapper-ack", "interrupted"} {
		t.Run(mode, func(t *testing.T) {
			f := &fakeBackend{}
			h := testHandler(f)
			h.maxChars = 2400
			h.statePath = filepath.Join(t.TempDir(), "sessions.json")
			req := completionsRequest{Messages: []message{{Role: "user", Content: "known"}}}
			a := assistant(t, request(h, "tools-gap", req))
			url := h.sessions["tools-gap"].URL
			req.Messages = append(req.Messages, a)
			for range 6 {
				req.Messages = append(req.Messages, message{Role: "user", Content: "gap question"}, message{Role: "assistant", Content: strings.Repeat("gap answer ", 55)})
			}
			req.Messages = append(req.Messages, message{Role: "user", Content: "Call current get_weather for Hanoi"})
			req.Tools, req.ToolChoice = weatherReq().Tools, "required"
			turns := h.buildTurns(req, req.Messages[2:], toolPrompt(req))
			if len(turns) < 3 {
				t.Fatal("test gap too small")
			}
			f.answers = []string{"OK"}
			for range len(turns) - 1 {
				f.answers = append(f.answers, "ACK")
			}
			f.answers = append(f.answers, weatherAnswer)
			if mode == "wrapper-ack" {
				f.answers[2] = weatherAnswer
			}
			if mode == "interrupted" {
				f.err, f.failAt, f.errorURL = context.Canceled, 3, url
			}
			w := request(h, "tools-gap", req)
			if mode == "valid-final" {
				got := assistant(t, w)
				if len(got.ToolCalls) != 1 || got.ToolCalls[0].Function.Name != "get_weather" {
					t.Fatal(got)
				}
				for _, p := range f.prompts[1 : len(f.prompts)-1] {
					if strings.Contains(p, "External client functions:") {
						t.Fatal("schema in history-only chunk")
					}
				}
				if !strings.Contains(f.prompts[len(f.prompts)-1], `"required":["city"]`) {
					t.Fatal("missing current schema")
				}
				maxPrompt(t, f, h.maxChars)
				return
			}
			if w.Code != 502 || strings.Contains(w.Body.String(), `"tool_calls"`) {
				t.Fatal(w.Body.String())
			}
			h2 := testHandler(f)
			h2.statePath = h.statePath
			if err := h2.load(); err != nil {
				t.Fatal(err)
			}
			s := h2.sessions["tools-gap"]
			if !s.Dirty || s.URL != url || len(s.Messages) <= 2 || len(s.Messages) >= len(req.Messages) {
				t.Fatal("lost acknowledged-prefix/dirty journal", s)
			}
			n := len(f.prompts)
			// Replay-phase interrupts resume instead of wedging: the failed
			// chunk's output was never parsed as calls, so resubmission cannot
			// execute twice. The final fake answer is plain "OK" against a
			// required tool_choice, so the resumed request must run the full turn
			// and fail closed on protocol (502) — proving it proceeded safely
			// instead of wedging, without leaking unexecuted calls.
			w2 := request(h2, "tools-gap", req)
			if w2.Code != 502 || len(f.prompts) == n {
				t.Fatal("interrupted import neither wedged nor safely repeated", w2.Code, len(f.prompts))
			}
			if strings.Contains(w2.Body.String(), `"tool_calls"`) {
				t.Fatal("unexecuted call leaked", w2.Body.String())
			}
			if h2.sessions["tools-gap"].Dirty || h2.sessions["tools-gap"].Phase != "invalid" {
				t.Fatal("finished rejected final not durably classified")
			}
			n = len(f.prompts)
			if retry := request(h2, "tools-gap", req); retry.Code != 502 || len(f.prompts) != n {
				t.Fatal("rejected final retry resubmitted", retry.Code)
			}
		})
	}
}

func TestCompactionCreatesNewThreadAndVideoGap(t *testing.T) {
	f := &fakeBackend{}
	h := testHandler(f)
	req := completionsRequest{Messages: []message{{Role: "user", Content: "original"}}}
	a := assistant(t, request(h, "video", req))
	url := h.sessions["video"].URL
	req.Messages = append(req.Messages, a, message{Role: "user", Content: []any{map[string]any{"type": "video_url", "video_url": map[string]any{"url": "data:video/mp4;base64," + base64.StdEncoding.EncodeToString([]byte("clip"))}}}}, message{Role: "assistant", Content: "video observed"}, message{Role: "user", Content: "recall video"})
	assistant(t, request(h, "video", req))
	if f.atts[1] != 1 || f.urls[1] != url {
		t.Fatal("video gap not forwarded", f.atts, f.urls)
	}
	req.Messages = []message{{Role: "system", Content: "Compacted summary: video observed"}, {Role: "user", Content: "recall video"}}
	assistant(t, request(h, "video", req))
	if f.urls[2] != "" || h.sessions["video"].URL == url {
		t.Fatal("compaction reused thread")
	}
}

func TestTooSmallChunkBudgetDoesNotSubmit(t *testing.T) {
	f := &fakeBackend{}
	h := testHandler(f)
	h.maxChars = 100
	if w := request(h, "budget", weatherReq()); w.Code != 400 || len(f.prompts) != 0 || h.sessions["budget"] != nil {
		t.Fatal("oversized schema submitted or dirtied", w.Body.String())
	}
}

func fatTools(n int, pad int) []toolDef {
	out := make([]toolDef, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, toolDef{Type: "function", Function: toolFunction{
			Name:        fmt.Sprintf("tool_%02d", i),
			Description: "does something useful " + strings.Repeat("x", pad),
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{"arg": map[string]any{"type": "string", "description": strings.Repeat("y", pad)}}},
		}})
	}
	return out
}

func TestToolSchemasTaughtOncePerThread(t *testing.T) {
	f := &fakeBackend{}
	h := testHandler(f)
	h.maxChars = 300000
	tools := fatTools(20, 2000) // full dump ~90KB: fits once, must not repeat
	schemaBytes := toolsBytes(tools)
	if schemaBytes < 80000 {
		t.Fatalf("fat toolset too small to prove anything: %d", schemaBytes)
	}
	req := completionsRequest{Messages: []message{{Role: "user", Content: "hi there"}}, Tools: tools}
	a1 := assistant(t, request(h, "teach", req))
	if !strings.Contains(f.prompts[0], "External client functions:") {
		t.Fatal("first turn did not teach full schemas")
	}
	url := h.sessions["teach"].URL
	if h.sessions["teach"].ToolsFP == "" {
		t.Fatal("taught fingerprint not persisted")
	}
	// Continuation with identical tools: same thread, compact reminder only.
	req.Messages = append(req.Messages, a1, message{Role: "user", Content: "and now?"})
	a2 := assistant(t, request(h, "teach", req))
	if f.urls[1] != url {
		t.Fatal("continuation left the thread")
	}
	if strings.Contains(f.prompts[1], "External client functions:") {
		t.Fatal("continuation re-pasted full schemas")
	}
	if !strings.Contains(f.prompts[1], "already taught") {
		t.Fatal("continuation missing compact reminder")
	}
	if len(f.prompts[1]) > schemaBytes/4 {
		t.Fatalf("reminder turn too large: %d", len(f.prompts[1]))
	}
	// Changed tool set: re-teach in full on the same thread.
	req.Tools = append(append([]toolDef{}, tools...), toolDef{Type: "function", Function: toolFunction{Name: "brand_new", Parameters: map[string]any{"type": "object"}}})
	req.Messages = append(req.Messages, a2, message{Role: "user", Content: "use it"})
	assistant(t, request(h, "teach", req))
	last := f.prompts[len(f.prompts)-1]
	if f.urls[len(f.urls)-1] != url {
		t.Fatal("tool change left the thread")
	}
	if !strings.Contains(last, "External client functions:") || !strings.Contains(last, "brand_new") {
		t.Fatal("changed toolset not re-taught")
	}
}
