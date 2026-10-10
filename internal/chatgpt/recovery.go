package chatgpt

import (
	"context"
	"errors"
	"fmt"
	"github.com/chromedp/chromedp"
	"strings"
	"time"
)

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
	if corr.User == "" || corr.User != normSpace(prompt) {
		return TurnResult{}, fmt.Errorf("stored user prompt does not exactly match original request (rendered %dB want %dB)", len(corr.User), len(normSpace(prompt)))
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
