package main

import (
	"bytes"
	"chatgpt-proxy/internal/chatgpt"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
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
	// Switch-back arrives with completed Muse exchanges, not the original
	// retry. Recover against the ORIGINAL required schema even though the new
	// request has removed that function; import completed calls only as history.
	for _, variant := range []struct{ header, lastRole string }{
		{"X-Session-Id", "user"}, {"x-opencode-session-id", "user"},
		{"x-opencode-session-id", "tool"},
	} {
		t.Run(variant.header+"/"+variant.lastRole, func(t *testing.T) {
			f := &inspectBackend{fakeBackend: &fakeBackend{answers: []string{"continue after Muse"}}, answer: weatherAnswer, expected: "original pending browser prompt"}
			h := testHandler(f.fakeBackend)
			h.client = f
			h.statePath = filepath.Join(t.TempDir(), "state.json")
			pending := req
			if variant.lastRole == "tool" {
				// The reported timeout was a continuation after a previously
				// delivered tool call, not the later switch-back user message.
				pending.Messages = append(append([]message{}, req.Messages...),
					message{Role: "assistant", ToolCalls: []toolCall{{ID: "delivered-before-timeout", Type: "function", Function: callFunction{Name: "get_weather", Arguments: `{"city":"Paris"}`}}}},
					message{Role: "tool", ToolCallID: "delivered-before-timeout", Content: "completed tool result before canceled browser response"})
			}
			h.sessions["switch-back"] = &session{URL: "https://chatgpt.com/c/stored", Dirty: true, Phase: "final", PendingRequest: &pending, PendingPrompt: f.expected}
			parent := &session{URL: "https://chatgpt.com/c/parent", Dirty: true, Phase: "final", PendingRequest: &req, PendingPrompt: "unrelated parent pending"}
			h.sessions["root-affinity"] = parent
			// IDs derive from the original fingerprint, exactly as recovery does.
			out, err := parseAnswer(weatherAnswer, pending)
			if err != nil {
				t.Fatal(err)
			}
			fp := requestFingerprint(pending)
			sum := sha256.Sum256([]byte("switch-back:" + fp + ":0"))
			out.Calls[0].ID = fmt.Sprintf("call_%x", sum[:12])
			current := completionsRequest{ToolChoice: "auto", Tools: []toolDef{{Type: "function", Function: toolFunction{Name: "recall_fact", Parameters: map[string]any{"type": "object", "properties": map[string]any{"fact": map[string]any{"type": "string"}}, "required": []string{"fact"}}}}}}
			current.Messages = append(append([]message{}, pending.Messages...),
				message{Role: "assistant", Content: "", ToolCalls: out.Calls},
				message{Role: "tool", ToolCallID: out.Calls[0].ID, Content: "weather result already consumed by client"},
				message{Role: "user", Content: "Muse task"},
				message{Role: "assistant", ToolCalls: []toolCall{{ID: "muse-call", Type: "function", Function: callFunction{Name: "old_muse_function", Arguments: `{"fact":"MUSE-91"}`}}}},
				message{Role: "tool", ToolCallID: "muse-call", Content: "MUSE-91"},
				message{Role: "assistant", Content: "Muse completed"},
				message{Role: "user", Content: "what now?"})
			body, _ := json.Marshal(current)
			r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
			r.Header.Set(variant.header, "switch-back")
			if variant.header == "x-opencode-session-id" {
				r.Header.Set("X-Session-Id", "root-affinity")
				r.Header.Set("x-session-affinity", "root-affinity")
			}
			w := httptest.NewRecorder()
			h.routes().ServeHTTP(w, r)
			got := assistant(t, w)
			if contentText(got) != "continue after Muse" || len(got.ToolCalls) != 0 || f.reads != 1 || len(f.prompts) != 1 || f.urls[0] != "https://chatgpt.com/c/stored" {
				t.Fatal("switch-back returned original calls or failed catchup", w.Body.String())
			}
			p := f.prompts[0]
			if strings.Contains(p, renderMsg(pending.Messages[len(pending.Messages)-1])) || strings.Contains(p, out.Calls[0].ID+`","type":"function"`) || strings.Count(p, "Muse completed") != 1 || strings.Count(p, "muse-call") != 2 || !strings.Contains(p, toolPrompt(current)) || strings.Index(p, "what now?") < strings.Index(p, historyEnd) {
				t.Fatal("switch gap duplicated or schema/current request misplaced", p)
			}
			if retry := request(h, "switch-back", current); retry.Body.String() != w.Body.String() || len(f.prompts) != 1 || f.reads != 1 {
				t.Fatal("switch-back retry resubmitted")
			}
			loaded := testHandler(&fakeBackend{})
			loaded.statePath = h.statePath
			if err := loaded.load(); err != nil {
				t.Fatal(err)
			}
			if retry := request(loaded, "switch-back", current); retry.Body.String() != w.Body.String() || loaded.sessions["switch-back"].Dirty {
				t.Fatal("switch-back cache lost across restart")
			}
			if h.sessions["root-affinity"] != parent || !parent.Dirty || parent.PendingPrompt != "unrelated parent pending" || !loaded.sessions["root-affinity"].Dirty {
				t.Fatal("child adoption poisoned parent uncertainty")
			}
		})
	}
}
func TestRecoveryFailsClosedAndKeepsDirty(t *testing.T) {
	// Automatic adoption must roll back its in-memory mutation too: losing the
	// pending prompt on a failed state write makes the next read-only retry unsafe.
	t.Run("auto-adoption-save-failure", func(t *testing.T) {
		req := weatherReq()
		f := &inspectBackend{fakeBackend: &fakeBackend{}, answer: weatherAnswer, expected: "exact original pending"}
		h := testHandler(f.fakeBackend)
		h.client = f
		h.statePath = t.TempDir()
		s := &session{URL: "https://chatgpt.com/c/stored", Dirty: true, Phase: "final", PendingRequest: &req, PendingPrompt: f.expected, Fingerprint: "previous-delivered"}
		h.sessions["auto-save-failure"] = s
		for range 2 {
			w := request(h, "auto-save-failure", req)
			if w.Code != 422 || !s.Dirty || s.Phase != "final" || s.PendingRequest != &req || s.PendingPrompt != f.expected || s.Fingerprint != "previous-delivered" || len(s.Messages) != 0 || len(s.Result.Calls) != 0 || len(f.prompts) != 0 {
				t.Fatal("failed adoption lost original binding or submitted", w.Code)
			}
		}
		if f.reads != 2 {
			t.Fatal("failed save prevented a subsequent read-only recovery")
		}
	})
	// A completed alternative-model transcript cannot prove the browser turn
	// finished. Mismatch is terminal, bounded, and leaves the ORIGINAL journal.
	for _, reason := range []string{"stored user prompt does not exactly match original request", "ambiguous multiple assistant responses after stored user prompt"} {
		req := weatherReq()
		f := &inspectBackend{fakeBackend: &fakeBackend{}, expected: "original pending prompt", readErr: errors.New(reason)}
		h := testHandler(f.fakeBackend)
		h.client = f
		s := &session{Dirty: true, Phase: "final", URL: "https://chatgpt.com/c/stored", PendingRequest: &req, PendingPrompt: f.expected}
		h.sessions["switch-unknown"] = s
		current := req
		current.Tools, current.ToolChoice = nil, "auto"
		current.Messages = append(append([]message{}, req.Messages...), message{Role: "assistant", Content: "Muse completed"}, message{Role: "user", Content: "what now?"})
		w := request(h, "switch-unknown", current)
		if w.Code != 422 || f.reads != 1 || len(f.prompts) != 0 || !s.Dirty || s.PendingRequest != &req || s.PendingPrompt != f.expected || s.Phase != "final" || !strings.Contains(w.Body.String(), "/v1/sessions/resync") {
			t.Fatal("unverified switch-back cleared/replaced pending or resubmitted", w.Code, w.Body.String())
		}
	}
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
