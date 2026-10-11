package chatgpt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/yuin/goldmark"
	"log"
	"strconv"
	"strings"
	"time"
)

// firstDiffByte returns the first byte offset at which a and b differ,
// or min(len) when one is a prefix of the other. Evidence-only helper for
// the adoption user-bubble comparison above.
func firstDiffByte(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// ReadThreadTurn only reads an existing finished turn in an owned target. The
// last rendered user bubble (or its exact ID-bound original text node) must
// equal the full normalized pending prompt. Never walk the backend mapping to
// guess the latest branch; only decode declared serialization for that DOM ID.
// A completed assistant body must follow it. Anything else fails closed.
func (c *Client) ReadThreadTurn(ctx context.Context, url, prompt string) (TurnResult, error) {
	if !threadPattern.MatchString(url) || prompt == "" {
		return TurnResult{}, errors.New("recovery requires a stored thread and exact original prompt")
	}
	ctx, cancelRead := context.WithTimeout(ctx, 90*time.Second)
	defer cancelRead()
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := c.tabCtx(ctx)
	if err != nil {
		return TurnResult{}, err
	}
	// Recovery must never reload the active adapter tab. Own a background
	// target for navigation/readback and close only that target afterwards.
	var tid target.ID
	if err := chromedp.Run(c.browser, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		tid, err = target.CreateTarget("about:blank").WithBackground(true).Do(cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Browser))
		return err
	})); err != nil {
		return TurnResult{}, err
	}
	defer func() {
		cleanup, done := context.WithTimeout(c.browser, 3*time.Second)
		defer done()
		_ = chromedp.Run(cleanup, chromedp.ActionFunc(func(ctx context.Context) error {
			return target.CloseTarget(tid).Do(cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Browser))
		}))
	}()
	owned, closeOwned := chromedp.NewContext(c.browser, chromedp.WithTargetID(tid))
	defer closeOwned()
	tab, cancel := requestContext(ctx, owned)
	defer cancel()
	nav, done := context.WithTimeout(tab, 45*time.Second)
	defer done()
	if err := chromedp.Run(nav, chromedp.Navigate(url)); err != nil {
		return TurnResult{}, err
	}
	if err := c.waitPageReady(ctx, tab, 30*time.Second); err != nil {
		return TurnResult{}, err
	}
	// Expand only the last user bubble's exact UI control; never a send,
	// regenerate, edit, or global page button. React may update asynchronously.
	var expanded bool
	if err := chromedp.Run(tab, chromedp.Evaluate(recoveryExpandJS, &expanded)); err != nil {
		return TurnResult{}, err
	}
	if expanded {
		if err := pause(tab, 200*time.Millisecond); err != nil {
			return TurnResult{}, err
		}
	}
	// Correlate in the rendered DOM: the last "You said:" bubble must match
	// the pending prompt (whitespace-normalized; rendered text reflows), and
	// a "ChatGPT said:" section must follow it, so a stale earlier answer can
	// never be adopted. The answer itself comes from responseProbe — the same
	// rendered representation as the normal path.
	var corr struct {
		User       string `json:"user"`
		UserID     string `json:"userID"`
		AnswerNext bool   `json:"answerNext"`
		Answers    int    `json:"answers"`
	}
	if err := chromedp.Run(tab, chromedp.Evaluate(recoveryUserJS, &corr)); err != nil {
		return TurnResult{}, err
	}
	normSpace := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	want := normSpace(prompt)
	userText := corr.User
	if userText != want && corr.UserID != "" {
		// The actual DOM-bound node, never a latest/branch mapping walk. The
		// composer stores plain paragraphs as escaped CommonMark. Decode that
		// declared format with a real parser, not guessed slash/prefix stripping.
		if original, err := originalUserText(tab, corr.UserID); err == nil {
			userText = normSpace(original)
		}
	}
	if userText == "" || userText != want {
		off := firstDiffByte(userText, want)
		log.Printf("[chatgpt] adoption user-bubble mismatch rendered=%dB want=%dB diffoff=%d", len(userText), len(want), off)
		return TurnResult{}, fmt.Errorf("stored user prompt does not exactly match original request (rendered %dB want %dB)", len(userText), len(want))
	}
	if !corr.AnswerNext {
		return TurnResult{}, errors.New("current response is not finished and nonempty")
	}
	if corr.Answers != 1 {
		return TurnResult{}, errors.New("ambiguous multiple assistant responses after stored user prompt")
	}
	visible, err := responseProbe(tab, nil)
	if err != nil {
		return TurnResult{}, err
	}
	if visible.Last == "" || visible.StopBtn || visible.ErrorText || visible.ContinueBtn || !visible.Complete {
		return TurnResult{}, errors.New("finished stored response is not identically rendered; inspect before recovery")
	}
	return TurnResult{Answer: visible.Last, Reasoning: visible.Reasoning, URL: url}, nil
}

const recoveryExpandJS = `(() => {
	const h = [...document.querySelectorAll('h4')].filter(e => (e.textContent || '').trim() === 'You said:').at(-1);
	const bs = [...(h?.nextElementSibling?.querySelectorAll('button') || [])].filter(b => (b.textContent || '').trim() === 'Show more');
	if (bs.length !== 1) return false;
	bs[0].click(); return true;
})()`

const recoveryUserJS = `(() => {
	  const norm = s => (s || '').replace(/\s+/g, ' ').trim();
	  const h4s = [...document.querySelectorAll('h4')];
	  let ui = -1;
	  for (let i = 0; i < h4s.length; i++) {
	    if ((h4s[i].textContent || '').trim() === 'You said:') ui = i;
	  }
	  if (ui < 0) return {user: '', answerNext: false};
	  let el = h4s[ui].nextElementSibling, parts = [], next = false;
	  while (el && el.tagName !== 'H4') {
	    const clone = el.cloneNode(true);
	    for (const b of clone.querySelectorAll('button, [role="toolbar"], [aria-hidden="true"], [data-markdown-copy="exclude"]')) b.remove();
	    for (const b of clone.querySelectorAll('br')) b.replaceWith(document.createTextNode('\n'));
	    for (const b of clone.querySelectorAll('p')) b.append(document.createTextNode('\n'));
	    parts.push(clone.textContent || '');
	    el = el.nextElementSibling;
	  }
	  let answers = 0;
	  for (let j = ui + 1; j < h4s.length; j++) {
	    if ((h4s[j].textContent || '').trim() === 'ChatGPT said:') { next = true; answers++; }
	  }
	  const h = h4s[ui];
	  const owner = h.closest('[data-message-id], [data-chatgpt-search-message-ids], [data-turn-key]');
	  const userID = owner?.getAttribute('data-message-id') || owner?.getAttribute('data-chatgpt-search-message-ids') || owner?.getAttribute('data-turn-key') || '';
	  return {user: norm(parts.join('\n')), userID, answerNext: next, answers};
	})()`

func originalUserText(tab context.Context, id string) (string, error) {
	var source struct {
		Text     string
		Markdown bool
	}
	js := fmt.Sprintf(`(async () => {
	  const id = %s;
	  if (!/^[a-zA-Z0-9-]+$/.test(id)) return {};
	  const thread = location.pathname.split('/c/')[1];
	  if (!thread) return {};
	  const auth = await (await fetch('/api/auth/session')).json();
	  if (!auth.accessToken) return {};
	  const r = await fetch('/backend-api/conversation/' + encodeURIComponent(thread), {headers:{Authorization:'Bearer ' + auth.accessToken}});
	  if (!r.ok) return {};
	  const m = (await r.json()).mapping?.[id]?.message;
	  if (m?.id !== id || m.author?.role !== 'user' || m.content?.content_type !== 'text' || m.content.parts?.length !== 1 || typeof m.content.parts[0] !== 'string') return {};
	  return {Text:m.content.parts[0], Markdown:m.metadata?.serialization_metadata?.render_format === 'markdown'};
	})()`, strconv.Quote(id))
	if err := chromedp.Run(tab, chromedp.Evaluate(js, &source, chromedp.EvalAsValue, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil {
		return "", err
	}
	if source.Text == "" {
		return "", errors.New("DOM-bound original user message unavailable")
	}
	if !source.Markdown {
		return source.Text, nil
	}
	var rendered bytes.Buffer
	if err := goldmark.New().Convert([]byte(source.Text), &rendered); err != nil {
		return "", err
	}
	if bytes.Contains(rendered.Bytes(), []byte("<img")) {
		return "", errors.New("serialized user source contains non-text image content")
	}
	var text string
	// Detached parse only: no HTML, scripts, links or resources enter the page.
	decode := fmt.Sprintf(`(() => {const d = new DOMParser().parseFromString(%s, 'text/html');for(const e of d.querySelectorAll('br'))e.replaceWith(d.createTextNode('\n'));for(const e of d.querySelectorAll('p'))e.append(d.createTextNode('\n'));return d.body.textContent || '';})()`, strconv.Quote(rendered.String()))
	err := chromedp.Run(tab, chromedp.Evaluate(decode, &text))
	return text, err
}
