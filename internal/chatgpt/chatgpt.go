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
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/gorilla/websocket"
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
	ErrChromeProfile  = errors.New("debugging endpoint cannot prove the configured Chrome profile; start the dedicated Chrome with --enable-automation on the configured port")
	ErrNotLoggedIn    = errors.New("not logged in to chatgpt.com")
	ErrNoComposer     = errors.New("chatgpt composer not found (no #prompt-textarea or ProseMirror contenteditable)")
	ErrNoNewChat      = errors.New("could not find the New chat button")
	ErrNoResponse     = errors.New("chatgpt did not produce a response in time")
	ErrMissingAnswer  = errors.New("ChatGPT generation ended without an extractable new assistant answer; inspect stored thread/rendering and recover the pending turn; no automatic resubmission")
	ErrProjectSetup   = errors.New("project setup failed before turn submission")
	ErrNotSubmitted   = errors.New("turn failed before any submission action")
)

// IsNavigateError reports a page-navigation or explicit project-setup failure. All
// navigate steps run before anything is typed or submitted, so a caller that
// sees only such an error (and reached no conversation URL) knows provably
// that nothing was sent and may safely retry instead of treating the turn
// as uncertain.
func IsNavigateError(err error) bool {
	return err != nil && (errors.Is(err, ErrProjectSetup) || strings.Contains(err.Error(), "navigate:"))
}

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
	ChromePort   int
	ProfileDir   string
	ChromePath   string // optional override; auto-detected when empty
	Background   bool   // run Chrome window in background (off-screen / minimized)
	DedicatedTab bool   // do not reuse the MCP/user's existing ChatGPT tab
	SetupLocks   bool   // coordinate only setup when using owned parallel targets

	mu      sync.Mutex
	life    context.Context // long-lived parent for browser/tab contexts
	browser context.Context
	cancel  context.CancelFunc
	// wsURL is the debugger endpoint the allocator is bound to. A Chrome
	// restart mints a new endpoint UUID, so it identifies staleness.
	wsURL           string
	tab             context.Context
	tabDone         context.CancelFunc
	launchAttempted bool
	launchPort      int
	actualPort      int
	pool            *Pool
	ownedTarget     target.ID
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
	c.teardownLocked()
}

// teardownLocked drops the tab and allocator contexts. Callers must hold c.mu.
// Nil-safe: double teardown is a no-op.
func (c *Client) teardownLocked() {
	if c.ownedTarget != "" {
		// Only explicit newly-created targets are ours to close. Context
		// cancellation of an attached target merely detaches its CDP session.
		client := http.Client{Timeout: 2 * time.Second}
		if resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/json/close/%s", c.actualPort, c.ownedTarget)); err == nil {
			resp.Body.Close()
		}
		c.ownedTarget = ""
	}
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
	c.wsURL = ""
}

// tabAlive exercises the full CDP chain (tab session -> allocator websocket)
// with a trivial read. Comparing /json/version UUIDs cannot catch a silently
// dropped websocket to a still-live Chrome — every later Run then fails
// instantly with "context canceled" forever, because the cached tab path
// never re-verifies. So ping the real thing. Short timeout: a wedged
// connection must read as dead, never hang the turn.
func (c *Client) tabAlive() bool {
	if c.tab == nil || c.browser == nil {
		return false
	}
	probe, cancel := context.WithTimeout(c.tab, 5*time.Second)
	defer cancel()
	var ok bool
	if err := chromedp.Run(probe, chromedp.Evaluate(`true`, &ok)); err != nil {
		return false
	}
	return ok
}

// --- browser lifecycle -------------------------------------------------------

// ensureBrowser attaches to a running Chrome on the debug port, or launches
// one with the dedicated profile. Safe to call repeatedly — once launched,
// the same port is reused and never relaunched while this process lives.
func (c *Client) ensureBrowser(ctx context.Context) error {
	if c.pool != nil {
		return c.pool.connectClient(ctx, c)
	}
	if c.browser != nil {
		if c.browserAlive() {
			return nil
		}
		// Stale allocator: Chrome restarted out from under us, which cancels
		// the old connection. Every CDP call on it fails instantly with
		// "context canceled", so tear down and fall through to attach-or-
		// launch instead of failing every turn until process restart.
		log.Printf("[chatgpt] browser connection stale; re-attaching")
		c.teardownLocked()
	}
	// 1. Attach to an already-running DevTools server (e.g. from a previous
	//    run of this proxy) before launching anything.
	port := c.resolvePort()
	wsURL, err := c.attach(ctx, port)
	if err == nil {
		c.actualPort = port
		return c.connect(wsURL)
	}
	if errors.Is(err, ErrChromeProfile) || ctx.Err() != nil {
		return err
	}
	// 2. Configured endpoint absent — launch only on that exact port.
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
		if errors.Is(err, ErrChromeProfile) || ctx.Err() != nil {
			return err
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
	c.wsURL = wsURL
	return nil
}

// browserAlive reports whether the allocator still points at the live
// browser by comparing /json/version's endpoint with the one we connected
// to. A Chrome restart mints a new endpoint UUID, so any difference (or any
// fetch failure) means stale. A short client timeout keeps a wedged listener
// from hanging us: worst case we relaunch unnecessarily, which is safe.
func (c *Client) browserAlive() bool {
	if c.wsURL == "" {
		return false
	}
	port := c.actualPort
	if port == 0 {
		port = c.ChromePort
	}
	if port == 0 {
		port = DefaultChromePort
	}
	httpClient := http.Client{Timeout: 5 * time.Second}
	resp, err := httpClient.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var v struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return false
	}
	return v.WebSocketDebuggerURL != "" && v.WebSocketDebuggerURL == c.wsURL
}

// resolvePort keeps browser identity on the configured endpoint. Adjacent
// listeners may belong to another provider/profile and must never be adopted.
func (c *Client) resolvePort() int {
	if c.ChromePort != 0 {
		return c.ChromePort
	}
	return DefaultChromePort
}

// attach returns the WebSocket debugger URL of an already-running Chrome.
// Bounded by a short timeout so a dead or foreign listener can't hang us.
func (c *Client) attach(ctx context.Context, port int) (string, error) {
	versionURL := fmt.Sprintf("http://127.0.0.1:%d/json/version", port)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, versionURL, nil)
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
		return "", fmt.Errorf("%w: invalid version response", ErrChromeProfile)
	}
	if v.WebSocketDebuggerURL == "" {
		return "", fmt.Errorf("%w: missing browser endpoint", ErrChromeProfile)
	}
	endpoint, err := url.Parse(v.WebSocketDebuggerURL)
	if err != nil || endpoint.Scheme != "ws" || (endpoint.Hostname() != "127.0.0.1" && endpoint.Hostname() != "localhost") || endpoint.Port() != strconv.Itoa(port) || !strings.HasPrefix(endpoint.Path, "/devtools/browser/") {
		return "", fmt.Errorf("%w: unexpected browser endpoint", ErrChromeProfile)
	}
	// This browser-level query is read-only: never create/adopt a target until
	// Chrome proves both its profile and port. Without --enable-automation the
	// command is unavailable; fail closed rather than trusting a static label.
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, v.WebSocketDebuggerURL, nil)
	if err != nil {
		return "", fmt.Errorf("%w: browser verification unavailable", ErrChromeProfile)
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	_ = conn.SetReadDeadline(deadline)
	_ = conn.SetWriteDeadline(deadline)
	if err := conn.WriteJSON(map[string]any{"id": 1, "method": "Browser.getBrowserCommandLine"}); err != nil {
		return "", fmt.Errorf("%w: browser verification unavailable", ErrChromeProfile)
	}
	var command struct {
		ID    int `json:"id"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Result struct {
			Arguments []string `json:"arguments"`
		} `json:"result"`
	}
	for command.ID != 1 {
		if err := conn.ReadJSON(&command); err != nil {
			return "", fmt.Errorf("%w: browser verification unavailable", ErrChromeProfile)
		}
	}
	profileOK, portOK := false, false
	wantProfile, err := filepath.Abs(c.ProfileDir)
	if err != nil {
		return "", ErrChromeProfile
	}
	// Older dedicated Chrome may not expose command-line CDP without
	// --enable-automation. In that ONE case require independent OS proof:
	// exact TCP listener PID, exact executable/profile/port args, and the same
	// PID as this profile's singleton owner. No missing evidence is trusted.
	if command.Error.Code == -32000 && command.Error.Message == "Command line not returned because --enable-automation not set." {
		listeners, err := exec.CommandContext(ctx, "lsof", "-nP", "-a", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN", "-Fp").Output()
		if err != nil {
			return "", fmt.Errorf("%w: listener owner unavailable", ErrChromeProfile)
		}
		var pids []string
		for _, field := range strings.Fields(string(listeners)) {
			if strings.HasPrefix(field, "p") {
				pids = append(pids, field)
			}
		}
		if len(pids) != 1 || !regexp.MustCompile(`^p[1-9][0-9]*$`).MatchString(pids[0]) {
			return "", fmt.Errorf("%w: listener owner ambiguous", ErrChromeProfile)
		}
		pid := pids[0][1:]
		lock, err := os.Readlink(filepath.Join(wantProfile, "SingletonLock"))
		if err != nil || !strings.HasSuffix(lock, "-"+pid) {
			return "", fmt.Errorf("%w: listener is not the profile singleton owner", ErrChromeProfile)
		}
		argv, err := exec.CommandContext(ctx, "ps", "-ww", "-p", pid, "-o", "command=").Output()
		bin := c.ChromePath
		if bin == "" {
			bin = findChrome()
		}
		line := strings.TrimSpace(string(argv))
		profileFlag := "--user-data-dir=" + wantProfile
		portFlag := "--remote-debugging-port=" + strconv.Itoa(port)
		if err != nil || bin == "" || !strings.HasPrefix(line, bin+" ") ||
			strings.Count(line, "--user-data-dir=") != 1 || strings.Count(line, "--remote-debugging-port=") != 1 ||
			!regexp.MustCompile(`(?:^|\s)`+regexp.QuoteMeta(profileFlag)+`(?:\s|$)`).MatchString(line) ||
			!regexp.MustCompile(`(?:^|\s)`+regexp.QuoteMeta(portFlag)+`(?:\s|$)`).MatchString(line) {
			return "", fmt.Errorf("%w: listener executable/profile/port mismatch", ErrChromeProfile)
		}
		command.Result.Arguments = []string{profileFlag, portFlag}
	}
	for _, arg := range command.Result.Arguments {
		if strings.HasPrefix(arg, "--user-data-dir=") {
			actual, err := filepath.Abs(strings.TrimPrefix(arg, "--user-data-dir="))
			profileOK = err == nil && actual == wantProfile
		}
		if strings.HasPrefix(arg, "--remote-debugging-port=") {
			portOK = arg == "--remote-debugging-port="+strconv.Itoa(port)
		}
	}
	if !profileOK || !portOK {
		return "", fmt.Errorf("%w: browser profile or port mismatch", ErrChromeProfile)
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
	// Never forward launch arguments into an existing non-debuggable Chrome
	// using this profile, or remove a potentially live singleton lock.
	for _, name := range []string{"SingletonLock", "SingletonSocket"} {
		if _, err := os.Lstat(filepath.Join(c.ProfileDir, name)); err == nil {
			return fmt.Errorf("%w: dedicated profile is already locked; inspect its owning Chrome and start its exact configured debugging port; no lock was removed", ErrChromeLaunch)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	args := []string{
		"--enable-automation",
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
	if c.tab != nil {
		// The cached tab is the common path, so verify it here — not just
		// in ensureBrowser. A silently dropped websocket to a live Chrome
		// keeps the UUID check green while every Run fails instantly, so
		// probe the real chain and rebuild on failure instead of failing
		// every turn until process restart.
		if c.tabAlive() {
			return c.tab, nil
		}
		log.Printf("[chatgpt] tab connection dead; re-attaching")
		c.teardownLocked()
	}
	if c.SetupLocks && c.pool == nil {
		unlock, err := LockSetup(ctx, c.ProfileDir)
		if err != nil {
			return nil, err
		}
		defer unlock()
	}
	if err := c.ensureBrowser(ctx); err != nil {
		return nil, err
	}
	if c.SetupLocks && c.pool != nil {
		unlock, err := LockSetup(ctx, c.ProfileDir)
		if err != nil {
			return nil, err
		}
		defer unlock()
	}
	// A freshly launched browser answers /json/version before its first
	// window exists, and Target.createTarget fails until then ("no browser
	// is open") — so retry instead of failing fast.
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for {
		tid, err := c.findOrCreateTab()
		if err == nil {
			tab, done := chromedp.NewContext(c.browser, chromedp.WithTargetID(tid))
			if aerr := chromedp.Run(tab, chromedp.ActionFunc(func(context.Context) error { return nil })); aerr == nil {
				c.tab, c.tabDone = tab, done
				return c.tab, nil
			} else {
				done()
				lastErr = aerr
			}
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("alloc: %w", lastErr)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// findOrCreateTab reuses an existing chatgpt.com page, or opens one through
// the DevTools HTTP API — which reliably creates a window even when none
// exists, unlike Target.createTarget over the websocket (fails with "no
// browser is open" when every window is closed). HTTP is also used for
// listing: chromedp.Targets needs a full tab context, which is what we're
// trying to build here.
func (c *Client) findOrCreateTab() (target.ID, error) {
	port := c.actualPort
	if port == 0 {
		port = c.ChromePort
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	var infos []struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		URL  string `json:"url"`
	}
	if err := httpGetJSON(base+"/json/list", &infos); err != nil {
		return "", err
	}
	// A newly restarted MCP client must not adopt the adapter's newest tab.
	// Target IDs survive navigation; a stale ID after Chrome exits is harmless.
	privateID, err := os.ReadFile(filepath.Join(c.ProfileDir, "openai-tab-id"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	for _, info := range infos {
		if c.DedicatedTab {
			break
		}
		if info.ID != string(privateID) && info.Type == "page" && strings.Contains(info.URL, "chatgpt.com") {
			return target.ID(info.ID), nil
		}
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := httpPutJSON(base+"/json/new?"+HomePage, &created); err != nil {
		return "", err
	}
	if created.ID == "" {
		return "", errors.New("empty target id from /json/new")
	}
	c.ownedTarget = target.ID(created.ID)
	if c.DedicatedTab && c.pool == nil {
		// Pool workers each own a short-lived target and must not clobber
		// the shared file: it would point the next restart at an already-
		// closed target. Owned clients never adopt by ID anyway.
		if err := os.MkdirAll(c.ProfileDir, 0700); err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(c.ProfileDir, "openai-tab-id"), []byte(created.ID), 0600); err != nil {
			return "", err
		}
	}
	return target.ID(created.ID), nil
}

func httpGetJSON(url string, v any) error {
	reqCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}

func httpPutJSON(url string, v any) error {
	reqCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPut, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
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
	tab, cancel := requestContext(ctx, tab)
	defer cancel()
	port := c.actualPort
	if port == 0 {
		port = c.ChromePort
	}
	s := &Status{Profile: c.ProfileDir, ChromePort: port}
	probe := `JSON.stringify((() => {
	  const q = (sel) => !!document.querySelector(sel);
	  const composer = () => !!(document.querySelector('#prompt-textarea')
	    || document.querySelector('form[data-chatgpt-composer] div[contenteditable="true"]')
	    || document.querySelector('.ProseMirror[contenteditable="true"]'));
	  return {
	    composer: composer(),
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
	return c.ask(ctx, prompt, newSession, timeout, 5*time.Second, nil)
}

// AskStream is Ask with live progress: onProgress is called with the full
// answer-so-far each time the polled text grows. Use it to stream deltas to
// an SSE client (diff against the previous value to get the new slice).
func (c *Client) AskStream(ctx context.Context, prompt string, newSession bool, timeout time.Duration, onProgress func(string)) (string, error) {
	return c.ask(ctx, prompt, newSession, timeout, 2*time.Second, onProgress)
}

func (c *Client) ask(ctx context.Context, prompt string, newSession bool, timeout time.Duration, pollInterval time.Duration, onProgress func(string)) (string, error) {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, timeoutCancel := context.WithTimeout(ctx, timeout)
	defer timeoutCancel()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	page := ""
	if newSession {
		page = HomePage
	}
	res, err := c.askLocked(ctx, TurnOptions{Prompt: prompt}, page, timeout, pollInterval, onProgress)
	return res.Answer, err
}

// sameURL reports whether two URLs address the same page, ignoring hash
// and trailing slash.
func sameURL(a, b string) bool {
	trim := func(s string) string {
		if i := strings.Index(s, "#"); i >= 0 {
			s = s[:i]
		}
		return strings.TrimSuffix(s, "/")
	}
	return trim(a) == trim(b)
}

// askLocked owns navigation through URL capture under one browser transaction.
func (c *Client) askLocked(ctx context.Context, opt TurnOptions, page string, timeout, pollInterval time.Duration, onProgress func(string)) (out TurnResult, err error) {
	submissionAttempted := false
	defer func() {
		if err != nil && !submissionAttempted {
			err = errors.Join(ErrNotSubmitted, err)
		}
	}()
	tab, err := c.tabCtx(ctx)
	if err != nil {
		return out, err
	}
	tab, cancel := requestContext(ctx, tab)
	defer cancel()

	// 1. Navigate to the target page, unless the tab is already there. A
	// same-URL Navigate is a full reload: wasted seconds plus a fresh React
	// mount on every turn. Consecutive turns on one thread skip it; the
	// readiness check below still verifies the composer before typing.
	if page != "" {
		var current string
		if err := chromedp.Run(tab, chromedp.Evaluate(`location.href`, &current)); err == nil && sameURL(current, page) {
			log.Printf("[chatgpt] already on %s, skipping reload", page)
		} else {
			log.Printf("[chatgpt] navigating to %s", page)
			navCtx, cancel := context.WithTimeout(tab, 120*time.Second)
			defer cancel()
			if err := chromedp.Run(navCtx, chromedp.Navigate(page)); err != nil {
				return out, fmt.Errorf("navigate: %w", err)
			}
		}
	}

	// 2. Wait up to 30s for the SPA to finish loading & hydrating
	log.Printf("[chatgpt] waiting for page ready...")
	if err := c.waitPageReady(ctx, tab, 30*time.Second); err != nil {
		return out, err
	}
	if projectPattern.MatchString(page) {
		var projectComposer bool
		if err := chromedp.Run(tab, chromedp.Evaluate(`(() => {
		  const els = [...document.querySelectorAll('[contenteditable="true"]')];
		  return els.some(e => (e.getAttribute('aria-label') || '').startsWith('New chat in '));
		})()`, &projectComposer)); err != nil {
			return out, err
		}
		if !projectComposer {
			return out, errors.New("Project composer not verified; REPAIR: check saved openai-project-url and Project DOM; no prompt submitted")
		}
	}

	// 2b. Optionally switch to the Thinking model so reasoning is emitted.
	if opt.Thinking {
		if err := c.selectThinking(ctx, tab); err != nil {
			return out, err
		}
	}

	// 3. Type the prompt.
	log.Printf("[chatgpt] typing prompt (%d bytes)...", len(opt.Prompt))
	if err := c.typePrompt(ctx, tab, opt.Prompt); err != nil {
		return out, err
	}

	// 3b. Upload any image/video attachments into the composer file input.
	if len(opt.Attachments) > 0 {
		log.Printf("[chatgpt] attaching %d file(s)...", len(opt.Attachments))
		files, err := c.attachFiles(ctx, tab, opt.Attachments)
		if err != nil {
			return out, err
		}
		defer func() {
			for _, f := range files {
				_ = os.Remove(f)
			}
		}()
	}

	// 4. Submit.
	var cprobe struct {
		Found   bool `json:"found"`
		Exact   bool `json:"exact"`
		CompLen int  `json:"compLen"`
		WantLen int  `json:"wantLen"`
		Off     int  `json:"off"`
	}
	if err := chromedp.Run(tab, chromedp.Evaluate(fmt.Sprintf(`(() => {
	  const e = document.querySelector('#prompt-textarea') || document.querySelector('form[data-chatgpt-composer] div[contenteditable="true"]') || document.querySelector('.ProseMirror[contenteditable="true"]');
	  const norm = s => (s || '').replace(/\r\n?/g, '\n').trim();
	  // Only the observed URL defusing space is equivalent, in either direction.
	  const canonical = s => norm(s).replace(/([<(\"]) ?(https?:\/\/)/g, '$1$2');
	  const want = canonical(%s);
	  if (!e) return {found:false, exact:false, compLen:0, wantLen:want.length, off:0};
	  // innerText includes visual line breaks around rendered link widgets,
	  // not editor content. Read the current ProseMirror document instead.
	  const node = e.pmViewDesc?.node;
	  const text = node?.type?.name === 'doc' && typeof node.textBetween === 'function'
	    ? node.textBetween(0, node.content.size, '\n', '\n')
	    : e.tagName === 'TEXTAREA' ? e.value : null;
	  if (text === null) return {found:true, exact:false, compLen:0, wantLen:want.length, off:0};
	  const comp = canonical(text);
	  if (comp === want) return {found:true, exact:true, compLen:comp.length, wantLen:want.length, off:0};
	  let off = 0;
	  const n = Math.min(comp.length, want.length);
	  while (off < n && comp[off] === want[off]) off++;
	  return {found:true, exact:false, compLen:comp.length, wantLen:want.length, off};
	})()`, strconv.Quote(opt.Prompt)), &cprobe)); err != nil {
		return out, err
	}
	if !cprobe.Exact {
		// Evidence for the systematic chunk-2-style mismatch: log WHERE the
		// composer diverges so the transform can be identified, not guessed.
		// Still fails closed — nothing submits on mismatch. Best-effort wipe
		// first: a partial insert must not persist as a draft that poisons
		// retries or another session's pooled tab.
		log.Printf("[chatgpt] composer mismatch found=%v compLen=%d wantLen=%d diffoff=%d", cprobe.Found, cprobe.CompLen, cprobe.WantLen, cprobe.Off)
		wipeCtx, wipeCancel := context.WithTimeout(tab, 5*time.Second)
		_ = chromedp.Run(wipeCtx, chromedp.Evaluate(`(() => {
		  const e = document.querySelector('#prompt-textarea') || document.querySelector('form[data-chatgpt-composer] div[contenteditable="true"]') || document.querySelector('.ProseMirror[contenteditable="true"]');
		  if (!e) return;
		  try {
		    if (e.tagName === 'TEXTAREA') { e.value = ''; }
		    else { e.focus(); document.execCommand('selectAll', false, null); document.execCommand('delete', false, null); }
		    e.dispatchEvent(new Event('input', { bubbles: true }));
		  } catch (_) {}
		})()`, nil))
		wipeCancel()
		return out, errors.New("composer does not exactly match this turn; nothing submitted")
	}
	before, err := responseProbe(tab, nil)
	if err != nil {
		return out, err
	}
	log.Printf("[chatgpt] target=%s submitting prompt...", chromedp.FromContext(tab).Target.TargetID)
	submissionAttempted = true // Even a failed click can have an unknown outcome.
	if err := c.submit(ctx, tab); err != nil {
		return out, err
	}

	// 5. Wait for the answer.
	log.Printf("[chatgpt] waiting for answer (timeout: %v)...", timeout)
	answer, reasoning, err := c.waitForAnswer(ctx, tab, timeout, pollInterval, onProgress, before.IDs)
	out.Answer, out.Reasoning = answer, reasoning
	var url string
	// Capture an uncertain submitted turn's URL even after request cancellation.
	// This bounded, read-only diagnostic never sends or retries a browser action.
	probe, done := context.WithTimeout(c.tab, 2*time.Second)
	defer done()
	if locationErr := chromedp.Run(probe, chromedp.Location(&url)); err == nil {
		err = locationErr
	}
	out.URL = url
	return out, err
}

// requestContext keeps the CDP executor but ties actions to request cancellation.
func requestContext(ctx, tab context.Context) (context.Context, context.CancelFunc) {
	child, cancel := context.WithCancel(tab)
	stop := context.AfterFunc(ctx, cancel)
	return child, func() { stop(); cancel() }
}

// AskThread restores an explicit stored thread in this client's dedicated tab.
// Empty URL starts a new thread. The caller serializes the whole transaction.
func (c *Client) AskThread(ctx context.Context, prompt, threadURL string) (string, string, error) {
	res, err := c.AskThreadTurn(ctx, TurnOptions{Prompt: prompt, URL: threadURL})
	return res.Answer, res.URL, err
}

// AskThreadTurn is AskThread plus image/video attachments and Thinking reasoning.
func (c *Client) AskThreadTurn(ctx context.Context, opt TurnOptions) (out TurnResult, err error) {
	enteredTurn := false
	defer func() {
		if err != nil && !enteredTurn {
			err = errors.Join(ErrNotSubmitted, err)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return TurnResult{}, err
	}
	if opt.URL != "" && !threadPattern.MatchString(opt.URL) {
		return TurnResult{}, errors.New("invalid ChatGPT thread URL")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	tab, err := c.tabCtx(ctx)
	if err != nil {
		return TurnResult{}, err
	}
	page := opt.URL
	projectURL := ""
	if page == "" {
		unlock := func() {}
		if c.SetupLocks {
			var err error
			unlock, err = LockSetup(ctx, c.ProfileDir)
			if err != nil {
				return TurnResult{}, err
			}
			defer unlock()
		}
		switch {
		case opt.ProjectURL != "":
			if !projectPattern.MatchString(opt.ProjectURL) {
				return TurnResult{}, errors.New("invalid ChatGPT project URL")
			}
			page, projectURL = opt.ProjectURL, opt.ProjectURL
		case opt.ProjectName != "":
			p, err := c.ensureProjectNamed(ctx, tab, opt.ProjectName)
			if err != nil {
				return TurnResult{}, errors.Join(ErrProjectSetup, err)
			}
			page, projectURL = p, p
		default:
			page, err = c.ensureProject(ctx, tab)
			if err != nil {
				return TurnResult{}, err
			}
		}
		unlock() // shared project setup ends before any prompt is submitted
	}
	enteredTurn = true
	res, err := c.askLocked(ctx, opt, page, DefaultTimeout, 3*time.Second, nil)
	if err != nil {
		return TurnResult{URL: res.URL}, err
	}
	res.ProjectURL = projectURL
	if !threadPattern.MatchString(res.URL) {
		return TurnResult{}, errors.New("answer completed but no persistent thread URL; turn cannot be safely continued")
	}
	return res, nil
}

var threadPattern = regexp.MustCompile(`^https://chatgpt\.com/(?:g/g-p-[a-zA-Z0-9-]+/)?c/[a-zA-Z0-9-]+$`)

func (c *Client) waitPageReady(ctx context.Context, tab context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := tab.Err(); err != nil {
			return err
		}
		checkCtx, cancel := context.WithTimeout(tab, 3*time.Second)
		var ready bool
		js := `(() => {
		  if (document.readyState !== 'complete') return false;
		  const el = document.querySelector('#prompt-textarea') || document.querySelector('textarea#prompt-textarea') || document.querySelector('form[data-chatgpt-composer] div[contenteditable="true"]') || document.querySelector('.ProseMirror[contenteditable="true"]');
		  if (!el) return false;
		  const rect = el.getBoundingClientRect();
		  return rect.width > 0 && rect.height > 0;
		})()`
		_ = chromedp.Run(checkCtx, chromedp.Evaluate(js, &ready))
		cancel()
		if ready {
			// Grace period for React hydration / event listener attachment
			return pause(tab, 2*time.Second)
		}
		if err := pause(tab, time.Second); err != nil {
			return err
		}
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
		if err := tab.Err(); err != nil {
			return err
		}
		checkCtx, cancel := context.WithTimeout(tab, 5*time.Second)
		var ready bool
		err := chromedp.Run(checkCtx, chromedp.Evaluate(
			`!!(document.querySelector('#prompt-textarea') || document.querySelector('textarea#prompt-textarea') || document.querySelector('form[data-chatgpt-composer] div[contenteditable="true"]') || document.querySelector('.ProseMirror[contenteditable="true"]'))`, &ready))
		cancel()
		if err == nil && ready {
			break
		}
		if time.Now().After(deadline) {
			return ErrNoComposer
		}
		if err := pause(tab, 500*time.Millisecond); err != nil {
			return err
		}
	}

	// Insert as native text first. ChatGPT's Project composer converts a large
	// paste event into a file attachment, leaving the composer empty so nothing
	// submits; execCommand('insertText') keeps the payload as literal composer
	// text. The paste path stays as a fallback for renderers that ignore it.
	typeCtx, cancel := context.WithTimeout(tab, 30*time.Second)
	defer cancel()
	insert := fmt.Sprintf(`(() => {
	  const t = %s;
	  const el = document.querySelector('#prompt-textarea') || document.querySelector('textarea#prompt-textarea') || document.querySelector('form[data-chatgpt-composer] div[contenteditable="true"]') || document.querySelector('.ProseMirror[contenteditable="true"]');
	  if (!el) return 'missing';
	  el.focus();
	  // Wipe deterministically FIRST. A leftover draft — from a failed turn
	  // on this thread, or another session's pooled tab — must never prepend
	  // or append: the submit check would fail closed every retry, and worse,
	  // a partial wipe could submit foreign text. Bail out if the wipe fails.
	  try {
	    if (el.tagName === 'TEXTAREA') { el.value = ''; }
	    else { document.execCommand('selectAll', false, null); document.execCommand('delete', false, null); }
	    el.dispatchEvent(new Event('input', { bubbles: true }));
	    el.focus();
	  } catch (e) {}
	  if ((el.innerText || el.value || '').trim().length > 0) return 'dirty';
	  try { document.execCommand('insertText', false, t); } catch (e) {}
	  if ((el.innerText || el.value || '').trim().length > 0) return 'ok';
	  try {
	    const dt = new DataTransfer();
	    dt.setData('text/plain', t);
	    el.dispatchEvent(new ClipboardEvent('paste', { bubbles: true, cancelable: true, clipboardData: dt }));
	    if ((el.innerText || el.value || '').trim().length > 0) return 'ok';
	  } catch (e) {}
	  return 'empty';
	})()`, strconv.Quote(prompt))
	var inserted string
	if err := chromedp.Run(typeCtx, chromedp.Evaluate(insert, &inserted)); err != nil {
		return fmt.Errorf("type prompt: %w", err)
	}
	switch inserted {
	case "ok":
		break
	case "dirty":
		return errors.New("type prompt: composer draft stuck, refusing to type over foreign text")
	default:
		return ErrNoComposer
	}
	// Confirm the composer actually holds the payload. If the renderer converted
	// it into an attachment, click "Show in text field" until it expands inline.
	minLen := len(prompt)
	if minLen > 200 {
		minLen = 200
	}
	deadline = time.Now().Add(15 * time.Second)
	for {
		var state struct {
			Len      int  `json:"len"`
			Expand   bool `json:"expand"`
			Attached bool `json:"attached"`
		}
		probe, probeCancel := context.WithTimeout(tab, 3*time.Second)
		err := chromedp.Run(probe, chromedp.Evaluate(`(() => {
		  const el = document.querySelector('#prompt-textarea') || document.querySelector('textarea#prompt-textarea') || document.querySelector('form[data-chatgpt-composer] div[contenteditable="true"]') || document.querySelector('.ProseMirror[contenteditable="true"]');
		  const btn = [...document.querySelectorAll('button')].find(e => (e.innerText || '').trim() === 'Show in text field');
		  return {
		    len: el ? (el.innerText || el.value || '').length : 0,
		    expand: !!btn,
		    attached: !!document.querySelector('button[aria-label="Remove pasted text attachment"]')
		  };
		})()`, &state))
		probeCancel()
		if err != nil {
			return err
		}
		if state.Len >= minLen {
			return nil
		}
		if state.Expand {
			clickCtx, clickCancel := context.WithTimeout(tab, 3*time.Second)
			_ = chromedp.Run(clickCtx, chromedp.Evaluate(`(() => {
			  const btn = [...document.querySelectorAll('button')].find(e => (e.innerText || '').trim() === 'Show in text field');
			  if (btn) btn.click();
			})()`, nil))
			clickCancel()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("composer holds %d/%d bytes (attached=%v); REPAIR: inspect the Project paste/attachment UI in internal/chatgpt/chatgpt.go", state.Len, minLen, state.Attached)
		}
		if err := pause(tab, 300*time.Millisecond); err != nil {
			return err
		}
	}
}

func (c *Client) submit(ctx context.Context, tab context.Context) error {
	sendJS := `(() => {
	  const btn = document.querySelector('[data-testid="send-button"]')
	    || document.querySelector('form[data-chatgpt-composer] button[aria-label^="Send"]')
	    || document.querySelector('button[aria-label="Send message"]')
	    || document.querySelector('button[aria-label="Send prompt"]')
	    || document.querySelector('button[aria-label="Send"]');
	  if (btn && !btn.disabled) {
	    btn.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true, cancelable: true }));
	    btn.dispatchEvent(new MouseEvent("mousedown", { bubbles: true, cancelable: true }));
	    btn.dispatchEvent(new MouseEvent("mouseup", { bubbles: true, cancelable: true }));
	    btn.click();
	    return true;
	  }
	  return false;
	})()`
	// Submit and VERIFY: an attachment upload can make an early Send click a
	// no-op, leaving the prompt in the composer. Retry click then Enter until
	// the composer is empty or a stop button appears.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if err := tab.Err(); err != nil {
			return err
		}
		clickCtx, cancel := context.WithTimeout(tab, 3*time.Second)
		_ = chromedp.Run(clickCtx, chromedp.Evaluate(sendJS, nil))
		cancel()
		if err := pause(tab, 700*time.Millisecond); err != nil {
			return err
		}
		if sent, err := c.turnSubmitted(tab); err == nil && sent {
			return nil
		}
		keyCtx, cancel2 := context.WithTimeout(tab, 5*time.Second)
		_ = chromedp.Run(keyCtx, chromedp.Focus(`.ProseMirror[contenteditable="true"]`, chromedp.ByQuery), chromedp.KeyEvent("\r"))
		cancel2()
		if err := pause(tab, 700*time.Millisecond); err != nil {
			return err
		}
		if sent, err := c.turnSubmitted(tab); err == nil && sent {
			return nil
		}
	}
	return errors.New("send did not submit the turn; REPAIR: inspect composer/send DOM (attachment upload may be stuck)")
}

// turnSubmitted reports that the composer was cleared or generation started.
func (c *Client) turnSubmitted(tab context.Context) (bool, error) {
	probe, cancel := context.WithTimeout(tab, 3*time.Second)
	defer cancel()
	var sent bool
	err := chromedp.Run(probe, chromedp.Evaluate(`(() => {
	  const el = document.querySelector('#prompt-textarea') || document.querySelector('form[data-chatgpt-composer] div[contenteditable="true"]') || document.querySelector('.ProseMirror[contenteditable="true"]');
	  const empty = !el || (el.innerText || el.value || '').trim().length === 0;
	  const stop = !!document.querySelector('[data-testid="stop-button"]') || [...document.querySelectorAll('button')].some(b => /stop/i.test(b.getAttribute('aria-label') || ''));
	  return empty || stop;
	})()`, &sent))
	return sent, err
}

// waitForAnswer polls the page until the assistant reply is stable (or the
// stop button disappears), handling the "Continue generating" button.
// onProgress (may be nil) receives the full answer-so-far whenever it grows.
func (c *Client) waitForAnswer(ctx context.Context, tab context.Context, timeout, pollInterval time.Duration, onProgress func(string), before []string) (string, string, error) {
	deadline := time.Now().Add(timeout)
	lastText := ""
	stablePolls := 0
	continues := 0

	empty := emptyAnswerWatch{Started: time.Now()}

	for time.Now().Before(deadline) {
		st, err := responseProbe(tab, before)
		if err != nil {
			return "", "", fmt.Errorf("poll: %w", err)
		}
		log.Printf("[chatgpt] target=%s poll: stop=%v, cont=%v, err=%v, len=%d reasoning=%d", chromedp.FromContext(tab).Target.TargetID, st.StopBtn, st.ContinueBtn, st.ErrorText, len(st.Last), len(st.Reasoning))

		if st.ErrorText {
			return "", "", errors.New("ChatGPT reported a page error; inspect the stored thread before recovery; no automatic resubmission")
		}
		if empty.expired(st, time.Now()) {
			return "", "", ErrMissingAnswer
		}
		if st.StopBtn {
			stablePolls = 0
			if onProgress != nil && st.Last != "" && st.Last != lastText {
				onProgress(st.Last)
				lastText = st.Last
			}
			if err := pause(tab, pollInterval); err != nil {
				return "", "", err
			}
			continue
		}
		if st.ContinueBtn && st.Last != "" && st.Last == lastText {
			if continues >= 3 {
				return "", "", errors.New("ChatGPT requires more than three Continue generating clicks")
			}
			continues++
			clickCtx, cancel := context.WithTimeout(tab, 5*time.Second)
			_ = chromedp.Run(clickCtx, chromedp.Evaluate(
				`(() => { const b = document.querySelector('[data-testid="continue-generate-button"]')
				  || [...document.querySelectorAll('button')].find(x => /continue generating/i.test((x.textContent||'').trim()));
				  if (b) { b.click(); return true; } return false; })()`, nil))
			cancel()
			if err := pause(tab, 2*time.Second); err != nil {
				return "", "", err
			}
			continue
		}
		if st.Last == lastText && st.Last != "" {
			stablePolls++
			if stablePolls >= 2 {
				return strings.TrimSpace(st.Last), strings.TrimSpace(st.Reasoning), nil
			}
		} else {
			if onProgress != nil && st.Last != "" {
				onProgress(st.Last)
			}
			lastText = st.Last
			stablePolls = 0
		}
		if err := pause(tab, pollInterval); err != nil {
			return "", "", err
		}
	}
	return "", "", ErrNoResponse
}

func pause(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
