package chatgpt

import (
	"context"
	"errors"
	"fmt"
	"github.com/chromedp/chromedp"
	"log"
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

// windowAt returns ±40 bytes around off for mismatch evidence logging.
func windowAt(s string, off int) string {
	lo := off - 40
	if lo < 0 {
		lo = 0
	}
	hi := off + 40
	if hi > len(s) {
		hi = len(s)
	}
	return s[lo:hi]
}

// ReadThreadTurn only reads an existing finished turn. It validates against
// the RENDERED page (the same representation the bridge normally extracts)
// instead of the backend mapping API, which proved unreliable: the mapping
// walk disagreed with the visibly rendered turn. The thread's last user
// bubble must match the pending prompt (whitespace-normalized; rendered text
// reflows), and a completed assistant body must follow it. Anything else
// fails closed for a bounded retry or manual inspection.
func (c *Client) ReadThreadTurn(ctx context.Context, url, prompt string) (TurnResult, error) {
	if !threadPattern.MatchString(url) || prompt == "" {
		return TurnResult{}, errors.New("recovery requires a stored thread and exact original prompt")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	tab, err := c.tabCtx(ctx)
	if err != nil {
		return TurnResult{}, err
	}
	tab, cancel := requestContext(ctx, tab)
	defer cancel()
	nav, done := context.WithTimeout(tab, 45*time.Second)
	defer done()
	if err := chromedp.Run(nav, chromedp.Navigate(url)); err != nil {
		return TurnResult{}, err
	}
	if err := c.waitPageReady(ctx, tab, 30*time.Second); err != nil {
		return TurnResult{}, err
	}
	// Correlate in the rendered DOM: the last "You said:" bubble must match
	// the pending prompt (whitespace-normalized; rendered text reflows), and
	// a "ChatGPT said:" section must follow it, so a stale earlier answer can
	// never be adopted. The answer itself comes from responseProbe — the same
	// rendered representation as the normal path.
	var corr struct {
		User       string `json:"user"`
		AnswerNext bool   `json:"answerNext"`
	}
	corrJS := `(() => {
	  const norm = s => (s || '').replace(/\s+/g, ' ').trim();
	  const h4s = [...document.querySelectorAll('h4')];
	  let ui = -1;
	  for (let i = 0; i < h4s.length; i++) {
	    if ((h4s[i].textContent || '').trim() === 'You said:') ui = i;
	  }
	  if (ui < 0) return {user: '', answerNext: false};
	  let el = h4s[ui].nextElementSibling, parts = [], next = false;
	  while (el && el.tagName !== 'H4') {
	    parts.push(el.innerText || '');
	    el = el.nextElementSibling;
	  }
	  for (let j = ui + 1; j < h4s.length; j++) {
	    if ((h4s[j].textContent || '').trim() === 'ChatGPT said:') { next = true; break; }
	  }
	  return {user: norm(parts.join('\n')), answerNext: next};
	})()`
	if err := chromedp.Run(tab, chromedp.Evaluate(corrJS, &corr)); err != nil {
		return TurnResult{}, err
	}
	normSpace := func(s string) string {
		return strings.Join(strings.Fields(s), " ")
	}
	want := normSpace(prompt)
	// ChatGPT collapses long user bubbles with a trailing expander whose
	// label leaks into innerText (proven live: a byte-identical 1068B prompt
	// rendered as 1082B with a 14B " … Show more" suffix). Strip one trailing
	// occurrence before comparing — a submitted prompt never contains it, so
	// the binding to this exact turn stays intact.
	userText := corr.User
	for _, suffix := range []string{" … Show more", " … Show less", "... Show more", "... Show less"} {
		if strings.HasSuffix(userText, suffix) {
			userText = strings.TrimSuffix(userText, suffix)
			break
		}
	}
	if userText == "" || userText != want {
		// Evidence for the inevitable near-miss: log WHERE the rendered
		// bubble first diverges (byte offset + surrounding windows) so the
		// transform can be identified instead of guessed. Still fails
		// closed — this changes no decision, only debuggability.
		off := firstDiffByte(userText, want)
		log.Printf("[chatgpt] adoption user-bubble mismatch rendered=%dB want=%dB diffoff=%d renderedctx=%q wantctx=%q", len(userText), len(want), off, windowAt(userText, off), windowAt(want, off))
		return TurnResult{}, fmt.Errorf("stored user prompt does not exactly match original request (rendered %dB want %dB)", len(userText), len(want))
	}
	if !corr.AnswerNext {
		return TurnResult{}, errors.New("current response is not finished and nonempty")
	}
	visible, err := responseProbe(tab, nil)
	if err != nil {
		return TurnResult{}, err
	}
	if visible.Last == "" || visible.StopBtn || visible.ErrorText {
		return TurnResult{}, errors.New("finished stored response is not identically rendered; inspect before recovery")
	}
	return TurnResult{Answer: visible.Last, Reasoning: visible.Reasoning, URL: url}, nil
}
