// chatgpt-openai translates ChatGPT Web replies into OpenAI chat completions.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"chatgpt-proxy/internal/chatgpt"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	addr := flag.String("addr", "127.0.0.1:8788", "listen address")
	port := flag.Int("chrome-port", 9222, "Chrome debugging port")
	profile := flag.String("profile", defaultProfile(), "Chrome profile")
	chrome := flag.String("chrome", "", "Chrome binary")
	background := flag.Bool("background", false, "minimize Chrome")
	model := flag.String("model", "chatgpt-web", "model ID")
	maxChars := flag.Int("max-chars", 200000, "maximum bytes per ChatGPT turn (chunk); long histories are replayed in chunks of this size, never one giant paste")
	thinking := flag.Bool("thinking", false, "always select the ChatGPT Thinking model (reasoning)")
	state := flag.String("state", filepath.Join(defaultProfile(), "openai-sessions.json"), "persistent session state")
	flag.Parse()
	cg := chatgpt.New(*port, *profile, *chrome)
	cg.Background, cg.DedicatedTab = *background, true
	defer cg.Close()
	h := &handler{client: cg, model: *model, maxChars: *maxChars, forceThinking: *thinking, statePath: *state, profileDir: *profile, gate: make(chan struct{}, 1)}
	if err := h.load(); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Addr: *addr, Handler: h.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	log.Printf("[openai] listening=%s model=%s dedicated_tab=true state=%s chrome_port=%d", *addr, *model, *state, *port)
	log.Print("[openai] REPAIR: login in ~/.chatgpt-proxy Chrome; CDP: localhost:9222/json/version; DOM selectors: internal/chatgpt/chatgpt.go; restart: launchctl kickstart -k gui/$(id -u)/com.chatgpt-proxy.openai")
	err := srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func defaultProfile() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".chatgpt-proxy")
}

type toolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}
type toolDef struct {
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}
type callFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}
type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function callFunction `json:"function"`
}
type message struct {
	Role       string     `json:"role"`
	Content    any        `json:"content"`
	Name       string     `json:"name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
}
type completionsRequest struct {
	Model           string    `json:"model"`
	Messages        []message `json:"messages"`
	Stream          bool      `json:"stream"`
	Tools           []toolDef `json:"tools"`
	ToolChoice      any       `json:"tool_choice"`
	ReasoningEffort any       `json:"reasoning_effort"`
	Reasoning       any       `json:"reasoning"`
}
type backend interface {
	AskThreadTurn(context.Context, chatgpt.TurnOptions) (chatgpt.TurnResult, error)
	LoginState(context.Context) (*chatgpt.Status, error)
}
type result struct {
	ID        string
	Created   int64
	Text      string
	Reasoning string
	Calls     []toolCall
}
type session struct {
	URL                 string
	Messages            []message
	Fingerprint         string
	RejectedFingerprint string `json:",omitempty"`
	Result              result
	Dirty               bool // an interrupted/failed submitted turn must not be silently retried
	// ToolsFP is the fingerprint of the tool set whose full schemas were last
	// taught to this thread. The thread holds them server-side afterwards, so
	// later turns only need a compact reminder (see toolReminder).
	ToolsFP        string
	PendingRequest *completionsRequest `json:",omitempty"`
	PendingPrompt  string              `json:",omitempty"`
	// Phase records where an in-flight turn was when it failed, so recovery
	// can distinguish "nothing executable happened" from genuine uncertainty:
	// "replay" means only inert history-import (ACK) turns were in flight and
	// the final answer-seeking turn was never submitted — safe to resume;
	// "final" means the answer-seeking turn may have executed with unknown
	// outcome — requires read-only adoption or 409. "" (legacy/unknown) is
	// treated as "final", the conservative choice.
	// "invalid" means a finished output failed strict validation and no calls
	// were delivered; exact retries fail without resubmitting.
	Phase          string    `json:",omitempty"`
	ResyncMessages []message `json:",omitempty"`
	ResyncFrom     string    `json:",omitempty"`
	// Project is the session's dedicated ChatGPT project URL, set only when
	// the CHATGPT_BRIDGE_PROJECT_PER_SESSION rotation minted one. Empty means
	// the grandfathered shared project (today's behavior, unchanged).
	Project string `json:",omitempty"`
}
type handler struct {
	client        backend
	model         string
	maxChars      int
	statePath     string
	profileDir    string // Chrome profile dir; scopes the cross-process turn lock
	forceThinking bool
	gate          chan struct{} // serializes browser transactions and all state access; cancellation-aware
	sessions      map[string]*session
}

func (h *handler) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"object": "list", "data": []any{map[string]any{"id": h.model, "object": "model", "owned_by": "chatgpt-web"}}})
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", h.readyz)
	mux.HandleFunc("/v1/chat/completions", h.completions)
	mux.HandleFunc("/v1/sessions/recover", h.recoverSession)
	mux.HandleFunc("/v1/sessions/resync", h.resyncSession)
	return mux
}
func (h *handler) acquire(ctx context.Context) bool {
	select {
	case h.gate <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}
func (h *handler) release() { <-h.gate }
func (h *handler) load() error {
	h.sessions = map[string]*session{}
	if h.statePath == "" {
		return nil
	}
	b, err := os.ReadFile(h.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, &h.sessions)
}
func (h *handler) save() error {
	if h.statePath == "" {
		return nil
	}
	b, err := json.Marshal(h.sessions)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(h.statePath), 0700); err != nil {
		return err
	}
	tmp := h.statePath + ".tmp"
	if err = os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, h.statePath)
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func apiError(w http.ResponseWriter, status int, err error) {
	kind := "proxy_error"
	if status == http.StatusUnprocessableEntity {
		kind = "invalid_request_error"
	}
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": err.Error(), "type": kind, "code": status}})
}
func (h *handler) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if !h.acquire(ctx) {
		apiError(w, 503, ctx.Err())
		return
	}
	defer h.release()
	st, err := h.client.LoginState(ctx) // never navigate a live thread
	if err != nil {
		log.Printf("[openai] NOT READY: %v; REPAIR CDP/profile/login/DOM", err)
		apiError(w, 503, err)
		return
	}
	ready := st.LoggedIn && st.Composer
	status := 200
	if !ready {
		status = 503
		log.Printf("[openai] NOT READY login=%v composer=%v url=%s; REPAIR login/DOM", st.LoggedIn, st.Composer, st.URL)
	}
	writeJSON(w, status, map[string]any{"ready": ready, "status": st})
}
func contentText(m message) string {
	switch c := m.Content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, p := range c {
			if obj, ok := p.(map[string]any); ok && obj["type"] == "text" {
				if s, ok := obj["text"].(string); ok {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}
func renderMsg(m message) string {
	if parts, ok := m.Content.([]any); ok {
		out := make([]any, 0, len(parts))
		for _, p := range parts {
			obj, _ := p.(map[string]any)
			if obj != nil {
				if t, _ := obj["type"].(string); t == "image_url" || t == "video_url" {
					if a, err := attachmentFromPart(obj); err == nil {
						out = append(out, map[string]any{"type": t, "note": fmt.Sprintf("attached to this ChatGPT turn: %s; use the attachment to answer", a.MIME)})
						continue
					}
				}
			}
			out = append(out, p)
		}
		clone := m
		clone.Content = out
		b, _ := json.Marshal(clone)
		return string(b) // JSON preserves tool IDs, roles and untrusted text boundaries
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// dataURLParts decodes a data:<mime>;base64,<data> URL.
func dataURLParts(u string) (string, []byte, bool) {
	if !strings.HasPrefix(u, "data:") {
		return "", nil, false
	}
	rest := u[len("data:"):]
	i := strings.Index(rest, ";base64,")
	if i < 0 {
		return "", nil, false
	}
	mime := rest[:i]
	data, err := base64.StdEncoding.DecodeString(rest[i+len(";base64,"):])
	if err != nil {
		return "", nil, false
	}
	return mime, data, true
}

// attachmentFromPart validates and decodes one image_url/video_url content part.
func attachmentFromPart(obj map[string]any) (chatgpt.Attachment, error) {
	t, _ := obj["type"].(string)
	holder, _ := obj[t].(map[string]any)
	if holder == nil {
		return chatgpt.Attachment{}, errors.New("attachment part missing payload")
	}
	u, _ := holder["url"].(string)
	mime, data, ok := dataURLParts(u)
	if !ok {
		return chatgpt.Attachment{}, errors.New("only data: base64 image/video attachments are supported")
	}
	if !strings.HasPrefix(mime, "image/") && !strings.HasPrefix(mime, "video/") {
		return chatgpt.Attachment{}, errors.New("only image/* and video/* attachments are supported")
	}
	return chatgpt.Attachment{Name: "attachment", MIME: mime, Data: data}, nil
}

func attachmentsOf(m message) []chatgpt.Attachment {
	parts, ok := m.Content.([]any)
	if !ok {
		return nil
	}
	var out []chatgpt.Attachment
	for _, p := range parts {
		obj, _ := p.(map[string]any)
		if obj == nil {
			continue
		}
		if t, _ := obj["type"].(string); t == "image_url" || t == "video_url" {
			if a, err := attachmentFromPart(obj); err == nil {
				out = append(out, a)
			}
		}
	}
	return out
}
func prefix(a, b []message) bool {
	return firstMismatch(a, b) == nil
}

func canonicalMessages(msgs []message) []message {
	out := append([]message{}, msgs...)
	for i := range out {
		// Normalize equivalent SDK text encodings without dropping media or names.
		if out[i].Content == nil {
			out[i].Content = ""
		} else if parts, ok := out[i].Content.([]any); ok && len(parts) == 1 {
			if p, ok := parts[0].(map[string]any); ok && p["type"] == "text" && len(p) == 2 {
				out[i].Content = p["text"]
			}
		}
		// System prompts are regenerated by the client every turn with
		// volatile skill metadata (see normalizeSystem); history, tools and
		// media stay byte-exact.
		if out[i].Role == "system" {
			if s, ok := out[i].Content.(string); ok {
				out[i].Content = normalizeSystem(s)
			}
		}
		// Platform per-message metadata (fresh IDs per resume invocation)
		// is never user intent; genuine text around it stays exact.
		if s, ok := out[i].Content.(string); ok {
			if out[i].Role == "assistant" {
				s = dcpReminderRe.ReplaceAllString(s, "")
			}
			out[i].Content = dcpMessageIDRe.ReplaceAllString(s, "<dcp-message-id></dcp-message-id>")
		}
		out[i].ToolCalls = append([]toolCall(nil), out[i].ToolCalls...)
		for j := range out[i].ToolCalls {
			var v any
			decoder := json.NewDecoder(strings.NewReader(out[i].ToolCalls[j].Function.Arguments))
			decoder.UseNumber() // do not round distinct large integers into the same prefix
			if json.Valid([]byte(out[i].ToolCalls[j].Function.Arguments)) && decoder.Decode(&v) == nil {
				b, _ := json.Marshal(v)
				out[i].ToolCalls[j].Function.Arguments = string(b)
			}
		}
	}
	return out
}

// System-prompt volatility normalization. Proven on real captured OpenCode
// transcripts (two independent sessions, ~104KB system, 136 skill entries):
// the client regenerates its system message per CLI invocation with three
// volatile classes that carry no user intent and are never forwarded to the
// ChatGPT thread (the bridge only sends new messages):
//  1. <location> inside <skill> blocks: the same skill resolves via
//     different root spellings (~/.agents vs ~/.claude vs
//     ~/.config/opencode), sibling variant dirs (linux-ssi vs k8s-ssi),
//     or random trash/sync paths (.trash/<rand>, synced/<uuid>).
//  2. <description> inside <skill> blocks: observed only together with a
//     location change on the same entry (variant-dir content), 0-5/turn.
//  3. A conditional context-management ("compress") reminder block (~38
//     lines), present or absent depending on client context state.
//
// Normalization is confined to system-role messages and, within them, to
// skill blocks plus the reminder span. Skill names, agent prompt,
// environment, references, MCP instructions, user history, tools and media
// stay byte-exact, so genuine edits, agent/model switches, branches,
// compactions and truncations still mismatch. Anything unrecognized fails
// safe (no strip).
var (
	skillBlockRe       = regexp.MustCompile(`(?s)<skill>.*?</skill>`)
	skillLocationRe    = regexp.MustCompile(`<location>[^<]*</location>`)
	skillDescriptionRe = regexp.MustCompile(`(?s)<description>.*?</description>`)
	// The platform injects <dcp-message-id> per-message metadata on the
	// wire (user prompts gain a ~40-char suffix, tool results one tag
	// line). IDs are stable within one CLI process (m0022 == m0022) but
	// regenerated by every resume invocation (m0043), so exact comparison
	// would reset-replay every resumed turn. The platform marks the tag
	// do-not-output metadata; it is never user intent.
	dcpMessageIDRe = regexp.MustCompile(`<dcp-message-id>[^<]*</dcp-message-id>`)
	// Assistant wire messages additionally carry a platform envelope after
	// the answer: <dcp-system-reminder>…</dcp-system-reminder> prose plus a
	// trailing <dcp-message-id>. The bridge stores the bare answer text it
	// received, so without stripping, every resumed turn mismatches on the
	// first assistant message. Proven shape (3 live samples): answer, then
	// the reminder block, then exactly one trailing message-id tag at end
	// of string. Anything else stays exact.
	dcpReminderRe = regexp.MustCompile(`(?s)\n*<dcp-system-reminder>.*?</dcp-system-reminder>\n*<dcp-message-id>[^<]*</dcp-message-id>\s*$`)
)

func normalizeSystem(s string) string {
	s = skillBlockRe.ReplaceAllStringFunc(s, func(block string) string {
		block = skillLocationRe.ReplaceAllString(block, "<location></location>")
		return skillDescriptionRe.ReplaceAllString(block, "<description></description>")
	})
	return stripCompressReminder(s)
}

// stripCompressReminder drops the client's conditional context-management
// advisory (opener line through the </dcp-system-reminder> line) and one
// preceding blank line. Missing opener or closer leaves the text exact.
func stripCompressReminder(s string) string {
	const opener = "You operate in a context-constrained environment."
	const closer = "</dcp-system-reminder>"
	lines := strings.Split(s, "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, opener) {
			start = i
			break
		}
	}
	if start < 0 {
		return s
	}
	end := -1
	for i := start; i < len(lines); i++ {
		if strings.Contains(lines[i], closer) {
			end = i
			break
		}
	}
	if end < 0 {
		return s
	}
	if start > 0 && strings.TrimSpace(lines[start-1]) == "" {
		start--
	}
	return strings.Join(append(lines[:start], lines[end+1:]...), "\n")
}

const toolPreamble = "You are the text-generation backend of an external OpenCode client. The listed functions ARE available in that external client, not as native ChatGPT tools. You do NOT need native tool access. Your job is to output a textual request that the external client parses and executes. Do not claim the functions are unavailable; do not execute or simulate them yourself. To request execution, reply ONLY with one or more literal wrappers in exactly this format:\n<opencode_bridge_call>\n{\"name\":\"function_name\",\"arguments\":{\"arg\":\"value\"}}\n</opencode_bridge_call>\nDo not put wrappers inside Markdown fences. Arguments MUST be a JSON object. Use only listed function names and match their parameter schema. No explanatory text with tool calls. After receiving external tool results, answer or request another tool. When no tool is needed, reply normally.\nExternal client functions:\n"

func toolPrompt(req completionsRequest) string {
	if len(req.Tools) == 0 {
		return "No functions available. Do not emit opencode_bridge_call wrappers.\n"
	}
	b, _ := json.Marshal(req.Tools)
	return toolPreamble + string(b) + "\nTool choice for THIS turn: " + toolChoiceText(req.ToolChoice) + "\n"
}

// toolChoiceText renders req.ToolChoice the way the model must read it. A
// missing or JSON-null choice is AUTO (the OpenAI default): the model decides
// per turn. Pasting the raw `null` misleads it into refusing every call with
// "tool execution is disabled (tool choice: null)" — seen live — so auto is
// spelled out with explicit permission to call.
func toolChoiceText(choice any) string {
	if choice == nil {
		return "auto (call a function when it helps, otherwise answer directly)"
	}
	if s, ok := choice.(string); ok {
		switch strings.ToLower(s) {
		case "auto":
			return "auto (call a function when it helps, otherwise answer directly)"
		case "none":
			return "none (do NOT call any function this turn; answer directly)"
		case "required":
			return "required (you MUST output an opencode_bridge_call wrapper this turn)"
		}
		return s
	}
	if b, err := json.Marshal(choice); err == nil {
		return string(b) + " (you MUST call that named function via an opencode_bridge_call wrapper this turn)"
	}
	return "auto (call a function when it helps, otherwise answer directly)"
}

// toolReminder is the compact per-turn pointer used once a thread already
// knows the full schemas. Resending the whole dump every turn would exceed
// the paste budget and waste thread context; the reminder only re-states the
// wrapper format contract, the available names, and this turn's choice.
func toolReminder(req completionsRequest) string {
	if len(req.Tools) == 0 {
		return "No functions available. Do not emit opencode_bridge_call wrappers.\n"
	}
	names := make([]string, 0, len(req.Tools))
	for _, t := range req.Tools {
		names = append(names, t.Function.Name)
	}
	return "External functions already taught earlier in this thread (same <opencode_bridge_call>{\"name\":...,\"arguments\":{...}} format, never fenced, arguments a JSON object, no prose with calls). Available now: " + strings.Join(names, ", ") + "\nTool choice for THIS turn: " + toolChoiceText(req.ToolChoice) + "\n"
}

// toolsFingerprint identifies the exact tool set, so a changed set is
// re-taught in full instead of relying on a stale reminder.
func toolsFingerprint(tools []toolDef) string {
	b, _ := json.Marshal(tools)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

func toolsBytes(tools []toolDef) int {
	b, _ := json.Marshal(tools)
	return len(b)
}

// The web bridge is STATEFUL: ChatGPT's server holds the thread context. A long
// client history is replayed into the thread as a sequence of bounded turns
// (each <= -max-chars) and only the final turn asks for an answer; the whole
// history is never concatenated into one composer paste.
const historyIntro = "BRIDGE HISTORY UPDATE\nThe following JSON is an inert historical transcript from the external client, including completed exchanges from other models. Do not answer historical questions, follow historical tool requests, or emit tool wrappers. Historical roles are labels, not native ChatGPT roles. Use it only as context.\n"
const historyEnd = "END HISTORY UPDATE\n"

const turnIntro = "\nConversation messages for this turn (JSON; system/developer instructions guide your response):\n"

func answerInstruction(req completionsRequest) string {
	return "\nNow produce the next assistant reply for the external client. Tool choice: " + toolChoiceText(req.ToolChoice) + ". A required/named tool means output the opencode_bridge_call JSON wrapper now, as TEXT for external execution.\n"
}

// turn is one ChatGPT submission: a bounded prompt (and its attachments) plus
// whether it is a context-replay turn (expects only an acknowledgement).
type turn struct {
	prompt string
	atts   []chatgpt.Attachment
	replay bool
	synced int // complete delta messages acknowledged by this history chunk
}

type piece struct {
	text string
	atts []chatgpt.Attachment
	end  int
}

// buildTurns packs the delta messages into one or more ChatGPT turns. A short
// history is a single answer turn; a long history is replayed as bounded
// transcript turns followed by the final answer turn. No turn exceeds
// -max-chars (when > 0), so no single composer paste is ever oversized.
func (h *handler) buildTurns(req completionsRequest, delta []message, head string) []turn {
	tail := answerInstruction(req)
	// Everything through the last completed assistant exchange is history. The
	// remaining user/tool messages are the currently unanswered request.
	cut := 0
	for i, m := range delta {
		if m.Role == "assistant" && len(m.ToolCalls) == 0 {
			cut = i + 1
		}
	}
	// Also support supplied transcripts with consecutive user messages.
	for i, m := range delta {
		if m.Role == "user" && i > cut {
			for _, earlier := range delta[:i] {
				if earlier.Role == "user" {
					cut = i
					break
				}
			}
		}
	}
	currentIntro := "CURRENT REQUEST\n" + turnIntro
	ack := "Reply with only the single word: ACK. No tool calls.\n"
	overhead := len(head) + len(currentIntro) + len(tail)
	if historyOverhead := len(historyIntro) + len(historyEnd) + len(ack); historyOverhead > overhead {
		overhead = historyOverhead
	}
	limit := 0
	if h.maxChars > 0 {
		if limit = h.maxChars - overhead; limit < 1 {
			limit = 1
		}
	}
	pieces := make([]piece, 0, len(delta))
	for i, m := range delta {
		pieces = append(pieces, piece{text: renderMsg(m), atts: attachmentsOf(m), end: i + 1})
	}
	render := func(ps []piece) (string, []chatgpt.Attachment) {
		var b strings.Builder
		var atts []chatgpt.Attachment
		for _, p := range ps {
			b.WriteString(p.text)
			b.WriteString("\n")
			atts = append(atts, p.atts...)
		}
		return b.String(), atts
	}
	history, ha := render(pieces[:cut])
	current, ca := render(pieces[cut:])
	combined := head
	if history != "" {
		combined += historyIntro + history + historyEnd
	}
	combined += currentIntro + current + tail
	if h.maxChars <= 0 || len(combined) <= h.maxChars {
		return []turn{{prompt: combined, atts: append(ha, ca...)}}
	}
	var turns []turn
	// Keep history separate from the final request even when both are small
	// enough individually. Oversized current inputs retain chunked replay.
	for _, g := range packPieces(pieces[:cut], limit) {
		text, atts := render(g)
		turns = append(turns, turn{prompt: historyIntro + text + historyEnd + ack, atts: atts, replay: true, synced: g[len(g)-1].end})
	}
	groups := packPieces(pieces[cut:], limit)
	for i, g := range groups {
		text, atts := render(g)
		last := i == len(groups)-1
		p := historyIntro + text + historyEnd + ack
		if last {
			p = head + currentIntro + text + tail
		}
		turns = append(turns, turn{prompt: p, atts: atts, replay: !last, synced: g[len(g)-1].end})
	}
	return turns
}

// packPieces greedily groups transcript pieces under limit bytes, never merging
// a piece larger than a whole turn; such a piece is split at rune boundaries so
// the per-chunk bound still holds.
func packPieces(pieces []piece, limit int) [][]piece {
	if limit <= 0 {
		return [][]piece{pieces}
	}
	var out [][]piece
	var cur []piece
	curLen := 0
	flush := func() {
		if len(cur) > 0 {
			out = append(out, cur)
			cur, curLen = nil, 0
		}
	}
	for _, p := range pieces {
		if len(p.text)+1 <= limit {
			if len(cur) > 0 && curLen+len(p.text)+1 > limit {
				flush()
			}
			cur = append(cur, p)
			curLen += len(p.text) + 1
			continue
		}
		flush()
		head := p.atts
		for len(p.text)+1 > limit {
			seg := p.text[:safeCut(p.text, max(1, limit-1))]
			p.text = p.text[len(seg):]
			out = append(out, []piece{{text: seg, atts: head}})
			head = nil
		}
		if p.text != "" {
			cur = append(cur, piece{text: p.text, atts: head, end: p.end})
			curLen = len(p.text) + 1
		}
	}
	flush()
	return out
}

// safeCut returns a byte index <= n that does not split a UTF-8 rune.
func safeCut(s string, n int) int {
	if n >= len(s) {
		return len(s)
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	if n == 0 {
		_, n = utf8.DecodeRuneInString(s)
	}
	return n
}

// Accept only the known Markdown-escaped delimiter variant; JSON is never
// unescaped as Markdown. Both variants still require complete wrappers.
var toolBlock = regexp.MustCompile(`(?s)<opencode\\?_bridge\\?_call>\s*(.*?)\s*</opencode\\?_bridge\\?_call>`)
var toolMarker = regexp.MustCompile(`<\/?opencode\\?_bridge\\?_call`)
var toolName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

type localSchemas struct{}

func (localSchemas) Load(url string) (any, error) {
	return nil, fmt.Errorf("external schema references disabled: %s", url)
}
func compileTools(tools []toolDef) (map[string]*jsonschema.Schema, error) {
	out := map[string]*jsonschema.Schema{}
	for _, t := range tools {
		if t.Type != "function" || !toolName.MatchString(t.Function.Name) {
			return nil, errors.New("only named function tools supported")
		}
		if _, exists := out[t.Function.Name]; exists {
			return nil, errors.New("duplicate tool name")
		}
		compiler := jsonschema.NewCompiler()
		compiler.UseLoader(localSchemas{})
		// Normalize struct/map schemas to JSON values expected by the validator.
		b, err := json.Marshal(t.Function.Parameters)
		if err != nil {
			return nil, err
		}
		var schema any
		if err = json.Unmarshal(b, &schema); err != nil {
			return nil, err
		}
		if err = compiler.AddResource("https://proxy.local/tool.json", schema); err != nil {
			return nil, err
		}
		s, err := compiler.Compile("https://proxy.local/tool.json")
		if err != nil {
			return nil, fmt.Errorf("tool %s schema: %w", t.Function.Name, err)
		}
		out[t.Function.Name] = s
	}
	return out, nil
}

func parseAnswer(answer string, req completionsRequest) (result, error) {
	out := result{Text: strings.TrimSpace(answer)}
	allowed, err := compileTools(req.Tools)
	if err != nil {
		return result{}, err
	}
	choice, _ := req.ToolChoice.(string)
	named := ""
	if obj, ok := req.ToolChoice.(map[string]any); ok {
		if fn, ok := obj["function"].(map[string]any); ok {
			named, _ = fn["name"].(string)
		}
	}
	for i, m := range toolBlock.FindAllStringSubmatch(answer, -1) {
		var v struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(m[1]), &v); err != nil {
			return result{}, fmt.Errorf("malformed tool JSON: %w", err)
		}
		var args map[string]any
		if err := json.Unmarshal(v.Arguments, &args); err != nil || args == nil {
			return result{}, errors.New("tool arguments must be a JSON object")
		}
		if allowed[v.Name] == nil || choice == "none" || (named != "" && named != v.Name) {
			return result{}, fmt.Errorf("tool name %q is not allowed by tools/tool_choice", v.Name)
		}
		if err := allowed[v.Name].Validate(args); err != nil {
			return result{}, fmt.Errorf("tool %s arguments: %w", v.Name, err)
		}
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", answer, i)))
		out.Calls = append(out.Calls, toolCall{ID: fmt.Sprintf("call_%x", sum[:12]), Type: "function", Function: callFunction{Name: v.Name, Arguments: string(v.Arguments)}})
		out.Text = strings.Replace(out.Text, m[0], "", 1)
	}
	if toolMarker.MatchString(out.Text) {
		return result{}, errors.New("incomplete opencode_bridge_call wrapper")
	}
	out.Text = strings.TrimSpace(out.Text)
	if len(out.Calls) > 0 && out.Text != "" {
		return result{}, errors.New("bridge calls must not contain prose or Markdown fences")
	}
	if (choice == "required" || named != "") && len(out.Calls) == 0 {
		return result{}, errors.New("backend did not honor required tool_choice")
	}
	return out, nil
}

func sessionID(r *http.Request) string {
	for _, name := range []string{"X-Session-Id", "x-opencode-session", "x-session-affinity"} {
		if id := r.Header.Get(name); id != "" {
			return id
		}
	}
	return ""
}
func (h *handler) completions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		apiError(w, 405, errors.New("POST required"))
		return
	}
	var req completionsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20)).Decode(&req); err != nil {
		apiError(w, 400, err)
		return
	}
	if len(req.Messages) == 0 {
		apiError(w, 400, errors.New("messages required"))
		return
	}
	if req.Model != "" && req.Model != h.model {
		apiError(w, 400, errors.New("unknown model"))
		return
	}
	req.Model = h.model
	if _, err := compileTools(req.Tools); err != nil {
		apiError(w, 400, err)
		return
	}
	switch choice := req.ToolChoice.(type) {
	case nil:
	case string:
		if choice != "none" && choice != "auto" && choice != "required" {
			apiError(w, 400, errors.New("invalid tool_choice"))
			return
		}
	case map[string]any:
		fn, ok := choice["function"].(map[string]any)
		name, _ := fn["name"].(string)
		found := false
		for _, t := range req.Tools {
			if t.Function.Name == name {
				found = true
			}
		}
		if !ok || choice["type"] != "function" || !found {
			apiError(w, 400, errors.New("tool_choice must name a supplied function"))
			return
		}
	default:
		apiError(w, 400, errors.New("invalid tool_choice"))
		return
	}
	for _, m := range req.Messages {
		if parts, ok := m.Content.([]any); ok {
			for _, p := range parts {
				obj, ok := p.(map[string]any)
				if !ok {
					apiError(w, 400, errors.New("invalid content part"))
					return
				}
				switch obj["type"] {
				case "text":
				case "image_url", "video_url":
					if _, err := attachmentFromPart(obj); err != nil {
						apiError(w, 400, err)
						return
					}
				default:
					apiError(w, 400, errors.New("unsupported content part type"))
					return
				}
			}
		}
	}
	id, lane, err := completionSessionID(r)
	if err != nil {
		apiError(w, 400, err)
		return
	}
	if !h.acquire(r.Context()) {
		return
	}
	defer h.release()
	// Cross-process turn lock (after the in-process gate — always this
	// order): the MCP server drives the same Chrome from another process.
	unlock, err := chatgpt.LockTurn(r.Context(), h.profileDir)
	if err != nil {
		log.Printf("[openai] turn lock: %v", err)
		apiError(w, http.StatusServiceUnavailable, err)
		return
	}
	defer unlock()
	if h.sessions == nil {
		h.sessions = map[string]*session{}
	}
	s := h.sessions[id]
	if id == "" || s == nil {
		s = &session{}
	}
	b, _ := json.Marshal(struct {
		Messages []message
		Tools    []toolDef
		Choice   any
	}{req.Messages, req.Tools, req.ToolChoice})
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(b))
	resync := len(s.ResyncMessages) > 0
	if resync && (!prefix(req.Messages, s.ResyncMessages) || len(req.Messages) <= len(s.ResyncMessages) || req.Messages[len(s.ResyncMessages)].Role != "user") {
		mismatch := firstMismatch(req.Messages, s.ResyncMessages)
		var nextRole string
		if len(req.Messages) > len(s.ResyncMessages) {
			nextRole = req.Messages[len(s.ResyncMessages)].Role
		}
		if mismatch != nil {
			log.Printf("[openai] session=%s resync_mismatch index=%d field=%s incoming_messages=%d armed_messages=%d next_role=%q incoming_encoding=%s saved_encoding=%s incoming_shape=%s saved_shape=%s", id, mismatch.Index, mismatch.Field, len(req.Messages), len(s.ResyncMessages), nextRole, mismatch.IncomingEncoding, mismatch.SavedEncoding, mismatch.IncomingShape, mismatch.SavedShape)
		} else {
			log.Printf("[openai] session=%s resync_mismatch prefix_ok incoming_messages=%d armed_messages=%d next_role=%q", id, len(req.Messages), len(s.ResyncMessages), nextRole)
		}
		// Self-healing re-arm: the armed snapshot went stale (client moved on
		// while it sat unused). If the live transcript strictly extends the
		// last synced prefix, replace the snapshot with the live transcript
		// minus trailing new user messages, then proceed with this same
		// request. Strictly pre-submission: Dirty, URL, and thread state are
		// untouched, and every armed byte comes from the client's own
		// authoritative transcript, so nothing can be duplicated or executed.
		if h.rearmResyncFromLive(id, s, req) {
			log.Printf("[openai] session=%s resync_rearmed history=%d; proceeding", id, len(s.ResyncMessages))
		} else {
			apiError(w, 409, errors.New("resync requires the authoritative completed transcript followed by a new user message; do not retry the uncertain original request"))
			return
		}
	}
	if s.Dirty && !resync {
		if s.Phase == "replay" {
			// The final answer-seeking turn was never submitted and replay
			// chunks are inert history imports: the request can proceed and
			// the normal delta machinery resumes the import. Duplicating an
			// inert chunk is harmless; no tool turn can execute twice.
			log.Printf("[openai] session=%s resuming interrupted replay without 409; final turn never submitted", id)
		} else if h.tryAdoptInterrupted(r, id, s) {
			// Adopted the completed answer above; fall through to normal
			// processing (exact retries hit the duplicate cache below).
			log.Printf("[openai] session=%s adopted interrupted final turn; continuing request", id)
		} else {
			// Installed v2 retries HTTP409 as provider.internal. This request
			// cannot safely progress without recovery, so return terminal422
			// while retaining the exact same uncertainty/adoption protections.
			apiError(w, 422, errors.New("session has an uncertain submitted turn; POST /v1/sessions/recover with the same session header and exact original request to validate/cache the stored response without resubmitting"))
			return
		}
	}
	if s.RejectedFingerprint == fingerprint || (s.Phase == "invalid" && s.Fingerprint == fingerprint) {
		apiError(w, 422, errors.New("completed model output failed strict tool protocol validation; no tool calls were delivered; send a new ordinary user message to continue in a fresh thread"))
		return
	}
	if s.Fingerprint == fingerprint {
		if s.Result.ID == "" {
			// Older state files did not persist completion metadata. Derive a
			// stable fallback so legacy cached retries do not regenerate IDs.
			s.Result.ID = fmt.Sprintf("chatcmpl-%x", sha256.Sum256([]byte(id+":"+fingerprint)))
		}
		log.Printf("[openai] session=%s duplicate-cache", id)
		h.respond(w, req, s.Result)
		return
	}
	start := 0
	if s.Phase == "invalid" {
		pending := s.PendingRequest
		// v2 rebuilds system/history and retains failed assistant placeholders.
		// Only a distinct, final ordinary user message may cross this gate;
		// schemas, tool results, or a changed system prompt alone are not input.
		last := req.Messages[len(req.Messages)-1]
		// v2 emits changing <system-update> metadata as a user message.
		// It is not an ordinary user boundary; ignore it only for comparison,
		// never remove it from the authoritative context sent to the model.
		updates := regexp.MustCompile(`(?s)<system-update>.*?</system-update>`)
		text := strings.TrimSpace(updates.ReplaceAllString(contentText(last), ""))
		freshUser := pending != nil && last.Role == "user" && text != "" && len(last.ToolCalls) == 0 && last.ToolCallID == ""
		if pending != nil {
			for _, old := range pending.Messages {
				oldText := strings.TrimSpace(updates.ReplaceAllString(contentText(old), ""))
				if old.Role == "user" && oldText != "" && (strings.Contains(text, oldText) || strings.Contains(oldText, text)) {
					freshUser = false
				}
			}
			if prefix(req.Messages, pending.Messages) {
				for _, m := range req.Messages[len(pending.Messages):] {
					if m.Role == "tool" || len(m.ToolCalls) != 0 || m.ToolCallID != "" {
						freshUser = false // rejected calls were never delivered
					}
				}
			}
		}
		if !freshUser {
			apiError(w, 422, errors.New("rejected model output requires a distinct new ordinary user message at the end of the authoritative transcript; no tool calls were delivered"))
			return
		}
		// Do not consume the durable rejection gate until the new request has
		// passed prompt/budget checks and is journaled for submission below.
		next := *s
		s = &next
		if s.RejectedFingerprint == "" {
			s.RejectedFingerprint = s.Fingerprint // upgrade existing rejected journals
		}
		s.URL, s.Messages, s.ToolsFP, s.Fingerprint, s.Result = "", nil, "", "", result{}
		s.PendingRequest, s.PendingPrompt, s.Phase = nil, "", ""
	}
	url := s.URL
	decision := "fresh"
	if url != "" {
		if prefix(req.Messages, s.Messages) {
			decision = "delta"
			start = len(s.Messages)
		} else {
			mismatch := firstMismatch(req.Messages, s.Messages)
			log.Printf("[openai] session=%s prefix_mismatch index=%d field=%s incoming_messages=%d saved_messages=%d incoming_encoding=%s saved_encoding=%s incoming_shape=%s saved_shape=%s", id, mismatch.Index, mismatch.Field, len(req.Messages), len(s.Messages), mismatch.IncomingEncoding, mismatch.SavedEncoding, mismatch.IncomingShape, mismatch.SavedShape)
			// The client history no longer matches the stored thread (client
			// compaction or a reused session ID). Re-establish context by
			// replaying the full client history into a new thread instead of
			// failing or concatenating it into one paste. The old thread is
			// left untouched.
			decision, start, url = "reset-replay", 0, ""
			// Flag-gated project-per-session: rotate into the session's
			// dedicated project instead of reset-replaying. serveRotation
			// falls back only on a proved pre-submission navigation failure;
			// a possibly submitted turn remains protected.
			if projectPerSessionEnabled() && id != "" && !resync {
				if h.serveRotation(w, r, id, lane, req, s) {
					return
				}
				log.Printf("[openai] session=%s lane=%s rotation_fallback: continuing with shared-project reset-replay", id, lane)
			}
		}
	}
	if resync {
		decision, start, url = "resync", 0, ""
	}
	if start == len(req.Messages) {
		apiError(w, 409, errors.New("no new messages"))
		return
	}
	delta := req.Messages[start:]
	// Teach the full tool schemas only when the thread cannot know them yet:
	// a new thread, or a changed tool set. Otherwise the thread holds them
	// server-side and a compact reminder suffices — resending the whole dump
	// every turn is what blew the per-turn paste budget.
	toolsFP := toolsFingerprint(req.Tools)
	teachFull := decision != "delta" || s.ToolsFP == "" || s.ToolsFP != toolsFP
	head := toolReminder(req)
	if teachFull {
		head = toolPrompt(req)
	}
	turns := h.buildTurns(req, delta, head)
	if resync {
		turns = h.buildResyncTurns(req, s.ResyncMessages, head)
	}
	for i, t := range turns {
		if h.maxChars > 0 && len(t.prompt) > h.maxChars {
			apiError(w, 400, fmt.Errorf("turn %d/%d is %d bytes, over the per-turn budget %d (tool schemas/instructions ~%d bytes, teach_full=%v); reduce attached tools or raise -max-chars", i+1, len(turns), len(t.prompt), h.maxChars, len(head), teachFull))
			return
		}
	}
	thinking := h.forceThinking || (req.ReasoningEffort != nil && req.ReasoningEffort != "none") || (req.Reasoning != nil && req.Reasoning != false)
	log.Printf("[openai] session=%s lane=%s decision=%s delta_messages=%d turns=%d tools=%d tools_bytes=%d teach_full=%v stream=%v thread=%s", id, lane, decision, len(delta), len(turns), len(req.Tools), toolsBytes(req.Tools), teachFull, req.Stream, url)
	// Journal uncertainty before the first submission so a crash/retry cannot
	// duplicate a tool turn. The thread URL is persisted turn by turn.
	if id != "" {
		h.sessions[id] = s
		s.Dirty = true
		// Consume explicit permission before submitting anything; a failed resync
		// cannot be retried automatically into another thread.
		if resync {
			s.ResyncMessages = nil
		}
		if err := h.save(); err != nil {
			h.backendError(w, err)
			return
		}
	}
	var answer, reasoning string
	for i, t := range turns {
		if id != "" {
			// Record the interruption phase before submitting. Replay turns
			// are inert history imports: if interrupted here, the request can
			// simply resume because the final turn was never submitted. A
			// final turn may execute with unknown outcome and needs read-only
			// adoption (or 409) on retry.
			if t.replay {
				s.Phase = "replay"
			} else {
				s.Phase = "final"
				s.PendingRequest, s.PendingPrompt = &req, t.prompt
			}
			if err := h.save(); err != nil {
				h.backendError(w, err)
				return
			}
		}
		res, err := h.eagerThreadTurn(r, id, lane, req, s, t, url, thinking, decision, resync)
		if err != nil {
			// Persist the URL only when the turn really reached a conversation,
			// so the dirty journal records a usable repair target, not a page
			// like the Project root. Uncertain sessions still return 409 and
			// never retry.
			if res.URL != "" && strings.Contains(res.URL, "/c/") {
				s.URL = res.URL
				if id != "" {
					_ = h.save()
				}
			} else if res.URL == "" && s.URL == "" && chatgpt.IsNavigateError(err) {
				// The page never loaded, so provably nothing was typed or
				// submitted: clear the uncertainty flag so a retry can proceed
				// instead of wedging the session forever on a transient error
				// (e.g. the browser restarting). A retry starts over on a
				// fresh thread; duplication is impossible because the first
				// attempt never reached a conversation.
				s.Dirty = false
				s.Phase = ""
				if id != "" {
					if serr := h.save(); serr != nil {
						h.backendError(w, serr)
						return
					}
				}
				log.Printf("[openai] transient pre-submission failure session=%s, cleared for retry: %v", id, err)
			}
			log.Printf("[openai] uncertain session=%s turn=%d/%d thread=%s", id, i+1, len(turns), s.URL)
			h.backendError(w, err)
			return
		}
		url = res.URL
		if id != "" {
			s.URL = url // crash-safe resume target for the in-flight session
			_ = h.save()
		}
		if t.replay {
			// Acknowledgements are never parsed as executable tool calls. Fail
			// closed on unexpected output rather than advancing a false prefix.
			if strings.TrimSpace(res.Answer) != "ACK" {
				h.backendError(w, errors.New("history import did not acknowledge with ACK; session remains uncertain"))
				return
			}
			if id != "" && t.synced > 0 {
				s.Messages = append([]message{}, req.Messages[:start+t.synced]...)
				if err := h.save(); err != nil {
					h.backendError(w, err)
					return
				}
			}
			log.Printf("[openai] session=%s replayed chunk=%d/%d bytes=%d thread=%s", id, i+1, len(turns), len(t.prompt), url)
			continue
		}
		answer, reasoning = res.Answer, res.Reasoning
	}
	out, err := parseAnswer(answer, req)
	if err != nil {
		s.URL = url
		if id != "" {
			if serr := h.rejectCompleted(s, fingerprint); serr != nil {
				h.backendError(w, serr)
				return
			}
		}
		log.Printf("[openai] invalid tool protocol session=%s thread=%s", id, url)
		apiError(w, 422, errors.New("completed model output failed strict tool protocol validation; no tool calls were delivered; send a new ordinary user message"))
		return
	}
	out.Reasoning = reasoning
	out.ID, out.Created = fmt.Sprintf("chatcmpl-%x", sha256.Sum256([]byte(id+":"+fingerprint))), time.Now().Unix()
	for i := range out.Calls {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d", id, fingerprint, i)))
		out.Calls[i].ID = fmt.Sprintf("call_%x", sum[:12])
	}
	// Include our assistant message in the expected prefix: never replay it to ChatGPT.
	s.Messages = append(append([]message{}, req.Messages...), message{Role: "assistant", Content: out.Text, ToolCalls: out.Calls})
	s.URL, s.Fingerprint, s.Result, s.Dirty = url, fingerprint, out, false
	s.ToolsFP = toolsFP // this thread now knows exactly this tool set
	s.PendingRequest, s.PendingPrompt, s.Phase = nil, "", ""
	if id != "" {
		if err := h.save(); err != nil {
			s.Dirty = true
			h.backendError(w, err)
			return
		}
	}
	log.Printf("[openai] session=%s done tool_calls=%d answer_bytes=%d thread=%s", id, len(out.Calls), len(answer), url)
	h.respond(w, req, out)
}
func (h *handler) backendError(w http.ResponseWriter, err error) {
	log.Printf("[openai] BACKEND FAILURE: %v; REPAIR: check CDP port/profile login, composer/send/assistant DOM selectors, ChatGPT page alerts; uncertain sessions return 409", err)
	apiError(w, 502, err)
}
func (h *handler) respond(w http.ResponseWriter, req completionsRequest, out result) {
	id, created := out.ID, out.Created
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
		created = time.Now().Unix()
	}
	finish := "stop"
	if len(out.Calls) > 0 {
		finish = "tool_calls"
	}
	if !req.Stream {
		msg := map[string]any{"role": "assistant", "content": out.Text}
		if out.Reasoning != "" {
			msg["reasoning_content"] = out.Reasoning
		}
		if len(out.Calls) > 0 {
			msg["tool_calls"] = out.Calls
		}
		writeJSON(w, 200, map[string]any{"id": id, "object": "chat.completion", "created": created, "model": req.Model, "choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}}, "usage": map[string]int{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}})
		return
	}
	// Buffer browser output until validated. Raw protocol fences never reach SSE clients.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	send := func(delta map[string]any, reason any) {
		b, _ := json.Marshal(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": reason}}})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	send(map[string]any{"role": "assistant"}, nil)
	if out.Reasoning != "" {
		send(map[string]any{"reasoning_content": out.Reasoning}, nil)
	}
	if out.Text != "" {
		send(map[string]any{"content": out.Text}, nil)
	}
	for i, c := range out.Calls {
		send(map[string]any{"tool_calls": []any{map[string]any{"index": i, "id": c.ID, "type": c.Type, "function": c.Function}}}, nil)
	}
	send(map[string]any{}, finish)
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
