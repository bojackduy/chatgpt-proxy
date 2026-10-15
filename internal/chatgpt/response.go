package chatgpt

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"time"
)

//go:embed response.js
var responseProbeJS string

type responseState struct {
	IDs            []string `json:"ids"`
	LastID         string   `json:"lastID"`
	StopBtn        bool     `json:"stopBtn"`
	ContinueBtn    bool     `json:"continueBtn"`
	ErrorText      bool     `json:"errorText"`
	Last           string   `json:"lastAssistant"`
	Reasoning      string   `json:"reasoning"`
	Fresh          bool     `json:"fresh"`
	Complete       bool     `json:"complete"`
	SourceRequired bool     `json:"sourceRequired"`
}

type emptyAnswerWatch struct {
	Started  time.Time
	Since    time.Time
	SeenStop bool
}

func (w *emptyAnswerWatch) expired(st responseState, now time.Time) bool {
	if st.StopBtn {
		w.SeenStop = true
		w.Since = time.Time{}
		return false
	}
	if st.Last != "" || st.ContinueBtn {
		w.Since = time.Time{}
		return false
	}
	if w.SeenStop || st.Complete || now.Sub(w.Started) >= 30*time.Second {
		if w.Since.IsZero() {
			w.Since = now
		}
		return now.Sub(w.Since) >= 15*time.Second
	}
	return false
}

func responseProbe(tab context.Context, baseline []string) (responseState, error) {
	if baseline == nil {
		baseline = []string{}
	}
	b, _ := json.Marshal(baseline)
	var st responseState
	err := chromedp.Run(tab, chromedp.Evaluate(responseProbeJS+"("+string(b)+")", &st))
	if err == nil && st.Fresh && st.Complete && (st.Last == "" || st.SourceRequired) && !st.StopBtn && !st.ContinueBtn && !st.ErrorText {
		// The observed DIL renderer can consume literal protocol tags and render
		// null. Read only that fresh DOM identity, never a backend latest branch.
		// Re-probe inside the readback to fence a changed user/assistant boundary.
		probe := responseProbeJS + "(" + string(b) + ")"
		var recovered string
		expected, _ := json.Marshal(st.LastID)
		read, cancel := context.WithTimeout(tab, 8*time.Second)
		defer cancel()
		js := "(async () => {const recheck = () => " + probe + "; const st = recheck(); if (st.lastID !== " + string(expected) + ") return '';" + nullDILWireJS + "})()"
		if rerr := chromedp.Run(read, chromedp.Evaluate(js, &recovered, chromedp.EvalAsValue, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); rerr == nil && recovered != "" {
			st.Last = recovered
		} else if st.SourceRequired {
			return st, errors.New("completed protocol source could not be verified against its DOM identity; rendered arguments are not safe")
		}
	}
	return st, err
}

// Only completed DIL protocol responses use source readback: the captured null
// renderer failure, or visible protocol whose rendering can transform JSON.
// Ordinary Markdown remains DOM-only; all schema validation stays in the adapter.
// Neither the model's renderer code nor its textual tool call is run.
const nullDILWireJS = `
 if (!st.fresh || !st.complete || (st.lastAssistant && !st.sourceRequired) || st.stopBtn || st.continueBtn || st.errorText) return '';
 const id = st.lastID;
 if (!/^[a-zA-Z0-9-]+$/.test(id)) return '';
 const root = [...document.querySelectorAll('[data-dil-message-id]')].find(e => e.getAttribute('data-dil-message-id') === id);
 if (!root || (!st.sourceRequired && root.childNodes.length !== 0)) return '';
 const users = [...document.querySelectorAll('h4')].filter(e => e.textContent.trim() === 'You said:');
 const user = users.at(-1);
 const owner = user?.nextElementSibling?.closest('[data-chatgpt-search-message-ids], [data-message-id]') || user?.closest('[data-chatgpt-search-message-ids], [data-message-id]');
 const uid = (owner?.getAttribute('data-chatgpt-search-message-ids') || owner?.getAttribute('data-message-id') || '').trim().split(/\s+/).at(-1);
 if (!uid || !(user.compareDocumentPosition(root) & Node.DOCUMENT_POSITION_FOLLOWING)) return '';
 const thread = location.pathname.split('/c/')[1];
 if (!thread) return '';
 try {
   const auth = await (await fetch('/api/auth/session')).json();
   if (!auth.accessToken) return '';
   const r = await fetch('/backend-api/conversation/' + encodeURIComponent(thread), {headers:{Authorization:'Bearer ' + auth.accessToken}});
   if (!r.ok) return '';
   const mapping = (await r.json()).mapping;
   const node = mapping?.[id], m = node?.message, dil = m?.metadata?.model_dil_v2;
   if (m?.id !== id || m.author?.role !== 'assistant' || m.channel !== 'final' || m.recipient !== 'all' ||
       m.status !== 'finished_successfully' || m.end_turn !== true || m.metadata?.is_complete !== true ||
       m.content?.content_type !== 'text' || m.content.parts?.length !== 1 || typeof m.content.parts[0] !== 'string' ||
        !dil) return '';
    if (!st.sourceRequired && (dil.fallbackMarkdown !== '' || !/return null;/.test(dil.code || '') ||
        !dil.recoveryDiagnostics?.some(d => d.code === 'invalid_tag_syntax' && d.action === 'recovered_parse'))) return '';
   // Follow only the selected DOM node's declared ancestry to the latest DOM
   // user; no sibling, current_node or latest-message selection is allowed.
   let parent = node.parent, bound = false;
   for (let i = 0; parent && i < 32; i++) {
     const p = mapping[parent];
     if (!p || p.message?.id !== parent) return '';
     if (p.message.author?.role === 'user') { bound = parent === uid; break; }
     parent = p.parent;
   }
    const text = m.content.parts[0].trim();
    // Do not reintroduce standalone empty escape artifacts via source fallback.
    // Refuse rather than rewriting source: JSON strings inside exact wrappers
    // (e.g. {"value":"<escape>"}) remain byte-for-byte unchanged.
    if (/(?:^|\n)[ \t]*(?:<escape>\s*<\/escape>[ \t]*)+(?=\n|$)/.test(text)) return '';
    if (!bound || !(st.sourceRequired ? /<\/?opencode(?:\\)?_bridge(?:\\)?_call\b/ : /^<opencode(?:\\)?_bridge(?:\\)?_call\b/).test(text)) return '';
   // The read may have awaited network while React changed the page.
   const after = recheck();
    if (!root.isConnected || after.lastID !== id || !after.complete || after.lastAssistant !== st.lastAssistant || after.sourceRequired !== st.sourceRequired || after.stopBtn || after.continueBtn || after.errorText || users.at(-1) !== [...document.querySelectorAll('h4')].filter(e => e.textContent.trim() === 'You said:').at(-1)) return '';
   return text;
 } catch (_) { return ''; }
`
