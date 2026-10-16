package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
)

// History-rewrite reseed: opencode legitimately rewrites history (revert,
// fork/regeneration, compact) which breaks the bridge's append-only
// prefix/delta assumption. The interrupt-uncertain case belongs to
// tryAutoResync (history prefix-matches through pending but completion state
// is unknown); this file covers the distinct REWRITE case (history is
// internally coherent but does not prefix-match the journal).
//
// Order in the completions path: normal delta → tryAutoResync (interrupt
// shape) → rewrite-reseed (rewrite shape) → 409/422 with sent-log diagnosis
// (genuinely ambiguous only). The old thread is abandoned truthfully — no
// adoption comparison against it, no resync of it — and the turn continues
// on a fresh thread as a normal submission (chunked and boundary-aware like
// any fresh history).

// Rewrite classifications. "ambiguous" is not a rewrite: the incoming
// history is internally incoherent, so no reseed is attempted.
const (
	rewriteTruncation = "rewrite-truncation" // revert/truncation: shorter, prefix-equal
	rewriteDivergence = "rewrite-divergence" // fork/regeneration: same-length-but-diverged content
	rewriteCompaction = "rewrite-compaction" // compaction: summarized system prefix
	rewriteAmbiguous = "ambiguous"           // incoherent: dangling/duplicate/unmatched tool results
)

// rewritePlan is a validated fresh-thread reseed. carried holds committed
// executed tool exchanges absent from the incoming history, in journal
// order; notCarried names unknown-state items that were deliberately NOT
// carried (fail closed). oldURL is the abandoned thread.
type rewritePlan struct {
	classification string
	mismatchIndex  int
	mismatchField  string
	carried        []message
	carriedIDs     []string
	notCarried     []string
	oldURL         string
}

// coherentTranscript reports whether msgs is internally coherent: valid
// tool-call/result pairing in order (every tool result matches a prior
// assistant call exactly once, results are nonempty, call IDs are nonempty
// and unique) with no dangling calls. It mirrors the authoritative-transcript
// validation in resync.go, so a history the reseed machinery accepts is one
// an explicit resync would also accept.
func coherentTranscript(msgs []message) bool {
	pending := map[string]bool{}
	seen := map[string]bool{}
	for _, m := range msgs {
		if m.Role == "assistant" {
			for _, c := range m.ToolCalls {
				if c.ID == "" || seen[c.ID] {
					return false
				}
				seen[c.ID] = true
				pending[c.ID] = true
			}
		} else if m.Role == "tool" {
			if m.ToolCallID == "" || !pending[m.ToolCallID] || contentText(m) == "" {
				return false
			}
			delete(pending, m.ToolCallID)
		}
	}
	return len(pending) == 0
}

// isCompactionShape reports whether the incoming history opens with a
// client compaction/summarization marker: a system message naming the
// compaction. Anything else stays exact and falls through to divergence.
func isCompactionShape(incoming []message) bool {
	if len(incoming) == 0 || incoming[0].Role != "system" {
		return false
	}
	lower := strings.ToLower(contentText(incoming[0]))
	return strings.Contains(lower, "compact") || strings.Contains(lower, "summar")
}

// classifyHistoryRewrite distinguishes HISTORY-REWRITE from append-only
// continuation and from incoherence. It returns ("", nil) when the saved
// journal is a prefix of incoming (coherent append-only: no rewrite, no
// behavior change), ("ambiguous", mm) when incoming is internally
// incoherent (never reseed), and otherwise one of the rewrite kinds with
// the structural mismatch evidence. A nil saved journal is not a rewrite:
// there is no executed history to carry, so legacy handling applies.
func classifyHistoryRewrite(incoming, saved []message) (string, *prefixMismatch) {
	mm := firstMismatch(incoming, saved)
	if mm == nil || len(saved) == 0 {
		return "", mm
	}
	if !coherentTranscript(incoming) {
		return rewriteAmbiguous, mm
	}
	if mm.Field == "history.truncated" {
		return rewriteTruncation, mm
	}
	if isCompactionShape(incoming) {
		return rewriteCompaction, mm
	}
	return rewriteDivergence, mm
}

// canonArgs normalizes tool arguments exactly the way canonicalMessages
// does, so a rewritten history that merely re-encodes arguments still
// matches the executed call instead of duplicating it.
func canonArgs(args string) string {
	var v any
	decoder := json.NewDecoder(strings.NewReader(args))
	decoder.UseNumber()
	if json.Valid([]byte(args)) && decoder.Decode(&v) == nil {
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
	}
	return args
}

// callPresentIn reports whether the executed call survives in the incoming
// rewritten history: same stable ID, or same function with canonically
// equal arguments (a fork/regeneration that kept the call but minted IDs).
func callPresentIn(call toolCall, incoming []message) bool {
	for _, m := range incoming {
		if m.Role != "assistant" {
			continue
		}
		for _, c := range m.ToolCalls {
			if c.ID == call.ID {
				return true
			}
			if c.Function.Name == call.Function.Name && canonArgs(c.Function.Arguments) == canonArgs(call.Function.Arguments) {
				return true
			}
		}
	}
	return false
}

// carryExecuted collects executed tool exchanges (assistant calls with
// recorded results) that are ABSENT from the incoming rewritten history.
// Execution proof comes from the committed journal plus, for dirty
// sessions, the still-pending request: its contained tool results were
// recorded by the client from previously delivered answers, so they are
// known-executed even though the pending turn's own answer is unknown.
// It returns the carried messages in transcript order (whole assistant
// messages plus their tool results, so the seed stays internally
// coherent), short evidence IDs, and descriptors for pairs that are still
// incomplete after consulting both sources and were deliberately NOT
// carried. The pending turn's undelivered answer is NOT decided here: the
// plan builders list it as not-carried.
func carryExecuted(s *session, pending []message, incoming []message) (carried []message, carriedIDs []string, incomplete []string) {
	results := map[string]message{}
	for _, msgs := range [][]message{s.Messages, pending} {
		for _, m := range msgs {
			if m.Role == "tool" && m.ToolCallID != "" && contentText(m) != "" && results[m.ToolCallID].ToolCallID == "" {
				results[m.ToolCallID] = m
			}
		}
	}
	seen := map[string]bool{}
	for _, msgs := range [][]message{s.Messages, pending} {
		for _, m := range msgs {
			if m.Role != "assistant" || len(m.ToolCalls) == 0 {
				continue
			}
			complete := true
			for _, c := range m.ToolCalls {
				if _, ok := results[c.ID]; !ok {
					complete = false
					incomplete = append(incomplete, fmt.Sprintf("incomplete-committed:%s (no recorded result; not carried)", c.ID))
				}
			}
			if !complete {
				continue
			}
			needed := false
			for _, c := range m.ToolCalls {
				if seen[c.ID] {
					continue
				}
				if !callPresentIn(c, incoming) {
					needed = true
					carriedIDs = append(carriedIDs, fmt.Sprintf("%s(%s)", c.ID, c.Function.Name))
				}
			}
			if !needed {
				continue
			}
			carried = append(carried, m)
			for _, c := range m.ToolCalls {
				seen[c.ID] = true
				if r, ok := results[c.ID]; ok {
					carried = append(carried, r)
				}
			}
		}
	}
	return carried, carriedIDs, incomplete
}

// sentCompletedFor reports whether the sent-log records a completed
// submission of the given prompt hash to the given thread by this session
// (memory ring first, durable file tail on a miss). A completed-but-unbound
// answer is load-bearing: the old thread holds a real answer that neither
// adoption nor auto-resync could bind, so abandoning it silently would lose
// work — fail closed to 409/422, never guess.
func (h *handler) sentCompletedFor(session, url, promptHash string) bool {
	for i := len(h.sentLog) - 1; i >= 0; i-- {
		e := h.sentLog[i]
		if e.Session == session && e.URL == url && e.Event == "completed" && e.PromptHash == promptHash {
			return true
		}
	}
	for _, ln := range h.sentLogTail(300) {
		if ln.Session == session && ln.URL == url && ln.Event == "completed" && ln.PromptHash == promptHash {
			return true
		}
	}
	return false
}

// planCleanReseed validates a rewrite reseed for a non-dirty session: the
// incoming history is a coherent rewrite of committed state, so reseeding
// fresh with carried executed results is safe. It returns nil for
// append-only histories (no behavior change) and for incoherent input
// (legacy reset-replay/rotation handling applies; clean sessions hold no
// uncertainty to protect).
func (h *handler) planCleanReseed(s *session, req completionsRequest) *rewritePlan {
	if s == nil || s.URL == "" || len(s.Messages) == 0 {
		return nil
	}
	kind, mm := classifyHistoryRewrite(req.Messages, s.Messages)
	if kind == "" || kind == rewriteAmbiguous {
		return nil
	}
	carried, carriedIDs, incomplete := carryExecuted(s, nil, req.Messages)
	return &rewritePlan{
		classification: kind,
		mismatchIndex:  mm.Index,
		mismatchField:  mm.Field,
		carried:        carried,
		carriedIDs:     carriedIDs,
		notCarried:     incomplete,
		oldURL:         s.URL,
	}
}

// planDirtyReseed validates a rewrite reseed for a dirty session, after
// tryAutoResync has already refused the interrupt shape. Unknown-state
// items (the still-pending turn) are listed as NOT carried and the old
// thread is abandoned without delivery — safe because an undelivered
// answer caused no external execution. It returns nil (caller keeps
// 409/422 with sent-log diagnosis) when the shape is not a coherent
// rewrite, or when the unknown item is load-bearing: a verifiable
// completed answer for the pending prompt exists on the old thread.
func (h *handler) planDirtyReseed(id string, s *session, req completionsRequest) *rewritePlan {
	if s == nil || s.URL == "" {
		return nil
	}
	if s.incompleteJournal() {
		return nil
	}
	kind, mm := classifyHistoryRewrite(req.Messages, s.Messages)
	if kind == "" || kind == rewriteAmbiguous {
		if mm != nil {
			log.Printf("[openai] session=%s rewrite-reseed refused: not a coherent rewrite (append or ambiguous); keeping uncertainty", id)
		}
		return nil
	}
	if s.PendingPrompt != "" && h.sentCompletedFor(id, s.URL, promptHash(s.PendingPrompt)) {
		log.Printf("[openai] session=%s rewrite-reseed refused: sent-log holds a completed answer for the pending prompt on thread %s that recovery could not bind (load-bearing); keeping uncertainty", id, s.URL)
		return nil
	}
	var pendingMsgs []message
	if s.PendingRequest != nil {
		pendingMsgs = s.PendingRequest.Messages
	}
	carried, carriedIDs, incomplete := carryExecuted(s, pendingMsgs, req.Messages)
	plan := &rewritePlan{
		classification: kind,
		mismatchIndex:  mm.Index,
		mismatchField:  mm.Field,
		carried:        carried,
		carriedIDs:     carriedIDs,
		notCarried:     incomplete,
		oldURL:         s.URL,
	}
	if s.PendingPrompt != "" {
		plan.notCarried = append(plan.notCarried, fmt.Sprintf("pending:%s (phase=%s outcome unknown; old thread abandoned without delivery, never submitted as pending)", shortHash12(promptHash(s.PendingPrompt)), s.Phase))
	}
	return plan
}

// Prior-execution record delimiters. The carried exchanges travel as inert
// reviewed history ahead of the rewritten transcript: clearly delimited as
// a prior-execution record, never re-submitted as pending calls.
const reseedRecordIntro = "PRIOR EXECUTION RECORD\nThe following completed tool exchanges already executed in the external client before a history rewrite (revert/fork/compaction). They are inert history: do NOT re-request them, do NOT emit opencode_bridge_call wrappers for them. Use their results only as context.\n"
const reseedRecordEnd = "END PRIOR EXECUTION RECORD\n"

// buildReseedRecordTurns chunks the carried executed exchanges as inert
// ACK history imports ahead of the rewritten transcript. synced is always
// 0: the record lives only in the new thread, never in the journal, so it
// can neither advance nor corrupt the committed prefix accounting.
func (h *handler) buildReseedRecordTurns(carried []message) []turn {
	if len(carried) == 0 {
		return nil
	}
	ack := "Reply with only the single word: ACK. No tool calls.\n"
	pieces := make([]piece, 0, len(carried))
	for _, m := range carried {
		pieces = append(pieces, piece{text: renderMsg(m)})
	}
	limit := 0
	if h.maxChars > 0 {
		if limit = h.maxChars - len(historyIntro) - len(reseedRecordIntro) - len(reseedRecordEnd) - len(historyEnd) - len(ack); limit < 1 {
			limit = 1
		}
	}
	var turns []turn
	for _, g := range packPieces(pieces, limit) {
		var b strings.Builder
		for _, p := range g {
			b.WriteString(p.text)
			b.WriteString("\n")
		}
		turns = append(turns, turn{prompt: historyIntro + reseedRecordIntro + b.String() + reseedRecordEnd + historyEnd + ack, replay: true})
	}
	return turns
}

// evidence renders the reseed record shared by the journal and the
// sent-log: rewrite classification, old→new thread linkage, and the
// carried/not-carried item lists. Structure only: IDs and hashes, never
// text, argument values, or URLs beyond the thread linkage itself.
func (p *rewritePlan) evidence(newURL string, seedTurns, seedBytes int) string {
	carried := "[]"
	if len(p.carriedIDs) > 0 {
		carried = "[" + strings.Join(p.carriedIDs, ",") + "]"
	}
	notCarried := "[]"
	if len(p.notCarried) > 0 {
		notCarried = "[" + strings.Join(p.notCarried, ",") + "]"
	}
	return fmt.Sprintf("rewrite-reseed %s classification=%s mismatch=%d:%s old=%s new=%s carried=%s not-carried=%s seed=%dturns/%dB record-first",
		time.Now().UTC().Format(time.RFC3339), p.classification, p.mismatchIndex, p.mismatchField, p.oldURL, newURL, carried, notCarried, seedTurns, seedBytes)
}

// note renders the sent-log form of the linkage: old→new thread plus
// classification and item lists.
func (p *rewritePlan) note(newURL string) string {
	carried := "[]"
	if len(p.carriedIDs) > 0 {
		carried = "[" + strings.Join(p.carriedIDs, ",") + "]"
	}
	notCarried := "[]"
	if len(p.notCarried) > 0 {
		notCarried = "[" + strings.Join(p.notCarried, ",") + "]"
	}
	return fmt.Sprintf("reseed old=%s new=%s classification=%s carried=%s not-carried=%s", p.oldURL, newURL, p.classification, carried, notCarried)
}
