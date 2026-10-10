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
			if w.Code == 200 || !h.sessions["recover"].Dirty || len(f.prompts) != 0 {
				t.Fatal("ambiguous recovery cleared/resubmitted", w.Code)
			}
		})
	}
}
