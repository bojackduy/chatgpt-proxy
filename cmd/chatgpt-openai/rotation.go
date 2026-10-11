package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"chatgpt-proxy/internal/chatgpt"
)

// Project-per-session (flag-gated, default off): 1 OpenCode session gets 1
// dedicated ChatGPT project, and prefix mismatches ROTATE (fresh working chat
// in the session's project seeded with a compact context card + the last few
// turns verbatim) instead of reset-replaying the full transcript. Old chats
// stay in the project for project-level retrieval. Any project/chat/creation
// failure falls back to today's shared-project thread behavior for that turn.
//
// The flag is CHATGPT_BRIDGE_PROJECT_PER_SESSION=1. When it is unset (or any
// other value), every path below is skipped and behavior is byte-identical to
// the legacy shared-project flow.

// projectPerSessionEnabled reports whether per-session projects are opted in.
func projectPerSessionEnabled() bool {
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv("CHATGPT_BRIDGE_PROJECT_PER_SESSION"))); v {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// sessionShort8 is the first 8 alphanumeric runes of a session ID, used in
// project display names. Stateless requests ("") yield "".
func sessionShort8(id string) string {
	var b strings.Builder
	for _, r := range id {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			if utf8.RuneCountInString(b.String()) >= 8 {
				break
			}
		}
	}
	return b.String()
}

var absPathRe = regexp.MustCompile(`/[A-Za-z0-9._~+~-]+(?:/[A-Za-z0-9._~+~-]+)+`)

// junkPathBases are last path segments that carry no repo identity (system
// temp dirs, home roots, the proxy's own state). Best effort only: this feeds
// a display name, never routing.
var junkPathBases = map[string]bool{
	"tmp": true, "temp": true, "var": true, "folders": true, "private": true,
	"Users": true, "home": true, "root": true, "etc": true, "usr": true,
	"chatgpt-proxy": true, ".chatgpt-proxy": true, "T": true,
}

// detectRepoBasename scans system messages for the most frequent absolute
// path and returns its sanitized last segment. Returns "" when nothing
// usable is found; the caller then names the project from the session ID.
func detectRepoBasename(msgs []message) string {
	counts := map[string]int{}
	best, bestN := "", 0
	for _, m := range msgs {
		if m.Role != "system" {
			continue
		}
		for _, p := range absPathRe.FindAllString(contentText(m), -1) {
			counts[p]++
			if counts[p] > bestN {
				best, bestN = p, counts[p]
			}
		}
	}
	if best == "" {
		return ""
	}
	base := path.Base(best)
	if junkPathBases[base] {
		return ""
	}
	var b strings.Builder
	for _, r := range base {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), ".-")
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

// sessionProjectName derives the dedicated project display name:
// `OpenCode Bridge — <repo-basename>/<ses-short8>`, or with the session
// short ID alone when no repo is detectable in the request context.
func sessionProjectName(id string, req completionsRequest) string {
	short := sessionShort8(id)
	if short == "" {
		short = "bridge"
	}
	if repo := detectRepoBasename(req.Messages); repo != "" {
		return "OpenCode Bridge — " + repo + "/" + short
	}
	return "OpenCode Bridge — " + short
}

// truncateRunes shortens s to at most n runes, marking the cut.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n]) + "…"
}

// rotationRecentN is how many trailing transcript messages go into the new
// working chat verbatim alongside the context card.
const rotationRecentN = 6

const rotationIntro = "BRIDGE SESSION ROTATION\nThis turn starts a fresh working chat inside this session's dedicated project. What follows is a compact context card written locally from stored adapter state (no model was consulted) plus the most recent turns verbatim. Older history remains in this project's retired chats and may be referenced from there. Reply to the current request below; do not replay history.\n"

// buildRotationPrompt composes the single first-turn prompt for a rotated
// working chat deterministically from the adapter's own Messages state: the
// session goal (first stored user message), recent assistant conclusions,
// digested tool results, and the last ~6 incoming turns verbatim. No model
// calls are involved.
func buildRotationPrompt(req completionsRequest, s *session, head string) string {
	goal := ""
	for _, m := range s.Messages {
		if m.Role == "user" {
			goal = truncateRunes(strings.TrimSpace(contentText(m)), 800)
			break
		}
	}
	if goal == "" {
		for _, m := range req.Messages {
			if m.Role == "user" {
				goal = truncateRunes(strings.TrimSpace(contentText(m)), 800)
				break
			}
		}
	}
	var notes []string
	for i := len(s.Messages) - 1; i >= 0 && len(notes) < 5; i-- {
		m := s.Messages[i]
		if m.Role == "assistant" && len(m.ToolCalls) == 0 && strings.TrimSpace(contentText(m)) != "" {
			notes = append([]string{"- " + truncateRunes(strings.TrimSpace(contentText(m)), 300)}, notes...)
		}
	}
	var digest []string
	results := map[string]string{}
	for _, m := range s.Messages {
		if m.Role == "tool" && m.ToolCallID != "" && results[m.ToolCallID] == "" {
			results[m.ToolCallID] = strings.TrimSpace(contentText(m))
		}
	}
	for _, m := range s.Messages {
		if m.Role != "assistant" {
			continue
		}
		for _, call := range m.ToolCalls {
			if len(digest) >= 12 {
				break
			}
			line := fmt.Sprintf("- call %s (args %d bytes)", call.Function.Name, len(call.Function.Arguments))
			if res, ok := results[call.ID]; ok {
				line += fmt.Sprintf(" -> result %d bytes: %s", len(res), truncateRunes(res, 200))
			}
			if len(strings.Join(digest, "\n"))+len(line) > 3000 {
				break
			}
			digest = append(digest, line)
		}
	}
	recent := req.Messages
	if len(recent) > rotationRecentN {
		recent = recent[len(recent)-rotationRecentN:]
	}
	var b strings.Builder
	b.WriteString(head)
	b.WriteString(rotationIntro)
	b.WriteString("CONTEXT CARD\nGoal: ")
	if goal == "" {
		b.WriteString("(unknown)")
	} else {
		b.WriteString(goal)
	}
	b.WriteString("\n")
	if len(notes) > 0 {
		b.WriteString("Recent assistant conclusions:\n" + strings.Join(notes, "\n") + "\n")
	}
	if len(digest) > 0 {
		b.WriteString("Tool digest:\n" + strings.Join(digest, "\n") + "\n")
	}
	b.WriteString("END CONTEXT CARD\n\nRECENT TURNS (verbatim, last few):\n")
	for _, m := range recent {
		b.WriteString(renderMsg(m) + "\n")
	}
	b.WriteString("END RECENT TURNS\n")
	b.WriteString(answerInstruction(req))
	return b.String()
}

// serveRotation attempts a project-per-session rotation for a prefix-mismatched
// stateful session: a fresh working chat in the session's dedicated project
// seeded with one context-card turn. It returns true when it wrote the HTTP
// response (success or terminal model-protocol failure). On ANY project/chat
// creation or submission failure it restores the session untouched and
// returns false so the caller falls back to the legacy shared-project
// reset-replay for that turn — never fail-closed, never 409 where the old
// path would have proceeded.
func (h *handler) serveRotation(w http.ResponseWriter, r *http.Request, id, lane string, req completionsRequest, s *session) bool {
	snapshot := *s
	name := sessionProjectName(id, req)
	projectURL := s.Project
	toolsFP := toolsFingerprint(req.Tools)
	prompt := buildRotationPrompt(req, s, toolPrompt(req))
	if h.maxChars > 0 && len(prompt) > h.maxChars {
		log.Printf("[openai] session=%s lane=%s rotation_fallback reason=card_over_budget bytes=%d budget=%d", id, lane, len(prompt), h.maxChars)
		return false
	}
	recent := req.Messages
	if len(recent) > rotationRecentN {
		recent = recent[len(recent)-rotationRecentN:]
	}
	var atts []chatgpt.Attachment
	for _, m := range recent {
		atts = append(atts, attachmentsOf(m)...)
	}
	restore := func() {
		*s = snapshot
		_ = h.save()
	}
	// Journal uncertainty before submission, exactly like the normal path.
	h.sessions[id] = s
	s.Dirty = true
	s.Phase = "final"
	s.PendingRequest, s.PendingPrompt = &req, prompt
	if err := h.save(); err != nil {
		h.backendError(w, err)
		return true
	}
	thinking := h.forceThinking || (req.ReasoningEffort != nil && req.ReasoningEffort != "none") || (req.Reasoning != nil && req.Reasoning != false)
	log.Printf("[openai] session=%s lane=%s decision=rotation project=%q thread=%s", id, lane, name, projectURL)
	res, err := h.client.AskThreadTurn(r.Context(), chatgpt.TurnOptions{
		Prompt: prompt, URL: "", ProjectURL: projectURL, ProjectName: name,
		Attachments: atts, Thinking: thinking,
	})
	if err != nil {
		if res.URL != "" || !chatgpt.IsNavigateError(err) {
			s.URL = res.URL
			if serr := h.save(); serr != nil {
				h.backendError(w, serr)
				return true
			}
			h.backendError(w, err)
			return true // submission may have happened: no automatic reset-replay
		}
		log.Printf("[openai] session=%s lane=%s rotation_fallback reason=pre_submission_navigation; using shared-project reset-replay", id, lane)
		restore()
		return false
	}
	if res.URL == "" || !strings.Contains(res.URL, "/c/") {
		s.URL = ""
		if serr := h.save(); serr != nil {
			h.backendError(w, serr)
			return true
		}
		h.backendError(w, errors.New("rotation returned no persistent thread URL; submission cannot be safely replayed"))
		return true
	}
	b, _ := json.Marshal(struct {
		Messages []message
		Tools    []toolDef
		Choice   any
	}{req.Messages, req.Tools, req.ToolChoice})
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(b))
	out, err := parseAnswer(res.Answer, req)
	if err != nil {
		// Model-protocol failure, not infra: behave exactly like the legacy
		// parse-error path (persist the thread, terminal 422) rather than resubmitting.
		s.URL = res.URL
		if res.ProjectURL != "" {
			s.Project = res.ProjectURL
		} else if projectURL != "" {
			s.Project = projectURL
		}
		if serr := h.rejectCompleted(s, fingerprint); serr != nil {
			h.backendError(w, serr)
			return true
		}
		log.Printf("[openai] invalid tool protocol session=%s lane=%s decision=rotation thread=%s", id, lane, res.URL)
		apiError(w, 422, errors.New("completed model output failed strict tool protocol validation; no tool calls were delivered; send a new ordinary user message"))
		return true
	}
	out.Reasoning = res.Reasoning
	out.ID, out.Created = fmt.Sprintf("chatcmpl-%x", sha256.Sum256([]byte(id+":"+fingerprint))), time.Now().Unix()
	for i := range out.Calls {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d", id, fingerprint, i)))
		out.Calls[i].ID = fmt.Sprintf("call_%x", sum[:12])
	}
	s.Messages = append(append([]message{}, req.Messages...), message{Role: "assistant", Content: out.Text, ToolCalls: out.Calls})
	s.URL = res.URL
	if res.ProjectURL != "" {
		s.Project = res.ProjectURL
	} else if projectURL != "" {
		s.Project = projectURL
	}
	s.Fingerprint, s.Result, s.Dirty = fingerprint, out, false
	s.ToolsFP = toolsFP
	s.PendingRequest, s.PendingPrompt, s.Phase = nil, "", ""
	if err := h.save(); err != nil {
		s.Dirty = true
		h.backendError(w, err)
		return true
	}
	log.Printf("[openai] session=%s lane=%s done decision=rotation tool_calls=%d answer_bytes=%d thread=%s project=%s", id, lane, len(out.Calls), len(res.Answer), res.URL, s.Project)
	h.respond(w, req, out)
	return true
}

// eagerThreadTurn submits one turn, starting a new session's FIRST thread in
// its own project when the flag is on (decision=fresh, no stored thread yet)
// instead of the shared project. serveRotation still owns the mismatch path;
// this covers fresh only, so a session lives in its own project from turn
// one. Delta turns and all legacy paths pass through untouched. On mint
// failure before any conversation exists (provably nothing submitted) it
// falls back to the shared project once rather than failing closed. A minted
// project URL is stored on s for the caller to persist with its normal save.
func (h *handler) eagerThreadTurn(r *http.Request, id, lane string, req completionsRequest, s *session, t turn, page string, thinking bool, decision string, resync bool) (chatgpt.TurnResult, error) {
	opt := chatgpt.TurnOptions{Prompt: t.prompt, URL: page, Attachments: t.atts, Thinking: thinking}
	eager := decision == "fresh" && page == "" && id != "" && !resync && projectPerSessionEnabled()
	if eager {
		if name := sessionProjectName(id, req); name != "" {
			opt.ProjectURL, opt.ProjectName = s.Project, name
		} else {
			eager = false
		}
	}
	res, err := h.client.AskThreadTurn(r.Context(), opt)
	if err != nil && eager && res.URL == "" && s.URL == "" && chatgpt.IsNavigateError(err) {
		log.Printf("[openai] session=%s lane=%s eager_fallback reason=%v; using shared project", id, lane, err)
		eager = false
		res, err = h.client.AskThreadTurn(r.Context(), chatgpt.TurnOptions{Prompt: t.prompt, URL: page, Attachments: t.atts, Thinking: thinking})
	}
	if err == nil && eager && res.ProjectURL != "" {
		s.Project = res.ProjectURL
		log.Printf("[openai] session=%s lane=%s decision=fresh-project project=%s thread=%s", id, lane, res.ProjectURL, res.URL)
	}
	return res, err
}
