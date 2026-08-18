// Package chatgpt drives chatgpt.com in a dedicated Chrome profile via CDP
// so that AI agents can use a ChatGPT Web session (no API key, no cost)
// through a plain ask/answer interface.
//
// The client owns one Chrome instance (launched on demand with its own
// profile directory) and one tab. All operations are serialized through a
// mutex because they share that single tab.
package chatgpt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// Defaults.
const (
	DefaultChromePort = 9222
	DefaultTimeout    = 10 * time.Minute
	HomePage          = "https://chatgpt.com/"
)

// Errors returned by Ask/Status. Callers can use errors.Is to give agents
// actionable messages.
var (
	ErrChromeNotFound = errors.New("chrome binary not found")
	ErrChromeLaunch   = errors.New("failed to launch chrome")
	ErrNotLoggedIn    = errors.New("not logged in to chatgpt.com")
	ErrNoComposer     = errors.New("chatgpt composer (#prompt-textarea) not found")
	ErrNoNewChat      = errors.New("could not find the New chat button")
	ErrNoResponse     = errors.New("chatgpt did not produce a response in time")
)

// Status describes the current state of the ChatGPT Web tab.
type Status struct {
	LoggedIn   bool   `json:"logged_in"`
	Composer   bool   `json:"composer"`
	NewChat    bool   `json:"new_chat_button"`
	LoginBtn   bool   `json:"login_button"`
	URL        string `json:"url"`
	Title      string `json:"title"`
	Profile    string `json:"chrome_profile"`
	ChromePort int    `json:"chrome_port"`
}

// Client owns the Chrome instance and the shared tab.
type Client struct {
	ChromePort int
	ProfileDir string
	ChromePath string // optional override; auto-detected when empty
	Background bool   // run Chrome window in background (off-screen / minimized)

	mu              sync.Mutex
	life            context.Context // long-lived parent for browser/tab contexts
	browser         context.Context
	cancel          context.CancelFunc
	tab             context.Context
	tabDone         context.CancelFunc
	launchAttempted bool
	launchPort      int
	actualPort      int
}

// New builds a client with defaults for anything zero-valued.
func New(port int, profileDir, chromePath string) *Client {
	if port == 0 {
		port = DefaultChromePort
	}
	if profileDir == "" {
		dir, err := os.UserHomeDir()
		if err == nil && dir != "" {
			profileDir = filepath.Join(dir, ".chatgpt-proxy")
		} else {
			profileDir = ".chatgpt-proxy"
		}
	}
	return &Client{ChromePort: port, ProfileDir: profileDir, ChromePath: chromePath,
		life: context.Background()}
}

// Close shuts down the CDP connection (not the Chrome process — the user's
// session stays alive in the profile for the next run).
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tabDone != nil {
		c.tabDone()
		c.tabDone = nil
	}
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	c.tab = nil
	c.browser = nil
}

// --- browser lifecycle -------------------------------------------------------

// ensureBrowser attaches to a running Chrome on the debug port, or launches
// one with the dedicated profile. Safe to call repeatedly — once launched,
// the same port is reused and never relaunched while this process lives.
func (c *Client) ensureBrowser(ctx context.Context) error {
	if c.browser != nil {
		return nil
	}
	// 1. Attach to an already-running DevTools server (e.g. from a previous
	//    run of this proxy) before launching anything.
	for port := c.scanStart(); port < c.scanStart()+10; port++ {
		wsURL, err := c.attach(ctx, port)
		if err == nil {
			c.actualPort = port
			return c.connect(wsURL)
		}
	}
	// 2. None found — launch once on a free port and poll until it comes up.
	port := c.resolvePort()
	if !c.launchAttempted {
		if err := c.launch(ctx, port); err != nil {
			return err
		}
		c.launchAttempted = true
		c.launchPort = port
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		wsURL, err := c.attach(ctx, c.launchPort)
		if err == nil {
			c.actualPort = c.launchPort
			return c.connect(wsURL)
		}
		if time.Now().After(deadline) {
			c.launchAttempted = false // allow a fresh launch on the next call
			return fmt.Errorf("chrome started but debugging port %d never came up: %w", c.launchPort, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// scanStart is the first port to probe for an existing DevTools server.
func (c *Client) scanStart() int {
	if c.ChromePort != 0 && c.ChromePort != DefaultChromePort {
		return c.ChromePort
	}
	return DefaultChromePort
}

// connect wires up the chromedp allocator for a found debugger URL. The
// allocator is rooted at c.life (not the per-call context) so the browser
// connection survives across individual MCP requests.
func (c *Client) connect(wsURL string) error {
	browserCtx, cancel := chromedp.NewRemoteAllocator(c.life, wsURL)
	c.browser = browserCtx
	c.cancel = cancel
	return nil
}

// resolvePort picks the debug port to use. An explicit (non-default) port is
// respected; the default scans for the first free port so a stray listener on
// 9222 (e.g. another tool) can't wedge the proxy.
func (c *Client) resolvePort() int {
	if c.ChromePort != 0 && c.ChromePort != DefaultChromePort {
		return c.ChromePort
	}
	for p := DefaultChromePort; p < DefaultChromePort+10; p++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			ln.Close()
			return p
		}
	}
	return DefaultChromePort
}

// attach returns the WebSocket debugger URL of an already-running Chrome.
// Bounded by a short timeout so a dead or foreign listener can't hang us.
func (c *Client) attach(ctx context.Context, port int) (string, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/json/version", port)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var v struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", err
	}
	if v.WebSocketDebuggerURL == "" {
		return "", errors.New("no webSocketDebuggerUrl in /json/version")
	}
	return v.WebSocketDebuggerURL, nil
}

// launch starts Chrome with the dedicated profile and the debug port,
// detached from this process.
func (c *Client) launch(ctx context.Context, port int) error {
	bin := c.ChromePath
	if bin == "" {
		bin = findChrome()
		if bin == "" {
			return ErrChromeNotFound
		}
	}
	if err := os.MkdirAll(c.ProfileDir, 0o755); err != nil {
		return err
	}
	args := []string{
		"--remote-debugging-port=" + strconv.Itoa(port),
		"--user-data-dir=" + c.ProfileDir,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-session-crashed-bubble",
		"--disable-features=Translate",
	}
	if c.Background {
		args = append(args,
			"--window-position=-2400,-2400",
			"--window-size=1280,800",
			"--start-minimized",
		)
	}
	args = append(args, HomePage)
	cmd := exec.Command(bin, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%w: %v", ErrChromeLaunch, err)
	}
	// Detach: the Chrome process outlives this one.
	go cmd.Wait() //nolint:errcheck
	return nil
}

func findChrome() string {
	candidates := []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
		"/Applications/Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary",
		"/usr/bin/google-chrome",
		"/usr/bin/google-chrome-stable",
		"/usr/bin/chromium",
		"/usr/bin/chromium-browser",
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// tabCtx returns the shared tab context, creating it on first use.
// Callers must already hold c.mu (the public methods do).
func (c *Client) tabCtx(ctx context.Context) (context.Context, error) {
	if c.tab == nil {
		if err := c.ensureBrowser(ctx); err != nil {
			return nil, err
		}
		// Look for an existing chatgpt.com page target before creating a blank one
		infos, err := chromedp.Targets(c.browser)
		var targetID target.ID
		if err == nil {
			for _, info := range infos {
				if info.Type == "page" && strings.Contains(info.URL, "chatgpt.com") {
					targetID = info.TargetID
					break
				}
			}
		}
		if targetID != "" {
			c.tab, c.tabDone = chromedp.NewContext(c.browser, chromedp.WithTargetID(targetID))
		} else {
			c.tab, c.tabDone = chromedp.NewContext(c.browser)
		}
		// Force browser allocation on the long-lived tab context.
		if err := chromedp.Run(c.tab, chromedp.ActionFunc(func(context.Context) error { return nil })); err != nil {
			return nil, fmt.Errorf("alloc: %w", err)
		}
	}
	return c.tab, nil
}

// --- public API --------------------------------------------------------------

// Status opens (or attaches to) Chrome, loads chatgpt.com and reports the
// state of the page — mainly whether the user is logged in.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	tab, err := c.tabCtx(ctx)
	if err != nil {
		return nil, err
	}
	navCtx, cancel := context.WithTimeout(tab, 45*time.Second)
	defer cancel()
	if err := chromedp.Run(navCtx, chromedp.Navigate(HomePage)); err != nil {
		return nil, fmt.Errorf("navigate: %w", err)
	}
	// Give the SPA a moment to render.
	time.Sleep(1500 * time.Millisecond)

	return c.probeState(ctx, tab)
}

// LoginState probes the current page without navigating — safe to call
// repeatedly while the user is mid-login in the Chrome window.
func (c *Client) LoginState(ctx context.Context) (*Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	tab, err := c.tabCtx(ctx)
	if err != nil {
		return nil, err
	}
	return c.probeState(ctx, tab)
}

func (c *Client) probeState(ctx context.Context, tab context.Context) (*Status, error) {
	port := c.actualPort
	if port == 0 {
		port = c.ChromePort
	}
	s := &Status{Profile: c.ProfileDir, ChromePort: port}
	probe := `JSON.stringify((() => {
	  const q = (sel) => !!document.querySelector(sel);
	  return {
	    composer: q('#prompt-textarea'),
	    sidebar: q('#sidebar'),
	    loginBtn: q('[data-testid="login-button"]'),
	    newChat: q('a[aria-label="New chat"], button[aria-label="New chat"], a[href="/"]'),
	    url: location.href,
	    title: document.title,
	  };
	})())`
	var raw string
	if err := chromedp.Run(tab, chromedp.Evaluate(probe, &raw)); err != nil {
		return nil, fmt.Errorf("probe: %w", err)
	}
	var p struct {
		Composer bool   `json:"composer"`
		Sidebar  bool   `json:"sidebar"`
		LoginBtn bool   `json:"loginBtn"`
		NewChat  bool   `json:"newChat"`
		URL      string `json:"url"`
		Title    string `json:"title"`
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, fmt.Errorf("probe decode: %w", err)
	}
	s.Composer = p.Composer
	s.NewChat = p.NewChat
	s.LoginBtn = p.LoginBtn
	s.URL = p.URL
	s.Title = p.Title
	// Logged in = composer or sidebar present and no login button.
	s.LoggedIn = (p.Composer || p.Sidebar) && !p.LoginBtn
	return s, nil
}

// Ask sends a prompt to ChatGPT Web and returns the final assistant answer.
// When newSession is true the conversation is reset first (default behavior —
// deterministic, isolated calls). timeout bounds the whole call.
func (c *Client) Ask(ctx context.Context, prompt string, newSession bool, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	tab, err := c.tabCtx(ctx)
	if err != nil {
		return "", err
	}

	// 1. If starting a new session, navigate to HomePage
	if newSession {
		log.Printf("[chatgpt] starting fresh session at %s", HomePage)
		navCtx, cancel := context.WithTimeout(tab, 45*time.Second)
		defer cancel()
		if err := chromedp.Run(navCtx, chromedp.Navigate(HomePage)); err != nil {
			return "", fmt.Errorf("navigate: %w", err)
		}
	}

	// 2. Wait up to 30s for the SPA to finish loading & hydrating
	log.Printf("[chatgpt] waiting for page ready...")
	if err := c.waitPageReady(ctx, tab, 30*time.Second); err != nil {
		return "", err
	}

	// 3. Type the prompt.
	log.Printf("[chatgpt] typing prompt (%d bytes)...", len(prompt))
	if err := c.typePrompt(ctx, tab, prompt); err != nil {
		return "", err
	}

	// 4. Submit.
	log.Printf("[chatgpt] submitting prompt...")
	if err := c.submit(ctx, tab); err != nil {
		return "", err
	}

	// 5. Wait for the answer.
	log.Printf("[chatgpt] waiting for answer (timeout: %v)...", timeout)
	return c.waitForAnswer(ctx, tab, timeout)
}

func (c *Client) waitPageReady(ctx context.Context, tab context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		checkCtx, cancel := context.WithTimeout(tab, 3*time.Second)
		var ready bool
		js := `(() => {
		  if (document.readyState !== 'complete') return false;
		  const el = document.querySelector('#prompt-textarea') || document.querySelector('textarea#prompt-textarea');
		  if (!el) return false;
		  const rect = el.getBoundingClientRect();
		  return rect.width > 0 && rect.height > 0;
		})()`
		_ = chromedp.Run(checkCtx, chromedp.Evaluate(js, &ready))
		cancel()
		if ready {
			// Grace period for React hydration / event listener attachment
			time.Sleep(2 * time.Second)
			return nil
		}
		time.Sleep(1 * time.Second)
	}
	return ErrNoComposer
}

func (c *Client) startNewChat(ctx context.Context, tab context.Context) error {
	clickCtx, cancel := context.WithTimeout(tab, 20*time.Second)
	defer cancel()
	clickJS := `(() => {
	  if (location.pathname === '/' || location.pathname === '') return true;
	  const sels = ['a[aria-label="New chat"]', 'button[aria-label="New chat"]', 'a[href="/"]'];
	  for (const sel of sels) {
	    const el = document.querySelector(sel);
	    if (el) { el.click(); return true; }
	  }
	  const all = [...document.querySelectorAll('a, button')];
	  const el = all.find(e => (e.textContent || '').trim().replace(/\s+/g, ' ') === 'New chat');
	  if (el) { el.click(); return true; }
	  location.href = 'https://chatgpt.com/';
	  return true;
	})()`
	var ok bool
	if err := chromedp.Run(clickCtx, chromedp.Evaluate(clickJS, &ok)); err != nil {
		return fmt.Errorf("new chat: %w", err)
	}
	// Wait for composer to be ready on the fresh session
	return c.waitPageReady(ctx, tab, 20*time.Second)
}

func (c *Client) typePrompt(ctx context.Context, tab context.Context, prompt string) error {
	// Wait for the composer to render — after a Navigate the SPA can take a
	// moment, and failing immediately breaks follow-ups (new_session=false).
	deadline := time.Now().Add(30 * time.Second)
	for {
		checkCtx, cancel := context.WithTimeout(tab, 5*time.Second)
		var ready bool
		err := chromedp.Run(checkCtx, chromedp.Evaluate(
			`!!(document.querySelector('#prompt-textarea') || document.querySelector('textarea#prompt-textarea'))`, &ready))
		cancel()
		if err == nil && ready {
			break
		}
		if time.Now().After(deadline) {
			return ErrNoComposer
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Use ClipboardEvent paste with DataTransfer for instant, non-blocking
	// insertion in modern ProseMirror contenteditable (especially for large prompts),
	// with fallbacks for standard textarea and execCommand.
	typeCtx, cancel := context.WithTimeout(tab, 30*time.Second)
	defer cancel()
	js := fmt.Sprintf(`(() => {
	  const t = %s;
	  const el = document.querySelector('#prompt-textarea') || document.querySelector('textarea#prompt-textarea');
	  if (!el) return false;
	  el.focus();
	  try {
	    const dt = new DataTransfer();
	    dt.setData('text/plain', t);
	    const pasteEvent = new ClipboardEvent('paste', {
	      bubbles: true,
	      cancelable: true,
	      clipboardData: dt
	    });
	    el.dispatchEvent(pasteEvent);
	    return true;
	  } catch (e) {}
	  if (el.tagName === 'TEXTAREA') {
	    el.value = t;
	    el.dispatchEvent(new Event('input', { bubbles: true }));
	    return true;
	  }
	  document.execCommand('insertText', false, t);
	  return true;
	})()`, strconv.Quote(prompt))
	var ok bool
	if err := chromedp.Run(typeCtx, chromedp.Evaluate(js, &ok)); err != nil {
		return fmt.Errorf("type prompt: %w", err)
	}
	if !ok {
		return ErrNoComposer
	}
	return nil
}

func (c *Client) submit(ctx context.Context, tab context.Context) error {
	// Wait up to 5s for the send button to become enabled after typing/pasting.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		clickCtx, cancel := context.WithTimeout(tab, 2*time.Second)
		var clicked bool
		js := `(() => {
		  const btn = document.querySelector('[data-testid="send-button"]');
		  if (btn && !btn.disabled) {
		    btn.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true, cancelable: true }));
		    btn.dispatchEvent(new MouseEvent("mousedown", { bubbles: true, cancelable: true }));
		    btn.dispatchEvent(new MouseEvent("mouseup", { bubbles: true, cancelable: true }));
		    btn.click();
		    return true;
		  }
		  return false;
		})()`
		_ = chromedp.Run(clickCtx, chromedp.Evaluate(js, &clicked))
		cancel()
		if clicked {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	keyCtx, cancel2 := context.WithTimeout(tab, 5*time.Second)
	defer cancel2()
	return chromedp.Run(keyCtx, chromedp.KeyEvent("\r"))
}

// waitForAnswer polls the page until the assistant reply is stable (or the
// stop button disappears), handling the "Continue generating" button.
func (c *Client) waitForAnswer(ctx context.Context, tab context.Context, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	lastText := ""
	stablePolls := 0
	continues := 0

	stateJS := `JSON.stringify((() => {
	  const q = (sel) => document.querySelector(sel);
	  const msgs = document.querySelectorAll('[data-message-author-role="assistant"]');
	  let last = '';
	  if (msgs.length) {
	    const m = msgs[msgs.length - 1];
	    const md = m.querySelector('.markdown');
	    last = (md || m).innerText || '';
	  }
	  if (!last) {
	    const allMd = document.querySelectorAll('.markdown');
	    if (allMd.length > 1) {
	      last = allMd[allMd.length - 1].innerText || '';
	    }
	  }
	  const alertEl = q('[role="alert"], [data-testid="error-notification"], .text-token-text-error');
	  const alertText = alertEl ? (alertEl.innerText || '') : '';
	  return {
	    stopBtn: !!q('[data-testid="stop-button"]'),
	    continueBtn: !!q('[data-testid="continue-generate-button"]'),
	    errorText: alertText.includes('Something went wrong') || alertText.includes('There was an error'),
	    lastAssistant: last,
	  };
	})())`

	for time.Now().Before(deadline) {
		var raw string
		if err := chromedp.Run(tab, chromedp.Evaluate(stateJS, &raw)); err != nil {
			return "", fmt.Errorf("poll: %w", err)
		}
		var st struct {
			StopBtn     bool   `json:"stopBtn"`
			ContinueBtn bool   `json:"continueBtn"`
			ErrorText   bool   `json:"errorText"`
			Last        string `json:"lastAssistant"`
		}
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			return "", fmt.Errorf("poll decode: %w", err)
		}
		log.Printf("[chatgpt] poll: stop=%v, cont=%v, err=%v, len=%d", st.StopBtn, st.ContinueBtn, st.ErrorText, len(st.Last))

		if st.ErrorText && st.Last == "" {
			return "", errors.New("chatgpt reported an error on the page")
		}
		if st.StopBtn {
			stablePolls = 0
			time.Sleep(10 * time.Second)
			continue
		}
		if st.ContinueBtn && st.Last != "" && st.Last == lastText {
			if continues >= 3 {
				return st.Last, nil // good enough
			}
			continues++
			clickCtx, cancel := context.WithTimeout(tab, 5*time.Second)
			_ = chromedp.Run(clickCtx, chromedp.Evaluate(
				`document.querySelector('[data-testid="continue-generate-button"]')?.click()`, nil))
			cancel()
			time.Sleep(2 * time.Second)
			continue
		}
		if st.Last == lastText && st.Last != "" {
			stablePolls++
			if stablePolls >= 2 {
				return strings.TrimSpace(st.Last), nil
			}
		} else {
			lastText = st.Last
			stablePolls = 0
		}
		time.Sleep(5 * time.Second)
	}
	if lastText != "" {
		return strings.TrimSpace(lastText), nil
	}
	return "", ErrNoResponse
}
