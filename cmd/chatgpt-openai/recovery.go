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
	"strings"
	"time"
)

type recoveryBackend interface {
	ReadThreadTurn(context.Context, string, string) (chatgpt.TurnResult, error)
}

// tryAdoptInterrupted attempts bounded read-only recovery of an interrupted
// final turn: it waits for the stored thread's answer to complete, validates
// it against the pending prompt, and caches it exactly like a normal
// completion (stable IDs from the pending transcript). On success it stores
// the adopted transcript (Dirty and Phase cleared) and returns true; the
// caller then falls through to normal request processing, so exact retries
// hit the duplicate cache. On any failure it returns false and the caller
// keeps the 409. It never submits, never executes tools, and never clears an
// ambiguous journal.
func (h *handler) tryAdoptInterrupted(r *http.Request, id string, s *session) bool {
	pending := s.PendingRequest
	if pending == nil || s.PendingPrompt == "" || s.URL == "" {
		return false
	}
	backend, ok := h.client.(recoveryBackend)
	if !ok {
		return false
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
	var res chatgpt.TurnResult
	for {
		var err error
		res, err = backend.ReadThreadTurn(r.Context(), s.URL, s.PendingPrompt)
		if err == nil {
			break
		}
		if deterministic(err) {
			log.Printf("[openai] session=%s adoption impossible: %v", id, err)
			return false
		}
		if time.Now().After(deadline) {
			log.Printf("[openai] session=%s adoption timed out waiting for stored answer: %v", id, err)
			return false
		}
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-r.Context().Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
	out, err := parseAnswer(res.Answer, *pending)
	if err != nil || (out.Text == "" && len(out.Calls) == 0) {
		log.Printf("[openai] session=%s adopted answer unusable: %v", id, err)
		return false
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
	s.Messages = append(append([]message{}, pending.Messages...), message{Role: "assistant", Content: out.Text, ToolCalls: out.Calls})
	s.Fingerprint, s.Result, s.Dirty, s.Phase = fp, out, false, ""
	s.ToolsFP = toolsFingerprint(pending.Tools)
	s.PendingRequest, s.PendingPrompt = nil, ""
	if err := h.save(); err != nil {
		s.Dirty = true
		return false
	}
	log.Printf("[openai] session=%s adopted interrupted final turn tool_calls=%d answer_bytes=%d", id, len(out.Calls), len(res.Answer))
	return true
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
	var supplied *completionsRequest
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
	s := h.sessions[id]
	if s == nil || !s.Dirty || s.URL == "" {
		apiError(w, 409, errors.New("no uncertain stored thread for this session"))
		return
	}
	req := s.PendingRequest
	if req == nil {
		req = supplied
	}
	if req == nil || len(req.Messages) == 0 {
		apiError(w, 409, errors.New("legacy journal requires exact original completion request including tools"))
		return
	}
	prompt := s.PendingPrompt
	if s.PendingRequest == nil {
		if !prefix(req.Messages, s.Messages) || len(req.Messages) <= len(s.Messages) || toolsFingerprint(req.Tools) != s.ToolsFP {
			apiError(w, 409, errors.New("original history/tool set does not match the journal"))
			return
		}
		turns := h.buildTurns(*req, req.Messages[len(s.Messages):], toolReminder(*req))
		if len(turns) != 1 || turns[0].replay {
			apiError(w, 409, errors.New("legacy recovery requires one exactly correlated answer turn"))
			return
		}
		prompt = turns[0].prompt
	}
	backend, ok := h.client.(recoveryBackend)
	if !ok {
		apiError(w, 503, errors.New("backend cannot inspect stored responses"))
		return
	}
	res, err := backend.ReadThreadTurn(r.Context(), s.URL, prompt)
	if err != nil {
		apiError(w, 409, err)
		return
	}
	out, err := parseAnswer(res.Answer, *req)
	if err != nil {
		apiError(w, 409, err)
		return
	}
	if out.Text == "" && len(out.Calls) == 0 {
		apiError(w, 409, errors.New("empty stored response"))
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
	recovered.Messages = append(append([]message{}, req.Messages...), message{Role: "assistant", Content: out.Text, ToolCalls: out.Calls})
	recovered.Fingerprint, recovered.Result, recovered.Dirty = fp, out, false
	recovered.ToolsFP = toolsFingerprint(req.Tools)
	recovered.PendingRequest, recovered.PendingPrompt = nil, ""
	h.sessions[id] = &recovered
	if err := h.save(); err != nil {
		h.sessions[id] = s
		apiError(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"recovered": true, "session": id, "cached_tool_calls": len(out.Calls), "response_bytes": len(res.Answer), "next": "retry the exact original completion request to receive the validated cache; no tools were executed by recovery"})
}
