package main

import (
	"bytes"
	"chatgpt-proxy/internal/chatgpt"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

type inspectBackend struct {
	*fakeBackend
	answer, expected string
	readErr          error
	reads            int
}

func (f *inspectBackend) ReadThreadTurn(ctx context.Context, url, prompt string) (chatgpt.TurnResult, error) {
	f.reads++
	if prompt != f.expected {
		return chatgpt.TurnResult{}, errors.New("original prompt mismatch")
	}
	return chatgpt.TurnResult{Answer: f.answer, URL: url}, f.readErr
}
func recoveryRequest(h *handler, req *completionsRequest) *httptest.ResponseRecorder {
	var b []byte
	if req != nil {
		b, _ = json.Marshal(req)
	}
	r := httptest.NewRequest("POST", "/v1/sessions/recover", bytes.NewReader(b))
	r.Header.Set("X-Session-Id", "recover")
	w := httptest.NewRecorder()
	h.routes().ServeHTTP(w, r)
	return w
}
func TestReadOnlyLegacyRecoveryCachesWithoutSubmission(t *testing.T) {
	req := weatherReq()
	req.Messages = append([]message{{Role: "system", Content: "context"}}, req.Messages...)
	f := &inspectBackend{fakeBackend: &fakeBackend{}, answer: weatherAnswer}
	h := testHandler(f.fakeBackend)
	h.client = f
	h.statePath = filepath.Join(t.TempDir(), "state.json")
	s := &session{URL: "https://chatgpt.com/c/stored", Dirty: true, Messages: req.Messages[:1], ToolsFP: toolsFingerprint(req.Tools)}
	h.sessions["recover"] = s
	f.expected = h.buildTurns(req, req.Messages[1:], toolReminder(req))[0].prompt
	if w := recoveryRequest(h, &req); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if len(f.prompts) != 0 || f.reads != 1 || h.sessions["recover"].Dirty {
		t.Fatal("recovery submitted or failed")
	}
	cached := assistant(t, request(h, "recover", req))
	if len(cached.ToolCalls) != 1 || len(f.prompts) != 0 {
		t.Fatal("retry missed validated cache")
	}
	loaded := testHandler(&fakeBackend{})
	loaded.statePath = h.statePath
	if err := loaded.load(); err != nil || loaded.sessions["recover"].Dirty {
		t.Fatal("recovery not durable", err)
	}
	// Normal completions also adopt the exact interrupted turn once and cache
	// stable call IDs; the subsequent consumer tool result is a delta, not replay.
	f2 := &inspectBackend{fakeBackend: &fakeBackend{answers: []string{"weather observed"}}, answer: weatherAnswer, expected: "exact pending prompt"}
	h2 := testHandler(f2.fakeBackend)
	h2.client = f2
	h2.sessions["auto"] = &session{URL: "https://chatgpt.com/c/stored", Dirty: true, Phase: "final", PendingRequest: &req, PendingPrompt: f2.expected}
	a := assistant(t, request(h2, "auto", req))
	b := assistant(t, request(h2, "auto", req))
	if f2.reads != 1 || len(f2.prompts) != 0 || len(a.ToolCalls) != 1 || a.ToolCalls[0].ID != b.ToolCalls[0].ID || h2.sessions["auto"].Dirty {
		t.Fatal("auto-adoption missed stable cache")
	}
	follow := req
	follow.ToolChoice = "auto"
	follow.Messages = append(append([]message{}, req.Messages...), a, message{Role: "tool", ToolCallID: a.ToolCalls[0].ID, Content: "fixture result"})
	if w := request(h2, "auto", follow); w.Code != 200 || len(f2.prompts) != 1 || f2.urls[0] != "https://chatgpt.com/c/stored" || f2.reads != 1 {
		t.Fatal("valid recovered tool loop replayed", w.Code)
	}
}
func TestRecoveryFailsClosedAndKeepsDirty(t *testing.T) {
	for _, mode := range []string{"mismatch", "invalid-tools", "invalid-answer", "unavailable", "save-failure"} {
		t.Run(mode, func(t *testing.T) {
			req := weatherReq()
			req.Messages = append([]message{{Role: "system", Content: "context"}}, req.Messages...)
			f := &inspectBackend{fakeBackend: &fakeBackend{}, answer: weatherAnswer}
			h := testHandler(f.fakeBackend)
			h.client = f
			s := &session{URL: "https://chatgpt.com/c/stored", Dirty: true, Messages: req.Messages[:1], ToolsFP: toolsFingerprint(req.Tools)}
			h.sessions["recover"] = s
			f.expected = h.buildTurns(req, req.Messages[1:], toolReminder(req))[0].prompt
			switch mode {
			case "mismatch":
				f.expected = "different"
			case "invalid-tools":
				req.Tools = nil
			case "invalid-answer":
				f.answer = "<opencode_bridge_call>{\"name\":\"get_weather\",\"arguments\":{}}</opencode_bridge_call>"
			case "unavailable":
				f.readErr = errors.New("unavailable")
			case "save-failure":
				h.statePath = t.TempDir() // cannot rename file over directory
			}
			w := recoveryRequest(h, &req)
			if mode == "invalid-answer" {
				if w.Code != 422 || s.Dirty || s.Phase != "invalid" || len(f.prompts) != 0 || len(s.Result.Calls) != 0 {
					t.Fatal("finished invalid output not safely classified", w.Code)
				}
				if retry := request(h, "recover", req); retry.Code != 422 || f.reads != 1 || len(f.prompts) != 0 {
					t.Fatal("rejected retry resubmitted or re-read", retry.Code)
				}
				return
			}
			if w.Code == 200 || !h.sessions["recover"].Dirty || len(f.prompts) != 0 {
				t.Fatal("ambiguous recovery cleared/resubmitted", w.Code)
			}
		})
	}
}
