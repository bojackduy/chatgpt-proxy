package main

import (
	"bytes"
	"chatgpt-proxy/internal/chatgpt"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type inspectBackend struct {
	*fakeBackend
	answer, expected string
	images           []chatgpt.RasterImage
	readErr          error
	reads            int
}

func (f *inspectBackend) ReadThreadTurn(ctx context.Context, url, prompt string) (chatgpt.TurnResult, error) {
	f.reads++
	if prompt != f.expected {
		return chatgpt.TurnResult{}, errors.New("original prompt mismatch")
	}
	return chatgpt.TurnResult{Answer: f.answer, Images: f.images, URL: url}, f.readErr
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
	// A supplied request is not proof of an absent durable source binding.
	for _, broken := range []*session{
		{Dirty: true},
		{Dirty: true, Phase: "final", PendingPrompt: "orphan"},
		{Dirty: true, Phase: "final", PendingRequest: &req},
		{Dirty: true, Phase: "final", PendingRequest: &req, PendingPrompt: "repair", RepairPendingPrompt: "repair", RepairFingerprint: "wrong"},
	} {
		broken.URL = "https://chatgpt.com/c/stored"
		broken.Messages = req.Messages[:1]
		h.sessions["recover"] = broken
		before, _ := json.Marshal(broken)
		if err := h.save(); err != nil {
			t.Fatal(err)
		}
		if err := h.load(); err != nil {
			t.Fatal(err)
		}
		for _, supplied := range []*completionsRequest{nil, &req} {
			w := recoveryRequest(h, supplied)
			if w.Code != 422 || !strings.Contains(w.Body.String(), `"code":"incomplete_journal"`) || !strings.Contains(w.Body.String(), "/v1/sessions/resync") {
				t.Fatal(w.Code, w.Body.String())
			}
		}
		if w := request(h, "recover", req); w.Code != 422 || !strings.Contains(w.Body.String(), `"code":"incomplete_journal"`) {
			t.Fatal(w.Code, w.Body.String())
		}
		after, _ := json.Marshal(h.sessions["recover"])
		if !bytes.Equal(before, after) || f.reads != 0 || len(f.prompts) != 0 {
			t.Fatal("incomplete record changed or browser accessed")
		}
	}
	f.expected = h.buildTurns(req, req.Messages[1:], toolReminder(req))[0].prompt
	s := &session{URL: "https://chatgpt.com/c/stored", Dirty: true, Phase: "final", PendingRequest: &req, PendingPrompt: f.expected, Messages: req.Messages[:1], ToolsFP: toolsFingerprint(req.Tools)}
	h.sessions["recover"] = s
	f.expected = h.buildTurns(req, req.Messages[1:], toolReminder(req))[0].prompt
	if w := recoveryRequest(h, &req); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if len(f.prompts) != 0 || f.reads != 1 || h.sessions["recover"].Dirty {
		t.Fatal("recovery submitted or failed")
	}
	// A lost URL may be supplied explicitly, but is not evidence by itself:
	// exact client fingerprint and full source/prompt verification still apply.
	for _, valid := range []bool{false, true} {
		h.sessions["recover"] = &session{Dirty: true, Phase: "final", PendingRequest: &req, PendingPrompt: f.expected}
		if !valid {
			f.readErr = errors.New("stored user prompt does not exactly match original request")
		} else {
			f.readErr = nil
		}
		payload := struct {
			completionsRequest
			ThreadURL string `json:"thread_url"`
		}{req, "https://chatgpt.com/c/stored"}
		body, _ := json.Marshal(payload)
		r := httptest.NewRequest("POST", "/v1/sessions/recover", bytes.NewReader(body))
		r.Header.Set("X-Session-Id", "recover")
		w := httptest.NewRecorder()
		h.routes().ServeHTTP(w, r)
		if valid && (w.Code != 200 || h.sessions["recover"].Dirty || h.sessions["recover"].URL != payload.ThreadURL) {
			t.Fatal("verified lost URL did not recover", w.Code)
		}
		if !valid && (w.Code != 409 || !h.sessions["recover"].Dirty || h.sessions["recover"].URL != "") {
			t.Fatal("candidate URL adopted without full prompt proof", w.Code)
		}
		if len(f.prompts) != 0 {
			t.Fatal("lost URL recovery submitted a new turn")
		}
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
func TestRecoveredTurnCarriesRasterImages(t *testing.T) {
	// Adopted/recovered turns must serve the same raster image parts as a live
	// turn: tryAdoptInterrupted once dropped res.Images, so an interrupted
	// diagram turn came back as the "[SVG: ...]" text fallback only.
	const png1x1 = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg=="
	img := chatgpt.RasterImage{Alt: "diagram", DataURL: png1x1}
	req := weatherReq()
	f := &inspectBackend{fakeBackend: &fakeBackend{}, answer: weatherAnswer, images: []chatgpt.RasterImage{img}}
	h := testHandler(f.fakeBackend)
	h.client = f
	h.statePath = filepath.Join(t.TempDir(), "state.json")
	f.expected = h.buildTurns(req, req.Messages[1:], toolReminder(req))[0].prompt
	h.sessions["recover"] = &session{URL: "https://chatgpt.com/c/stored", Dirty: true, Phase: "final", PendingRequest: &req, PendingPrompt: f.expected}
	w := recoveryRequest(h, &req)
	if w.Code != 200 {
		t.Fatalf("adoption failed: %d %s", w.Code, w.Body.String())
	}
	// The recover endpoint only reports the cache; the images reach the wire on
	// the exact-retry completion that serves the validated cache.
	got := assistant(t, request(h, "recover", req))
	if !strings.Contains(contentText(got), "data:image/png;base64,") {
		t.Fatalf("adopted turn dropped raster images: %+v", got)
	}
	// Durable across restart: the recovered result is served from the cache
	// without another browser read, still carrying the image.
	loaded := testHandler(&fakeBackend{})
	loaded.statePath = h.statePath
	if err := loaded.load(); err != nil {
		t.Fatal(err)
	}
	if retry := request(loaded, "recover", req); f.reads != 1 || !strings.Contains(retry.Body.String(), "data:image/png;base64,") {
		t.Fatal("cached recovered result lost its image")
	}
}

func TestSentLogDiagnosesPendingOverwrite(t *testing.T) {
	// End-to-end through completions: the submitted H1 is logged, then a
	// newer retry H3 overwrites pending. Diagnosis must name the desync.
	f := &fakeBackend{answers: []string{weatherAnswer}}
	h := testHandler(f)
	h.statePath = filepath.Join(t.TempDir(), "state.json")
	req := weatherReq()
	if w := request(h, "diag", req); w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	s := h.sessions["diag"]
	if len(h.sentLog) < 2 {
		t.Fatalf("expected submitted+completed records, got %d", len(h.sentLog))
	}
	s.Dirty, s.Phase = true, "final"
	s.PendingRequest = &req
	s.PendingPrompt = "newer retry prompt H3 that the thread never received"
	if d := h.sentDiagnosis("diag", s); !strings.Contains(d, "MISMATCH") || !strings.Contains(d, "pending-overwritten-by-retry") {
		t.Fatalf("overwrite not diagnosed: %s", d)
	}
	// Consistent case: pending is exactly the submitted prompt.
	s.PendingPrompt = f.prompts[0]
	if d := h.sentDiagnosis("diag", s); !strings.Contains(d, "consistent") {
		t.Fatalf("consistent state misdiagnosed: %s", d)
	}
	// Durability: a fresh handler with the same state path recovers the
	// diagnosis from the file tail with an empty memory ring.
	s.PendingPrompt = "newer retry prompt H3 that the thread never received"
	h2 := testHandler(&fakeBackend{})
	h2.statePath = h.statePath
	if d := h2.sentDiagnosis("diag", s); !strings.Contains(d, "MISMATCH") {
		t.Fatalf("file-tail diagnosis missed: %s", d)
	}
	if d := h2.sentDiagnosis("unknown-session", s); !strings.Contains(d, "no submission recorded") {
		t.Fatalf("unknown session misdiagnosed: %s", d)
	}
}

// resyncBackend is a fake that fails exact adoption structurally while the
// thread holds a transplantable latest turn (or a configured latest error).
type resyncBackend struct {
	*fakeBackend
	mismatch       *chatgpt.UserBubbleMismatchError
	latestUser     string
	latestAnswer   string
	latestImages   []chatgpt.RasterImage
	latestErr      error
	reads, latests int
}

func (f *resyncBackend) ReadThreadTurn(ctx context.Context, url, prompt string) (chatgpt.TurnResult, error) {
	f.reads++
	if f.mismatch != nil {
		return chatgpt.TurnResult{}, f.mismatch
	}
	return chatgpt.TurnResult{}, errors.New("original prompt mismatch")
}

func (f *resyncBackend) ReadLatestThreadTurn(ctx context.Context, url string) (string, chatgpt.TurnResult, error) {
	f.latests++
	if f.latestErr != nil {
		return "", chatgpt.TurnResult{}, f.latestErr
	}
	return f.latestUser, chatgpt.TurnResult{Answer: f.latestAnswer, Images: f.latestImages, URL: url}, nil
}

func structuralMismatch() *chatgpt.UserBubbleMismatchError {
	return &chatgpt.UserBubbleMismatchError{Path: "mapping", Rendered: 97805, Want: 102400, DiffOff: 100, Spans: "100-5000/200-5100,9000+rest/9100+rest"}
}

func TestSpansStructural(t *testing.T) {
	for _, tc := range []struct {
		spans string
		want  bool
	}{
		{"", false},
		{"10-40/10-35", false},        // one small span: bounded transform class
		{"0-100/0-90", false},         // one span under the multi-KB bar
		{"0-3000/0-2900", true},       // multi-KB interior region
		{"1-2/3-4,50-60/70-80", true}, // scattered spans: rebuilt request
		{"100+rest/200+rest", true},   // never re-converged
		{"10-40/10-35,9000+rest/9100+rest", true},
		{"garbage", false}, // unparseable fails closed
	} {
		if got := spansStructural(tc.spans); got != tc.want {
			t.Errorf("spansStructural(%q) = %v, want %v", tc.spans, got, tc.want)
		}
	}
}

func TestAutoResyncRecognizedShape(t *testing.T) {
	// ses_f1c7ba8b shape, resolved the ses_ee37 way but programmatically:
	// thread holds the completed H1 submission, pending is the newer H3.
	const h1 = "first submission prompt H1 with full context"
	req := weatherReq()
	f := &resyncBackend{fakeBackend: &fakeBackend{}, mismatch: structuralMismatch(), latestUser: h1, latestAnswer: weatherAnswer}
	h := testHandler(f.fakeBackend)
	h.client = f
	h.statePath = filepath.Join(t.TempDir(), "state.json")
	h.sessions["auto"] = &session{URL: "https://chatgpt.com/c/stored", Dirty: true, Phase: "final", PendingRequest: &req, PendingPrompt: "newer retry prompt H3", Messages: req.Messages[:1]}
	h.appendSent(sentEntry{Event: "completed", Session: "auto", URL: "https://chatgpt.com/c/stored", PromptHash: promptHash(h1), Bytes: len(h1)})
	w := request(h, "auto", req)
	got := assistant(t, w)
	if len(got.ToolCalls) != 1 || f.latests != 1 || len(f.prompts) != 0 {
		t.Fatalf("auto-resync did not transplant read-only: calls=%d latests=%d submits=%d", len(got.ToolCalls), f.latests, len(f.prompts))
	}
	s := h.sessions["auto"]
	if s.Dirty || s.Phase != "" || s.PendingRequest != nil || s.PendingPrompt != "" {
		t.Fatal("transplant left uncertainty behind")
	}
	// Stable IDs match adoption derivation exactly.
	fp := requestFingerprint(req)
	sum := sha256.Sum256([]byte("auto:" + fp + ":0"))
	if want := fmt.Sprintf("call_%x", sum[:12]); got.ToolCalls[0].ID != want {
		t.Fatalf("transplanted call ID %s != adoption-derived %s", got.ToolCalls[0].ID, want)
	}
	// Journal evidence: what was transplanted and why the shape was safe.
	for _, want := range []string{"executed-by-resync", "pending-overwritten-by-retry", got.ToolCalls[0].ID, "structural spans"} {
		if !strings.Contains(s.AutoResyncEvidence, want) {
			t.Fatalf("evidence lacks %q: %s", want, s.AutoResyncEvidence)
		}
	}
	// Exact retry hits the duplicate cache with no further reads or submits.
	if retry := request(h, "auto", req); retry.Body.String() != w.Body.String() || f.latests != 1 || len(f.prompts) != 0 {
		t.Fatal("retry after auto-resync resubmitted or re-read")
	}
	// Durable across restart.
	loaded := testHandler(&fakeBackend{})
	loaded.statePath = h.statePath
	if err := loaded.load(); err != nil || loaded.sessions["auto"].Dirty || loaded.sessions["auto"].AutoResyncEvidence == "" {
		t.Fatal("transplant not restart-safe", err)
	}
}

func TestAutoResyncTransplantsRasterImages(t *testing.T) {
	// The auto-resync transplant must carry rasters the same way adoption does.
	const png1x1 = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg=="
	const h1 = "first submission prompt H1 with full context"
	req := weatherReq()
	f := &resyncBackend{fakeBackend: &fakeBackend{}, mismatch: structuralMismatch(), latestUser: h1, latestAnswer: weatherAnswer,
		latestImages: []chatgpt.RasterImage{{Alt: "diagram", DataURL: png1x1}}}
	h := testHandler(f.fakeBackend)
	h.client = f
	h.statePath = filepath.Join(t.TempDir(), "state.json")
	h.sessions["auto"] = &session{URL: "https://chatgpt.com/c/stored", Dirty: true, Phase: "final", PendingRequest: &req, PendingPrompt: "newer retry prompt H3", Messages: req.Messages[:1]}
	h.appendSent(sentEntry{Event: "completed", Session: "auto", URL: "https://chatgpt.com/c/stored", PromptHash: promptHash(h1), Bytes: len(h1)})
	w := request(h, "auto", req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "data:image/png;base64,") {
		t.Fatalf("auto-resync transplant dropped raster images: %d %s", w.Code, w.Body.String())
	}
}

func TestAutoResyncStays409WhenAmbiguous(t *testing.T) {
	const h1 = "first submission prompt H1 with full context"
	req := weatherReq()
	setup := func(t *testing.T, spans string, latestErr error, logSubmission bool) (*handler, *resyncBackend) {
		f := &resyncBackend{fakeBackend: &fakeBackend{}, latestUser: h1, latestAnswer: weatherAnswer, latestErr: latestErr}
		if spans != "" {
			f.mismatch = &chatgpt.UserBubbleMismatchError{Path: "mapping", Rendered: 100, Want: 120, DiffOff: 10, Spans: spans}
		} else {
			f.mismatch = structuralMismatch()
		}
		h := testHandler(f.fakeBackend)
		h.client = f
		h.statePath = filepath.Join(t.TempDir(), "state.json")
		h.sessions["recover"] = &session{URL: "https://chatgpt.com/c/stored", Dirty: true, Phase: "final", PendingRequest: &req, PendingPrompt: "newer retry prompt H3", Messages: req.Messages[:1]}
		if logSubmission {
			h.appendSent(sentEntry{Event: "completed", Session: "recover", URL: "https://chatgpt.com/c/stored", PromptHash: promptHash(h1), Bytes: len(h1)})
		}
		return h, f
	}
	for _, tc := range []struct {
		name           string
		spans          string
		latestErr      error
		logSubmission  bool
		wantDiagSubstr string
	}{
		{"no sent-log submission", "", nil, false, "no submission recorded"},
		{"no complete thread answer", "", errors.New("stored thread holds no complete assistant answer to transplant"), true, "MISMATCH"},
		{"bounded transform spans", "10-40/10-35", nil, true, "MISMATCH"},
		{"thread turn is not ours", "", nil, true, "MISMATCH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, f := setup(t, tc.spans, tc.latestErr, tc.logSubmission)
			if tc.name == "thread turn is not ours" {
				f.latestUser = "some other session's prompt"
			}
			w := recoveryRequest(h, nil)
			if w.Code != 409 || !strings.Contains(w.Body.String(), "sent-log:") || !strings.Contains(w.Body.String(), tc.wantDiagSubstr) {
				t.Fatalf("expected ambiguous 409 with diagnosis, got %d %s", w.Code, w.Body.String())
			}
			s := h.sessions["recover"]
			if !s.Dirty || s.Phase != "final" || s.AutoResyncEvidence != "" || len(f.prompts) != 0 {
				t.Fatal("ambiguous 409 changed uncertainty journal or submitted")
			}
		})
	}
}

func TestRecover409CarriesSentLogDiagnosis(t *testing.T) {
	// A structural adoption failure (thread holds earlier H1, pending is H3)
	// must return 409 with the sent-log diagnosis line attached.
	req := weatherReq()
	f := &inspectBackend{fakeBackend: &fakeBackend{}, expected: "original pending", readErr: errors.New("stored user prompt does not exactly match original request (rendered 10B want 20B)")}
	h := testHandler(f.fakeBackend)
	h.client = f
	h.statePath = filepath.Join(t.TempDir(), "state.json")
	h.sessions["recover"] = &session{URL: "https://chatgpt.com/c/stored", Dirty: true, Phase: "final", PendingRequest: &req, PendingPrompt: "newer retry prompt H3", Messages: req.Messages[:1]}
	h.appendSent(sentEntry{Event: "completed", Session: "recover", URL: "https://chatgpt.com/c/stored", PromptHash: promptHash("original submitted prompt H1"), Bytes: 100})
	w := recoveryRequest(h, nil)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "sent-log: MISMATCH") {
		t.Fatalf("409 lacks diagnosis: %d %s", w.Code, w.Body.String())
	}
	if !h.sessions["recover"].Dirty {
		t.Fatal("ambiguous 409 cleared Dirty")
	}
}

func TestRecoveryFailsClosedAndKeepsDirty(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		t.Run(fmt.Sprintf("bound-completed-empty/automatic=%v", automatic), func(t *testing.T) {
			req := weatherReq()
			req.ToolChoice = "auto"
			req.Messages = append(req.Messages, message{Role: "assistant", ToolCalls: []toolCall{{ID: "already-delivered", Type: "function", Function: callFunction{Name: "get_weather", Arguments: `{"city":"Paris"}`}}}}, message{Role: "tool", ToolCallID: "already-delivered", Content: "result already produced"})
			f := &inspectBackend{fakeBackend: &fakeBackend{answers: []string{"ACK", "continue safely"}}, expected: "full pending tool-result prompt", readErr: chatgpt.ErrCompletedEmpty}
			h := testHandler(f.fakeBackend)
			h.client = f
			h.statePath = filepath.Join(t.TempDir(), "state.json")
			s := &session{URL: "https://chatgpt.com/c/stored", Dirty: true, Phase: "final", Messages: append([]message{}, req.Messages[:len(req.Messages)-1]...), PendingRequest: &req, PendingPrompt: f.expected}
			h.sessions["recover"] = s
			before, _ := json.Marshal(s.Messages)
			var w *httptest.ResponseRecorder
			if automatic {
				w = request(h, "recover", req)
			} else {
				w = recoveryRequest(h, nil)
			}
			if w.Code != 422 || s.Dirty || s.Phase != "invalid" || s.PendingRequest != &req || len(s.Result.Calls) != 0 || len(f.prompts) != 0 || f.reads != 1 {
				t.Fatal("completed empty not durably rejected", w.Code, w.Body.String())
			}
			after, _ := json.Marshal(s.Messages)
			if !bytes.Equal(before, after) {
				t.Fatal("prior delivered call history changed")
			}
			if retry := request(h, "recover", req); retry.Code != 422 || f.reads != 1 || len(f.prompts) != 0 {
				t.Fatal("empty retry submitted/re-read")
			}
			loaded := testHandler(f.fakeBackend)
			loaded.statePath = h.statePath
			if err := loaded.load(); err != nil || loaded.sessions["recover"].Phase != "invalid" || loaded.sessions["recover"].Dirty {
				t.Fatal("empty rejection not restart-safe", err)
			}
			current := req
			current.Messages = append(append([]message{}, req.Messages...), message{Role: "user", Content: "summarize the already produced result"})
			if next := request(h, "recover", current); next.Code != 200 || len(f.prompts) == 0 {
				t.Fatal("new-user continuation blocked", next.Code, next.Body.String())
			}
			if !strings.Contains(strings.Join(f.prompts, "\n"), "already-delivered") || !strings.Contains(strings.Join(f.prompts, "\n"), "result already produced") {
				t.Fatal("inert tool context lost")
			}
		})
	}
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
		f.readErr = chatgpt.ErrCompletedEmpty
		if w := request(h, "auto-save-failure", req); w.Code != 422 || !s.Dirty || s.Phase != "final" || s.PendingRequest != &req || s.PendingPrompt != f.expected || s.Fingerprint != "previous-delivered" || len(f.prompts) != 0 {
			t.Fatal("failed empty rejection discarded pending binding")
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
			pending := req
			s.Phase, s.PendingRequest, s.PendingPrompt = "final", &pending, f.expected
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

func TestDirtyRewriteReseedAbandonsUncertainPending(t *testing.T) {
	// Turn1 commits a weather call; turn2 (result + follow-up) is
	// interrupted at submit and stays Dirty/final. The client then forks
	// history coherently, dropping the executed exchange. The bridge must
	// reseed fresh: carried executed results travel as inert record, the
	// never-delivered pending turn is listed as NOT carried, and the old
	// thread is abandoned without adoption.
	f := &fakeBackend{answers: []string{weatherAnswer, "", "ACK", "reseeded answer"}}
	h := testHandler(f)
	h.statePath = filepath.Join(t.TempDir(), "state.json")
	req := weatherReq()
	a := assistant(t, request(h, "rdirty", req))
	callID := a.ToolCalls[0].ID
	oldURL := h.sessions["rdirty"].URL
	req.Messages = append(append([]message{}, req.Messages...), a, message{Role: "tool", ToolCallID: callID, Content: "25 C"}, message{Role: "user", Content: "and tomorrow?"})
	req.ToolChoice = "none"
	f.err, f.errorURL = context.Canceled, oldURL
	if w := request(h, "rdirty", req); w.Code != 502 || !h.sessions["rdirty"].Dirty || h.sessions["rdirty"].Phase != "final" {
		t.Fatalf("interrupted turn did not stay uncertain: %d %s", w.Code, w.Body.String())
	}
	rewritten := completionsRequest{Messages: []message{
		{Role: "user", Content: "Call weather for Hanoi"},
		{Role: "user", Content: "summarize instead"},
	}, ToolChoice: "none"}
	f.err = nil
	got := assistant(t, request(h, "rdirty", rewritten))
	if contentText(got) != "reseeded answer" {
		t.Fatalf("dirty rewrite did not reseed fresh: %+v", got)
	}
	s := h.sessions["rdirty"]
	if s.Dirty || s.Phase != "" || s.PendingRequest != nil || s.PendingPrompt != "" || s.URL == "" || s.URL == oldURL {
		t.Fatalf("reseed left uncertainty behind or reused the thread: %+v", s)
	}
	if len(f.prompts) != 4 || f.urls[2] != "" || f.urls[3] != s.URL {
		t.Fatalf("reseed did not seed a fresh thread record-first: urls=%v", f.urls)
	}
	if record := f.prompts[2]; !strings.Contains(record, "PRIOR EXECUTION RECORD") || !strings.Contains(record, callID) {
		t.Fatal("carried executed exchange missing from record import")
	}
	if final := f.prompts[3]; strings.Contains(final, callID) || strings.Contains(final, "<opencode_bridge_call>") {
		t.Fatal("carried call re-submitted in final prompt")
	}
	// Unknown-state items fail closed in the listing, reseed proceeds for
	// the known-safe remainder.
	for _, want := range []string{"rewrite-divergence", "old=" + oldURL, "new=" + s.URL, callID + "(get_weather)", "pending:", "outcome unknown"} {
		if !strings.Contains(s.ReseedEvidence, want) {
			t.Fatalf("evidence lacks %q: %s", want, s.ReseedEvidence)
		}
	}
	if s.ReseedFrom != oldURL {
		t.Fatalf("linkage lost: %q", s.ReseedFrom)
	}
	if request(h, "rdirty", rewritten); len(f.prompts) != 4 {
		t.Fatal("retry after dirty reseed resubmitted")
	}
	loaded := testHandler(&fakeBackend{})
	loaded.statePath = h.statePath
	if err := loaded.load(); err != nil || loaded.sessions["rdirty"].Dirty || loaded.sessions["rdirty"].ReseedEvidence == "" {
		t.Fatal("dirty reseed not restart-safe", err)
	}
}

func TestDirtyContinuationReseedsInsteadOfBricking(t *testing.T) {
	// Turn1 commits a weather call; turn2 is interrupted at submit and stays
	// Dirty/final. The client then reverts that uncertain turn and continues.
	// Its history is a canonical PREFIX-EXTENSION of the committed journal —
	// the exact shape opencode sends after a revert, a model switch or a
	// retry. It used to classify as append-only ("not a rewrite"), so no plan
	// existed, adoption and auto-resync had nothing to bind, and every retry
	// got the permanent uncertainty refusal. It must seed a fresh thread and
	// deliver the new message instead.
	f := &fakeBackend{answers: []string{weatherAnswer, "", "reseeded answer"}}
	h := testHandler(f)
	h.statePath = filepath.Join(t.TempDir(), "state.json")
	req := weatherReq()
	a := assistant(t, request(h, "rdirty2", req))
	callID := a.ToolCalls[0].ID
	oldURL := h.sessions["rdirty2"].URL
	committed := append([]message{}, h.sessions["rdirty2"].Messages...)
	req.Messages = append(append([]message{}, req.Messages...), a, message{Role: "tool", ToolCallID: callID, Content: "25 C"}, message{Role: "user", Content: "and tomorrow?"})
	req.ToolChoice = "none"
	f.err, f.errorURL = context.Canceled, oldURL
	if w := request(h, "rdirty2", req); w.Code != 502 || !h.sessions["rdirty2"].Dirty || h.sessions["rdirty2"].Phase != "final" {
		t.Fatalf("interrupted turn did not stay uncertain: %d %s", w.Code, w.Body.String())
	}
	next := completionsRequest{Messages: append(append([]message{}, committed...),
		message{Role: "tool", ToolCallID: callID, Content: "25 C"},
		message{Role: "user", Content: "what about Saigon?"}), ToolChoice: "none"}
	if !prefix(next.Messages, committed) || len(next.Messages) <= len(committed) {
		t.Fatal("test setup is not a canonical prefix-extension")
	}
	f.err = nil
	got := assistant(t, request(h, "rdirty2", next))
	if contentText(got) != "reseeded answer" {
		t.Fatalf("dirty continuation was not reseeded: %+v", got)
	}
	s := h.sessions["rdirty2"]
	if s.Dirty || s.Phase != "" || s.PendingRequest != nil || s.PendingPrompt != "" || s.URL == "" || s.URL == oldURL {
		t.Fatalf("continuation reseed left uncertainty behind or reused the thread: %+v", s)
	}
	for _, want := range []string{"rewrite-continuation", "old=" + oldURL, "new=" + s.URL, "pending:", "outcome unknown"} {
		if !strings.Contains(s.ReseedEvidence, want) {
			t.Fatalf("evidence lacks %q: %s", want, s.ReseedEvidence)
		}
	}
	if s.ReseedFrom != oldURL {
		t.Fatalf("linkage lost: %q", s.ReseedFrom)
	}
	if len(f.prompts) != 3 || f.urls[2] != "" {
		t.Fatalf("continuation reseed did not mint a fresh thread: urls=%v", f.urls)
	}
	if final := f.prompts[2]; !strings.Contains(final, "what about Saigon?") {
		t.Fatal("new user message was dropped")
	} else if strings.Contains(final, "and tomorrow?") {
		t.Fatal("abandoned uncertain turn was resubmitted")
	}
	if request(h, "rdirty2", next); len(f.prompts) != 3 {
		t.Fatal("retry after continuation reseed resubmitted")
	}
}

func TestDirtyContinuationLoadBearingKeepsUncertainty(t *testing.T) {
	// Same canonical prefix-extension shape, but the sent log holds a
	// COMPLETED answer for the pending prompt on the old thread: abandoning
	// it would lose real work, so the continuation still refuses.
	h1 := "first submission prompt H1 with full context"
	req := weatherReq()
	f := &resyncBackend{fakeBackend: &fakeBackend{}, mismatch: structuralMismatch(), latestUser: h1, latestAnswer: weatherAnswer, latestErr: errors.New("stored thread holds no complete assistant answer to transplant")}
	h := testHandler(f.fakeBackend)
	h.client = f
	h.statePath = filepath.Join(t.TempDir(), "state.json")
	saved := []message{{Role: "user", Content: "first"}, {Role: "assistant", Content: "ok"}}
	h.sessions["recover"] = &session{URL: "https://chatgpt.com/c/stored", Dirty: true, Phase: "final", PendingRequest: &req, PendingPrompt: h1, Messages: saved}
	h.appendSent(sentEntry{Event: "completed", Session: "recover", URL: "https://chatgpt.com/c/stored", PromptHash: promptHash(h1), Bytes: len(h1)})
	next := completionsRequest{Messages: append(append([]message{}, saved...), message{Role: "user", Content: "next"}), ToolChoice: "none"}
	if !prefix(next.Messages, saved) {
		t.Fatal("test setup is not a canonical prefix-extension")
	}
	if w := request(h, "recover", next); w.Code != 422 || !strings.Contains(w.Body.String(), "sent-log:") || len(f.prompts) != 0 {
		t.Fatalf("load-bearing continuation abandoned uncertainty: %d %s", w.Code, w.Body.String())
	}
	s := h.sessions["recover"]
	if !s.Dirty || s.Phase != "final" || s.PendingPrompt != h1 || s.ReseedEvidence != "" || s.ReseedFrom != "" {
		t.Fatalf("load-bearing continuation changed the journal: %+v", s)
	}
	if w := recoveryRequest(h, nil); w.Code != 409 || !h.sessions["recover"].Dirty {
		t.Fatalf("load-bearing continuation recover lost diagnosis or Dirty: %d %s", w.Code, w.Body.String())
	}
}

func TestDirtyRewriteReseedLoadBearingKeepsUncertainty(t *testing.T) {
	// A verifiable completed answer for the still-pending prompt exists on
	// the old thread, but adoption fails structurally and auto-resync
	// cannot transplant it. That unknown is load-bearing: a coherent
	// rewrite must NOT abandon it silently — completions keeps terminal
	// 422 and recover keeps 409, both with the sent-log diagnosis, Dirty
	// untouched, nothing submitted.
	const h1 = "first submission prompt H1 with full context"
	req := weatherReq()
	f := &resyncBackend{fakeBackend: &fakeBackend{}, mismatch: structuralMismatch(), latestUser: h1, latestAnswer: weatherAnswer, latestErr: errors.New("stored thread holds no complete assistant answer to transplant")}
	h := testHandler(f.fakeBackend)
	h.client = f
	h.statePath = filepath.Join(t.TempDir(), "state.json")
	h.sessions["recover"] = &session{URL: "https://chatgpt.com/c/stored", Dirty: true, Phase: "final", PendingRequest: &req, PendingPrompt: h1, Messages: req.Messages[:1]}
	h.appendSent(sentEntry{Event: "completed", Session: "recover", URL: "https://chatgpt.com/c/stored", PromptHash: promptHash(h1), Bytes: len(h1)})
	rewritten := completionsRequest{Messages: []message{{Role: "user", Content: "something else entirely"}}, ToolChoice: "none"}
	if w := request(h, "recover", rewritten); w.Code != 422 || !strings.Contains(w.Body.String(), "sent-log:") || len(f.prompts) != 0 {
		t.Fatalf("load-bearing rewrite abandoned uncertainty: %d %s", w.Code, w.Body.String())
	}
	s := h.sessions["recover"]
	if !s.Dirty || s.Phase != "final" || s.PendingPrompt != h1 || s.ReseedEvidence != "" || s.ReseedFrom != "" {
		t.Fatalf("load-bearing 422 changed the journal: %+v", s)
	}
	if w := recoveryRequest(h, nil); w.Code != 409 || !strings.Contains(w.Body.String(), "sent-log:") || len(f.prompts) != 0 {
		t.Fatalf("load-bearing recover lost diagnosis: %d %s", w.Code, w.Body.String())
	}
	if !h.sessions["recover"].Dirty {
		t.Fatal("load-bearing 409 cleared Dirty")
	}
}

func TestDirtyIncoherentHistoryKeepsUncertainty(t *testing.T) {
	// A dirty session met with an internally incoherent rewrite (dangling
	// tool call) is genuinely ambiguous: no reseed, terminal 422/409 with
	// sent-log diagnosis, Dirty untouched, nothing submitted.
	req := weatherReq()
	f := &inspectBackend{fakeBackend: &fakeBackend{}, expected: "newer retry prompt H3", readErr: errors.New("stored user prompt does not exactly match original request (rendered 10B want 20B)")}
	h := testHandler(f.fakeBackend)
	h.client = f
	h.statePath = filepath.Join(t.TempDir(), "state.json")
	h.sessions["recover"] = &session{
		URL: "https://chatgpt.com/c/stored", Messages: []message{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "ok"}},
		Dirty: true, Phase: "final", PendingRequest: &req, PendingPrompt: "newer retry prompt H3",
	}
	h.appendSent(sentEntry{Event: "completed", Session: "recover", URL: "https://chatgpt.com/c/stored", PromptHash: promptHash("original submitted prompt H1"), Bytes: 100})
	dangling := completionsRequest{Messages: []message{
		{Role: "user", Content: "changed"},
		{Role: "assistant", ToolCalls: []toolCall{{ID: "ghost", Type: "function", Function: callFunction{Name: "get_weather", Arguments: `{"city":"Hanoi"}`}}}},
	}, ToolChoice: "none"}
	if w := request(h, "recover", dangling); w.Code != 422 || !strings.Contains(w.Body.String(), "sent-log:") || len(f.prompts) != 0 {
		t.Fatalf("incoherent rewrite abandoned uncertainty: %d %s", w.Code, w.Body.String())
	}
	s := h.sessions["recover"]
	if !s.Dirty || s.Phase != "final" || s.ReseedEvidence != "" || s.ReseedFrom != "" {
		t.Fatalf("incoherent 422 changed the journal: %+v", s)
	}
	if w := recoveryRequest(h, nil); w.Code != 409 || !strings.Contains(w.Body.String(), "sent-log:") || !h.sessions["recover"].Dirty {
		t.Fatalf("incoherent recover lost diagnosis or Dirty: %d %s", w.Code, w.Body.String())
	}
}

func TestRewriteReseedNeverFiresOnAppend(t *testing.T) {
	// Coherent append-only histories never trigger reseed: same thread,
	// no evidence, no sent-log reseed event.
	f := &fakeBackend{answers: []string{weatherAnswer, "23 C"}}
	h := testHandler(f)
	req := weatherReq()
	a := assistant(t, request(h, "happy", req))
	thread := h.sessions["happy"].URL
	req.Messages = append(req.Messages, a, message{Role: "tool", ToolCallID: a.ToolCalls[0].ID, Content: "23 C"})
	req.ToolChoice = "none"
	if got := assistant(t, request(h, "happy", req)); contentText(got) != "23 C" {
		t.Fatal(got)
	}
	s := h.sessions["happy"]
	if s.Dirty || s.URL != thread || f.urls[1] != thread || s.ReseedEvidence != "" || s.ReseedFrom != "" {
		t.Fatalf("happy path reseeded or left the thread: %+v urls=%v", s, f.urls)
	}
	for _, e := range h.sentLog {
		if e.Event == "reseed" {
			t.Fatalf("happy path logged a reseed: %+v", e)
		}
	}
}

// rejectionClass reads the refusal class durably recorded with
// RejectedFingerprint from the persisted journal, so the test observes what a
// restart would see rather than in-memory bookkeeping.
func rejectionClass(t *testing.T, statePath, id string) string {
	t.Helper()
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]map[string]any
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	class, _ := state[id]["RejectReason"].(string)
	return class
}

// A completed-empty final is cause (a): the vendor backend proved the assistant
// produced no content at all. It must be recorded and reported as an empty
// final, and that empty answer must never be parsed again — doing so produced a
// SECOND rejection log that read as if the model had emitted an invalid call.
func TestCompletedEmptyFinalIsReportedAsEmptyNotRefused(t *testing.T) {
	req := weatherReq()
	req.ToolChoice = "auto"
	req.Messages = append(req.Messages, message{Role: "assistant", ToolCalls: []toolCall{{ID: "already-delivered", Type: "function", Function: callFunction{Name: "get_weather", Arguments: `{"city":"Paris"}`}}}}, message{Role: "tool", ToolCallID: "already-delivered", Content: "result already produced"})
	f := &inspectBackend{fakeBackend: &fakeBackend{answers: []string{"ACK", "continue safely"}}, expected: "full pending tool-result prompt", readErr: chatgpt.ErrCompletedEmpty}
	h := testHandler(f.fakeBackend)
	h.client = f
	h.statePath = filepath.Join(t.TempDir(), "state.json")
	s := &session{URL: "https://chatgpt.com/c/stored", Dirty: true, Phase: "final", Messages: append([]message{}, req.Messages[:len(req.Messages)-1]...), PendingRequest: &req, PendingPrompt: f.expected}
	h.sessions["recover"] = s

	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(prev)

	w := request(h, "recover", req)
	if w.Code != 422 || s.Dirty || s.Phase != "invalid" || len(f.prompts) != 0 || len(s.Result.Calls) != 0 {
		t.Fatalf("completed empty final not durably rejected: %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "finished without producing an answer") {
		t.Fatalf("empty final misreported: %s", body)
	}
	if strings.Contains(body, "could not be used") {
		t.Fatalf("empty final reported as a protocol refusal: %s", body)
	}
	if got := rejectionClass(t, h.statePath, "recover"); got != "empty_final" {
		t.Fatalf("empty final refusal class not durable: %q", got)
	}
	if n := strings.Count(logs.String(), "completed stored output rejected"); n != 0 {
		t.Fatalf("empty final re-parsed into %d protocol refusal log(s): %s", n, logs.String())
	}
	if n := strings.Count(logs.String(), "bound completed empty final rejected"); n != 1 {
		t.Fatalf("expected exactly one empty-final rejection log, got %d: %s", n, logs.String())
	}
}

// A refusal of output the model DID produce is cause (b): the stored answer
// failed strict tool validation. It must be recorded as a protocol refusal and
// must never be reported as an empty final, including at the recover gate.
func TestProtocolRefusalIsReportedAsRefusedNotEmpty(t *testing.T) {
	req := weatherReq()
	req.Messages = append([]message{{Role: "system", Content: "context"}}, req.Messages...)
	f := &inspectBackend{fakeBackend: &fakeBackend{}, answer: `<opencode_bridge_call>{"name":"get_weather","arguments":{}}</opencode_bridge_call>`}
	h := testHandler(f.fakeBackend)
	h.client = f
	h.statePath = filepath.Join(t.TempDir(), "state.json")
	s := &session{URL: "https://chatgpt.com/c/stored", Dirty: true, Messages: req.Messages[:1], ToolsFP: toolsFingerprint(req.Tools)}
	h.sessions["recover"] = s
	f.expected = h.buildTurns(req, req.Messages[1:], toolReminder(req))[0].prompt
	pending := req
	s.Phase, s.PendingRequest, s.PendingPrompt = "final", &pending, f.expected

	w := recoveryRequest(h, &req)
	if w.Code != 422 || s.Dirty || s.Phase != "invalid" || len(f.prompts) != 0 || len(s.Result.Calls) != 0 {
		t.Fatalf("schema-invalid stored answer not refused: %d %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "replied in a form that could not be used") || strings.Contains(body, "without producing an answer") {
		t.Fatalf("protocol refusal misreported: %s", body)
	}
	if got := rejectionClass(t, h.statePath, "recover"); got != "tool_protocol" {
		t.Fatalf("protocol refusal class not durable: %q", got)
	}
	// The recover gate must name the refusal, never emptiness.
	gate := recoveryRequest(h, nil)
	if gate.Code != 422 || !strings.Contains(gate.Body.String(), "replied in a form that could not be used") || strings.Contains(gate.Body.String(), "without producing an answer") {
		t.Fatalf("recover gate misreported the refusal: %d %s", gate.Code, gate.Body.String())
	}
}

// --- user-facing recovery contract ---------------------------------------

func stateGet(h *handler, id, method string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/v1/sessions/state", nil)
	if id != "" {
		r.Header.Set("X-Session-Id", id)
	}
	w := httptest.NewRecorder()
	h.routes().ServeHTTP(w, r)
	return w
}

func stateBody(t *testing.T, h *handler, id string) map[string]any {
	t.Helper()
	w := stateGet(h, id, "GET")
	if w.Code != 200 {
		t.Fatalf("state %d: %s", w.Code, w.Body.String())
	}
	var v map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func abandonPost(h *handler, body string, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/v1/sessions/abandon", strings.NewReader(body))
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	h.routes().ServeHTTP(w, r)
	return w
}

func stateActionsOf(v map[string]any) []string {
	raw, _ := v["actions"].([]any)
	out := make([]string, 0, len(raw))
	for _, a := range raw {
		out = append(out, fmt.Sprint(a))
	}
	return out
}

// The inspector must name all four failure classes distinctly, and each action
// list must contain only what is really available.
func TestSessionStateDistinguishesRecoveryClasses(t *testing.T) {
	req := weatherReq()
	cases := []struct {
		name       string
		session    *session
		absent     bool
		state      string
		canRecover bool
		canResync  bool
		actions    []string
	}{
		{"absent", nil, true, stateUnknownSession, false, false, []string{}},
		{"clean", &session{URL: "https://chatgpt.com/c/t", Messages: []message{{Role: "user", Content: "hi"}}}, false, stateClean, false, false, []string{actionContinue}},
		{"proven pre-submit keeps no uncertainty", &session{URL: "https://chatgpt.com/c/t", Messages: []message{{Role: "user", Content: "hi"}}}, false, stateClean, false, false, []string{actionContinue}},
		{"adoptable pending", &session{URL: "https://chatgpt.com/c/t", Messages: []message{{Role: "user", Content: "hi"}}, Dirty: true, Phase: "final", PendingRequest: &req, PendingPrompt: "p"}, false, statePendingVerifiable, true, true, []string{actionRecover, actionResync, actionAbandon}},
		{"inert replay only", &session{Dirty: true, Phase: "replay"}, false, statePendingVerifiable, false, false, []string{actionContinue, actionAbandon}},
		{"resync only", &session{URL: "https://chatgpt.com/c/t", Dirty: true, Messages: []message{{Role: "user", Content: "hi"}}}, false, statePendingVerifiable, false, true, []string{actionResync, actionAbandon}},
		{"genuinely unverifiable", &session{Dirty: true}, false, statePendingUnverifiable, false, false, []string{actionAbandon}},
		{"rejected output", &session{Phase: "invalid", RejectedFingerprint: "fp", RejectReason: rejectToolProtocol}, false, stateRejectedOutput, false, false, []string{actionContinue}},
		{"empty final", &session{Phase: "invalid", RejectedFingerprint: "fp", RejectReason: rejectEmptyFinal}, false, stateEmptyFinal, false, false, []string{actionContinue}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := testHandler(&fakeBackend{})
			if !c.absent {
				h.sessions["state"] = c.session
			}
			v := stateBody(t, h, "state")
			if got := fmt.Sprint(v["state"]); got != c.state {
				t.Fatalf("state %q want %q (%v)", got, c.state, v)
			}
			if v["can_recover"] != c.canRecover || v["can_resync"] != c.canResync {
				t.Fatalf("can_recover/can_resync = %v/%v want %v/%v", v["can_recover"], v["can_resync"], c.canRecover, c.canResync)
			}
			if got := stateActionsOf(v); strings.Join(got, ",") != strings.Join(c.actions, ",") {
				t.Fatalf("actions %v want %v", got, c.actions)
			}
			if s, _ := v["summary"].(string); s == "" {
				t.Fatal("missing plain-English summary")
			}
		})
	}
}

// The inspector is read-only and content-free: no prompt text, tool arguments,
// message bodies or call ids, and no journal mutation or browser work.
func TestSessionStateLeaksNoContentAndDoesNotMutate(t *testing.T) {
	const secret = "SECRETPROMPTTEXT-should-never-appear"
	const argSecret = "ARGSECRET-should-never-appear"
	req := completionsRequest{Messages: []message{{Role: "user", Content: secret}}, Tools: []toolDef{{Type: "function", Function: toolFunction{Name: "get_weather", Parameters: map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string", "description": argSecret}}}}}}}
	f := &fakeBackend{}
	h := testHandler(f)
	h.sessions["leak"] = &session{
		URL: "https://chatgpt.com/c/leak", Fingerprint: "fp",
		Messages: []message{
			{Role: "user", Content: secret},
			{Role: "assistant", ToolCalls: []toolCall{{ID: "call_secret_id", Type: "function", Function: callFunction{Name: "get_weather", Arguments: `{"city":"` + argSecret + `"}`}}}},
			{Role: "tool", ToolCallID: "call_secret_id", Content: "29 C"},
		},
		Dirty: true, Phase: "final", PendingRequest: &req, PendingPrompt: secret + " pending",
	}
	before, _ := json.Marshal(h.sessions["leak"])

	w := stateGet(h, "leak", "GET")
	if w.Code != 200 {
		t.Fatalf("state %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, leak := range []string{secret, argSecret, "call_secret_id", "Now produce the next assistant reply", "/v1/sessions/recover"} {
		if strings.Contains(body, leak) {
			t.Fatalf("state inspector leaked %q: %s", leak, body)
		}
	}
	var v map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"session": true, "state": true, "can_recover": true, "can_resync": true, "thread_url_present": true, "completed_messages": true, "pending_prompt_bytes": true, "outstanding_tool_calls": true, "summary": true, "actions": true}
	for k := range v {
		if !allowed[k] {
			t.Fatalf("unexpected field %q in state body", k)
		}
	}
	if v["completed_messages"] != float64(3) || v["pending_prompt_bytes"] != float64(len(secret+" pending")) || v["outstanding_tool_calls"] != float64(0) {
		t.Fatalf("counts/sizes wrong: %v", v)
	}
	if len(f.prompts) != 0 || len(f.urls) != 0 {
		t.Fatal("state inspection touched the browser")
	}
	after, _ := json.Marshal(h.sessions["leak"])
	if !bytes.Equal(before, after) {
		t.Fatal("state inspection mutated the journal")
	}
	if w := stateGet(h, "leak", "POST"); w.Code != 405 {
		t.Fatalf("state accepted POST: %d", w.Code)
	}
	if w := stateGet(h, "", "GET"); w.Code != 400 {
		t.Fatalf("state accepted a missing identity: %d", w.Code)
	}
}

// Abandon is an explicit, named, non-wildcard operation. Anything else is
// refused WHOLE and changes nothing.
func TestAbandonRefusesImplicitOrBrowserRequests(t *testing.T) {
	f := &fakeBackend{}
	h := testHandler(f)
	h.statePath = filepath.Join(t.TempDir(), "state.json")
	h.sessions["keep"] = &session{URL: "https://chatgpt.com/c/t", Dirty: true, Phase: "final", PendingPrompt: "p"}

	for _, body := range []string{`{"sessions":[]}`, `{"sessions":["*"]}`, `{"sessions":["keep","all"]}`, `{"sessions":["any"]}`, `{"sessions":[""]}`, `{`} {
		w := abandonPost(h, body, "")
		if w.Code != 400 {
			t.Fatalf("body %s not refused whole: %d %s", body, w.Code, w.Body.String())
		}
		if !h.sessions["keep"].Dirty {
			t.Fatalf("refused body %s still cleared the journal", body)
		}
	}
	var many []string
	for i := 0; i < maxAbandonSessions+1; i++ {
		many = append(many, fmt.Sprintf("ses_%d", i))
	}
	over, _ := json.Marshal(map[string]any{"sessions": many})
	if w := abandonPost(h, string(over), ""); w.Code != 400 {
		t.Fatalf("over-cap list accepted: %d", w.Code)
	}
	// A browser-originated POST (cross-site fetch to loopback) is refused.
	if w := abandonPost(h, `{"sessions":["keep"]}`, "https://attacker.example"); w.Code != 403 || !h.sessions["keep"].Dirty {
		t.Fatalf("browser-originated abandon not refused: %d %s", w.Code, w.Body.String())
	}
	if w := abandonPost(h, `{"sessions":["keep"]}`, ""); w.Code != 200 {
		t.Fatalf("explicit abandon failed: %d %s", w.Code, w.Body.String())
	}
}

// Abandon clears ONLY the pending binding: transcript, thread, cache identity
// and executed results survive, and it survives a restart.
func TestAbandonClearsOnlyNamedSessionsAndRetainsHistory(t *testing.T) {
	f := &fakeBackend{}
	h := testHandler(f)
	h.statePath = filepath.Join(t.TempDir(), "state.json")
	req := weatherReq()
	delivered := message{Role: "assistant", ToolCalls: []toolCall{
		{ID: "call_done", Type: "function", Function: callFunction{Name: "get_weather", Arguments: `{"city":"Hanoi"}`}},
		{ID: "call_open", Type: "function", Function: callFunction{Name: "get_weather", Arguments: `{"city":"Paris"}`}},
	}}
	h.sessions["go"] = &session{
		URL: "https://chatgpt.com/c/go", Fingerprint: "cache-fp", ToolsFP: "tools-fp",
		Messages:            []message{{Role: "user", Content: "hi"}, delivered, {Role: "tool", ToolCallID: "call_done", Content: "25 C"}},
		Result:              result{ID: "chatcmpl-cached", Text: "cached answer"},
		Project:             "https://chatgpt.com/g/g-p-project",
		RejectedFingerprint: "rejected-fp", RejectReason: rejectToolProtocol,
		RepairFingerprint: "repair-fp", RepairPendingPrompt: "repair prompt",
		Dirty: true, Phase: "final", PendingRequest: &req, PendingPrompt: "stuck pending prompt",
		ResyncMessages: []message{{Role: "user", Content: "reviewed"}}, ResyncFrom: "https://chatgpt.com/c/old",
	}
	h.sessions["other"] = &session{URL: "https://chatgpt.com/c/other", Dirty: true, Phase: "final", PendingPrompt: "other pending"}

	w := abandonPost(h, `{"sessions":["go"],"reason":"operator reviewed the thread"}`, "")
	if w.Code != 200 {
		t.Fatalf("abandon %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Requested int `json:"requested"`
		Abandoned int `json:"abandoned"`
		Results   []abandonOutcome
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Requested != 1 || body.Abandoned != 1 || len(body.Results) != 1 || body.Results[0].Outcome != "abandoned" {
		t.Fatalf("unexpected abandon response: %s", w.Body.String())
	}
	if r := body.Results[0]; r.DeliveredCalls != 2 || r.RecordedResults != 1 || r.OutstandingCalls != 1 || r.AbandonedBytes != len("stuck pending prompt") || r.StateBefore != statePendingVerifiable {
		t.Fatalf("abandon did not account for delivered/executed work: %+v", r)
	}
	s := h.sessions["go"]
	if s.Dirty || s.Phase != "" || s.PendingRequest != nil || s.PendingPrompt != "" || s.RepairPendingPrompt != "" || s.ResyncMessages != nil || s.ResyncFrom != "" {
		t.Fatalf("pending binding not fully cleared: %+v", s)
	}
	if s.URL != "https://chatgpt.com/c/go" || s.Fingerprint != "cache-fp" || s.ToolsFP != "tools-fp" || s.Result.ID != "chatcmpl-cached" || len(s.Messages) != 3 || s.Project == "" || s.RejectedFingerprint != "rejected-fp" || s.RejectReason != rejectToolProtocol || s.RepairFingerprint != "repair-fp" {
		t.Fatalf("abandon discarded retained history/identity: %+v", s)
	}
	if other := h.sessions["other"]; !other.Dirty || other.PendingPrompt != "other pending" {
		t.Fatal("abandon touched a session it was not given")
	}
	// A named session that is not pending is reported, never silently cleared.
	if w := abandonPost(h, `{"sessions":["go","missing"]}`, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"not_pending"`) || !strings.Contains(w.Body.String(), `"unknown"`) {
		t.Fatalf("per-session outcome not explicit: %d %s", w.Code, w.Body.String())
	}
	// Durable: a restart sees the cleared binding and the retained transcript.
	h2 := testHandler(f)
	h2.statePath = h.statePath
	if err := h2.load(); err != nil {
		t.Fatal(err)
	}
	if reloaded := h2.sessions["go"]; reloaded == nil || reloaded.Dirty || len(reloaded.Messages) != 3 || reloaded.URL == "" {
		t.Fatalf("abandonment not durable: %+v", reloaded)
	}
}

func TestAbandonNeverReportsSuccessWithoutDurability(t *testing.T) {
	h := testHandler(&fakeBackend{})
	h.statePath = t.TempDir() // a directory: the atomic rename cannot succeed
	req := weatherReq()
	h.sessions["go"] = &session{URL: "https://chatgpt.com/c/go", Messages: []message{{Role: "user", Content: "hi"}}, Dirty: true, Phase: "final", PendingRequest: &req, PendingPrompt: "p"}
	w := abandonPost(h, `{"sessions":["go"]}`, "")
	if w.Code != 500 {
		t.Fatalf("non-durable clear reported %d: %s", w.Code, w.Body.String())
	}
	if s := h.sessions["go"]; !s.Dirty || s.Phase != "final" || s.PendingPrompt != "p" {
		t.Fatalf("failed persistence was not rolled back: %+v", s)
	}
	if v := stateBody(t, h, "go"); fmt.Sprint(v["state"]) != statePendingVerifiable {
		t.Fatalf("rollback left the inspector claiming %v", v["state"])
	}
}

func TestAbandonRefusedWhileTurnInFlight(t *testing.T) {
	h := testHandler(&fakeBackend{})
	h.sessions["go"] = &session{URL: "https://chatgpt.com/c/go", Dirty: true, Phase: "final", PendingPrompt: "p"}
	h.gate <- struct{}{} // a live turn holds the whole-turn gate
	defer func() { <-h.gate }()
	if w := abandonPost(h, `{"sessions":["go"]}`, ""); w.Code != 503 {
		t.Fatalf("abandon ran next to a live turn: %d %s", w.Code, w.Body.String())
	}
	if s := h.sessions["go"]; !s.Dirty || s.PendingPrompt != "p" {
		t.Fatal("busy refusal changed the journal")
	}
	if w := abandonPost(h, `{"sessions":["go"]}`, ""); w.Code == 200 {
		t.Fatal("abandon succeeded while a turn was in flight")
	}
}
