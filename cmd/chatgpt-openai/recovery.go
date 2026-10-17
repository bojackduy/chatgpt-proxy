package main

import (
	"chatgpt-proxy/internal/chatgpt"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

type recoveryBackend interface {
	ReadThreadTurn(context.Context, string, string) (chatgpt.TurnResult, error)
}

// An absent binding is not evidence that a turn was never submitted. Replay is
// different: its explicit phase proves only inert imports were in flight.
// Classification is derived so loading an old record preserves its raw fields.
func (s *session) incompleteJournal() bool {
	return s != nil && s.Dirty && s.Phase != "replay" &&
		(s.Phase != "final" || s.PendingRequest == nil || len(s.PendingRequest.Messages) == 0 || s.PendingPrompt == "" ||
			(s.RepairPendingPrompt != "" && (s.RepairPendingPrompt != s.PendingPrompt || s.RepairFingerprint != requestFingerprint(*s.PendingRequest))))
}

func incompleteJournalError(w http.ResponseWriter) {
	writeJSON(w, 422, map[string]any{"error": map[string]any{
		"type": "incomplete_journal", "code": "incomplete_journal",
		"message": "session uncertainty journal lacks a complete original request/prompt binding; submission outcome is unknown. Read-only /v1/sessions/recover cannot reconstruct it safely. Explicitly POST /v1/sessions/resync with the authoritative completed transcript and recorded tool results, then send a NEW ordinary user message; historical tools will be imported inertly, never rerun",
	}})
}

// readPendingTurn waits for the stored thread's answer to complete and reads
// it against the pending prompt. It returns adopted=true with the thread
// result when the pending prompt matches (or when a completed-empty final is
// durably rejected, which the caller treats as adopted-as-invalid). A typed
// structural mismatch is returned separately so the caller can attempt
// hash-lineage auto-resync; every other failure returns adopted=false with a
// nil mismatch and the caller keeps terminal uncertainty.
func (h *handler) readPendingTurn(r *http.Request, id string, s *session) (chatgpt.TurnResult, *chatgpt.UserBubbleMismatchError, bool) {
	pending := s.PendingRequest
	if pending == nil || s.PendingPrompt == "" || s.URL == "" {
		return chatgpt.TurnResult{}, nil, false
	}
	backend, ok := h.client.(recoveryBackend)
	if !ok {
		return chatgpt.TurnResult{}, nil, false
	}
	// Errors that waiting cannot fix: the stored prompt genuinely does not
	// match, or the thread holds multiple candidate answers.
	deterministic := func(err error) bool {
		if err == nil {
			return false
		}
		msg := err.Error()
		return strings.Contains(msg, "does not exactly match") ||
			strings.Contains(msg, "ambiguous multiple assistant")
	}
	deadline := time.Now().Add(90 * time.Second)
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	var res chatgpt.TurnResult
	for {
		var err error
		res, err = backend.ReadThreadTurn(ctx, s.URL, s.PendingPrompt)
		if errors.Is(err, chatgpt.ErrCompletedEmpty) {
			if err := h.rejectCompleted(s, requestFingerprint(*pending)); err != nil {
				return chatgpt.TurnResult{}, nil, false
			}
			log.Printf("[openai] session=%s bound completed empty final rejected; no calls delivered", id)
			return chatgpt.TurnResult{}, nil, true
		}
		if err == nil {
			return res, nil, true
		}
		var mismatch *chatgpt.UserBubbleMismatchError
		if errors.As(err, &mismatch) {
			log.Printf("[openai] session=%s adoption impossible: %v", id, err)
			return chatgpt.TurnResult{}, mismatch, false
		}
		if deterministic(err) {
			log.Printf("[openai] session=%s adoption impossible: %v", id, err)
			return chatgpt.TurnResult{}, nil, false
		}
		if time.Now().After(deadline) {
			log.Printf("[openai] session=%s adoption timed out waiting for stored answer: %v", id, err)
			return chatgpt.TurnResult{}, nil, false
		}
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return chatgpt.TurnResult{}, nil, false
		case <-timer.C:
		}
	}
}

// tryAdoptInterrupted attempts bounded read-only recovery of an interrupted
// final turn: it waits for the stored thread's answer to complete, validates
// it against the pending prompt, and caches it exactly like a normal
// completion (stable IDs from the pending transcript). On success it stores
// the adopted transcript (Dirty and Phase cleared) and returns true; the
// caller then falls through to normal request processing, so exact retries
// hit the duplicate cache. On any failure it returns false and the caller
// keeps terminal uncertainty. It never submits, never executes tools, and never clears an
// ambiguous journal.
// Adoption intentionally remains read-only: it never initiates a format repair.
// A stored repair prompt is adopted against the original client request and
// the durable RepairFingerprint budget is retained, so restart cannot re-ask.
//
// The returned mismatch is non-nil only when adoption failed structurally
// (typed user-bubble divergence with span evidence); the caller offers it to
// tryAutoResync before giving up with a 409.
func (h *handler) tryAdoptInterrupted(r *http.Request, id string, s *session) (bool, *chatgpt.UserBubbleMismatchError) {
	pending := s.PendingRequest
	res, mismatch, adopted := h.readPendingTurn(r, id, s)
	if mismatch != nil || !adopted {
		return false, mismatch
	}
	if pending == nil {
		return false, nil
	}
	out, err := parseAnswer(res.Answer, *pending)
	if err != nil {
		if err := h.rejectCompleted(s, requestFingerprint(*pending)); err != nil {
			return false, nil
		}
		log.Printf("[openai] session=%s completed stored output rejected; no calls delivered", id)
		return true, nil
	}
	if out.Text == "" && len(out.Calls) == 0 {
		log.Printf("[openai] session=%s adopted answer empty", id)
		return false, nil
	}
	out.Reasoning = res.Reasoning
	b, _ := json.Marshal(struct {
		Messages []message
		Tools    []toolDef
		Choice   any
	}{pending.Messages, pending.Tools, pending.ToolChoice})
	fp := fmt.Sprintf("%x", sha256.Sum256(b))
	out.ID, out.Created = fmt.Sprintf("chatcmpl-%x", sha256.Sum256([]byte(id+":"+fp))), time.Now().Unix()
	for i := range out.Calls {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d", id, fp, i)))
		out.Calls[i].ID = fmt.Sprintf("call_%x", sum[:12])
	}
	old := *s
	s.Messages = append(append([]message{}, pending.Messages...), message{Role: "assistant", Content: out.Text, ToolCalls: out.Calls})
	s.Fingerprint, s.Result, s.Dirty, s.Phase = fp, out, false, ""
	s.ToolsFP = toolsFingerprint(pending.Tools)
	s.PendingRequest, s.PendingPrompt = nil, ""
	s.RepairPendingPrompt = ""
	if err := h.save(); err != nil {
		*s = old // retain the original pending binding if adoption was not durable
		return false, nil
	}
	log.Printf("[openai] session=%s adopted interrupted final turn tool_calls=%d answer_bytes=%d", id, len(out.Calls), len(res.Answer))
	return true, nil
}

// A finished, bound response that failed strict validation cannot have delivered
// partial calls. Retain its fingerprint so exact retries never resubmit it.
func (h *handler) rejectCompleted(s *session, fingerprint string) error {
	old := *s
	s.Dirty, s.Phase, s.Fingerprint, s.Result = false, "invalid", fingerprint, result{}
	s.RejectedFingerprint = fingerprint
	if err := h.save(); err != nil {
		*s = old
		return err
	}
	return nil
}

func requestFingerprint(req completionsRequest) string {
	b, _ := json.Marshal(struct {
		Messages []message
		Tools    []toolDef
		Choice   any
	}{req.Messages, req.Tools, req.ToolChoice})
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

// Recovery never emits executable calls, never submits, and never clears an
// ambiguous journal. The client must retry its exact request to use the cache.
func (h *handler) recoverSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		apiError(w, 405, errors.New("POST required"))
		return
	}
	id, _, err := completionSessionID(r)
	if err != nil {
		apiError(w, 400, err)
		return
	}
	if id == "" {
		apiError(w, 400, errors.New("session header required"))
		return
	}
	var supplied *struct {
		completionsRequest
		ThreadURL string `json:"thread_url,omitempty"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20)).Decode(&supplied); err != nil {
			apiError(w, 400, err)
			return
		}
	}
	if !h.acquire(r.Context()) {
		return
	}
	defer h.release()
	unlock, err := h.lockTurn(r.Context())
	if err != nil {
		log.Printf("[openai] turn lock: %v", err)
		apiError(w, http.StatusServiceUnavailable, err)
		return
	}
	defer unlock()
	s := h.sessions[id]
	if s.incompleteJournal() {
		incompleteJournalError(w)
		return
	}
	if s != nil && s.Phase == "invalid" {
		apiError(w, 422, errors.New("completed output was rejected; no tool calls were delivered; continue with a distinct new ordinary user message at the end of the authoritative transcript"))
		return
	}
	if s == nil || !s.Dirty {
		apiError(w, 409, fmt.Errorf("no uncertain stored thread for this session; %s", h.sentDiagnosis(id, s)))
		return
	}
	if s.Phase == "replay" {
		apiError(w, 409, errors.New("only inert history import was interrupted; resume the completion request to continue replay; there is no submitted final response to recover"))
		return
	}
	req := s.PendingRequest
	if supplied != nil && requestFingerprint(supplied.completionsRequest) != requestFingerprint(*req) {
		apiError(w, 409, fmt.Errorf("supplied request does not match the exact original pending request; %s", h.sentDiagnosis(id, s)))
		return
	}
	threadURL := s.URL
	if supplied != nil && supplied.ThreadURL != "" {
		if !regexp.MustCompile(`^https://chatgpt\.com/(?:g/g-p-[a-zA-Z0-9-]+/)?c/[a-zA-Z0-9-]+$`).MatchString(supplied.ThreadURL) || (s.URL != "" && s.URL != supplied.ThreadURL) {
			apiError(w, 422, errors.New("recovery thread_url must be an exact ChatGPT conversation URL and cannot replace an existing journal target"))
			return
		}
		threadURL = supplied.ThreadURL
	}
	if threadURL == "" {
		apiError(w, 409, fmt.Errorf("pending turn has no saved thread URL; supply a candidate thread_url with the exact pending request for read-only full-prompt validation; %s", h.sentDiagnosis(id, s)))
		return
	}
	prompt := s.PendingPrompt
	backend, ok := h.client.(recoveryBackend)
	if !ok {
		apiError(w, 503, errors.New("backend cannot inspect stored responses"))
		return
	}
	res, err := backend.ReadThreadTurn(r.Context(), threadURL, prompt)
	if errors.Is(err, chatgpt.ErrCompletedEmpty) {
		old := *s
		s.URL = threadURL // completed-empty proof includes exact pending binding
		s.PendingRequest, s.PendingPrompt = req, prompt
		if serr := h.rejectCompleted(s, requestFingerprint(*req)); serr != nil {
			*s = old
			apiError(w, 500, serr)
			return
		}
		apiError(w, 422, err)
		return
	}
	if err != nil {
		// Before giving up with a 409, attempt the recognized clear-cut
		// shape: structural adoption failure plus a verifiably complete
		// thread answer that the sent-log ties to this session.
		var mismatch *chatgpt.UserBubbleMismatchError
		if s.URL != "" && errors.As(err, &mismatch) && h.tryAutoResync(r, id, s, mismatch) {
			out := s.Result
			writeJSON(w, 200, map[string]any{"recovered": true, "session": id, "cached_tool_calls": len(out.Calls), "auto_resync": s.AutoResyncEvidence, "next": "retry the exact original completion request to receive the validated cache; transplanted calls were recorded, never executed by recovery"})
			return
		}
		apiError(w, 409, fmt.Errorf("%w; %s", err, h.sentDiagnosis(id, s)))
		return
	}
	out, err := parseAnswer(res.Answer, *req)
	if err != nil {
		// Retain the bound original request for the new-user continuation gate.
		old := *s
		s.URL = threadURL // source-bound answer was received, even if invalid
		s.PendingRequest, s.PendingPrompt = req, prompt
		if serr := h.rejectCompleted(s, requestFingerprint(*req)); serr != nil {
			*s = old
			apiError(w, 500, serr)
			return
		}
		apiError(w, 422, errors.New("completed stored output failed strict protocol validation; no tool calls were delivered; continue with a new ordinary user message"))
		return
	}
	if out.Text == "" && len(out.Calls) == 0 {
		apiError(w, 409, fmt.Errorf("empty stored response; %s", h.sentDiagnosis(id, s)))
		return
	}
	out.Reasoning = res.Reasoning
	b, _ := json.Marshal(struct {
		Messages []message
		Tools    []toolDef
		Choice   any
	}{req.Messages, req.Tools, req.ToolChoice})
	fp := fmt.Sprintf("%x", sha256.Sum256(b))
	out.ID, out.Created = fmt.Sprintf("chatcmpl-%x", sha256.Sum256([]byte(id+":"+fp))), time.Now().Unix()
	for i := range out.Calls {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d", id, fp, i)))
		out.Calls[i].ID = fmt.Sprintf("call_%x", sum[:12])
	}
	recovered := *s
	recovered.URL = threadURL
	recovered.Messages = append(append([]message{}, req.Messages...), message{Role: "assistant", Content: out.Text, ToolCalls: out.Calls})
	recovered.Fingerprint, recovered.Result, recovered.Dirty, recovered.Phase = fp, out, false, ""
	recovered.ToolsFP = toolsFingerprint(req.Tools)
	recovered.PendingRequest, recovered.PendingPrompt = nil, ""
	h.sessions[id] = &recovered
	if err := h.save(); err != nil {
		h.sessions[id] = s
		apiError(w, 500, err)
		return
	}
	h.appendSent(sentEntry{Event: "recovered", Session: id, URL: recovered.URL, PromptHash: promptHash(prompt), Bytes: len(prompt), AnswerHash: promptHash(res.Answer), Note: "read-only exact-source validation; no submission or execution"})
	writeJSON(w, 200, map[string]any{"recovered": true, "session": id, "cached_tool_calls": len(out.Calls), "response_bytes": len(res.Answer), "next": "retry the exact original completion request to receive the validated cache; no tools were executed by recovery"})
}
