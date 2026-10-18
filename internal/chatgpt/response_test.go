package chatgpt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestResponseDOMIdentityAndScope(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("Chrome unavailable for DOM regression fixtures")
	}
	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(chrome), chromedp.UserDataDir(t.TempDir()))
	alloc, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()
	tab, cancelTab := chromedp.NewContext(alloc)
	defer cancelTab()
	tab, timeout := context.WithTimeout(tab, 45*time.Second)
	defer timeout()
	set := func(html string) {
		t.Helper()
		if err := chromedp.Run(tab, chromedp.ActionFunc(func(ctx context.Context) error { return nil })); err != nil {
			t.Fatal(err)
		}
		if err := chromedp.Run(tab, chromedp.Evaluate(`document.body.innerHTML = `+quoteJS(html), nil)); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("composer settles complete logical document", func(t *testing.T) {
		c := &Client{}
		for _, size := range []int{300, 19000, 190000, 120} {
			set(`<div class="ProseMirror" contenteditable="true" role="textbox"></div>`)
			// Reproduce the observed race: DOM text is already long enough for
			// the old 200-character check while the editor model is still empty.
			// Real editors batch DOM reconciliation; only their logical document
			// is authoritative for pre-submit verification.
			js := `(() => {const e=document.querySelector('.ProseMirror'); let model=''; let timer;
			 e.pmViewDesc={node:{type:{name:'doc'},content:{size:0},textBetween(){return model}}};
			 e.addEventListener('input',()=>{clearTimeout(timer);const next=e.innerText;timer=setTimeout(()=>{model=next},1600)});
			})()`
			if err := chromedp.Run(tab, chromedp.Evaluate(js, nil)); err != nil {
				t.Fatal(err)
			}
			prompt := strings.Repeat("x", size)
			if err := c.typePrompt(tab, tab, prompt); err != nil {
				t.Fatalf("size %d: %v", size, err)
			}
			var exact bool
			if err := chromedp.Run(tab, chromedp.Evaluate(`document.querySelector('.ProseMirror').pmViewDesc.node.textBetween() === `+quoteJS(prompt), &exact)); err != nil || !exact {
				t.Fatalf("returned before full model settlement: size=%d exact=%v err=%v", size, exact, err)
			}
		}
		// A long visual prefix is never enough; persistent partial input must
		// fail before submission. Keep this test bounded by caller cancellation.
		set(`<div class="ProseMirror" contenteditable="true"></div>`)
		if err := chromedp.Run(tab, chromedp.Evaluate(`document.querySelector('.ProseMirror').pmViewDesc={node:{type:{name:'doc'},content:{size:250},textBetween(){return 'x'.repeat(250)}}}`, nil)); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(tab, 1800*time.Millisecond)
		defer cancel()
		if err := c.typePrompt(ctx, tab, strings.Repeat("x", 500)); !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			t.Fatalf("persistent partial model accepted or cancellation lost: %v", err)
		}
		ctx, cancel = context.WithCancel(tab)
		cancel()
		if err := c.typePrompt(ctx, tab, "must not type"); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled insertion: %v", err)
		}
	})
	t.Run("post-click DOM is never proof of non-submission", func(t *testing.T) {
		p := sendProof{Readable: true, ComposerExact: true}
		if !p.provenUnsubmitted() {
			t.Fatal("no-action exact readable draft should be retryable")
		}
		for _, p := range []sendProof{
			{ActionAttempted: true, Readable: true, ComposerExact: true},
			{ComposerExact: true}, {Readable: true},
			{Readable: true, ComposerExact: true, UserBubble: true},
			{Readable: true, ComposerExact: true, Generating: true},
		} {
			if p.provenUnsubmitted() {
				t.Fatalf("ambiguous send permitted retry: %+v", p)
			}
		}
		set(`<div class="ProseMirror" contenteditable="true">draft</div><button data-testid="send-button">Send</button>`)
		if err := chromedp.Run(tab, chromedp.Evaluate(`window.sendClicks=0;document.querySelector('button').onclick=()=>window.sendClicks++`, nil)); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(tab, 2200*time.Millisecond)
		defer cancel()
		if err := (&Client{}).submit(ctx, tab, "draft"); err == nil || errors.Is(err, ErrNotSubmitted) {
			t.Fatalf("in-flight/cancelled Send lost uncertainty: %v", err)
		}
		var clicks int
		if err := chromedp.Run(tab, chromedp.Evaluate(`window.sendClicks`, &clicks)); err != nil || clicks != 1 {
			t.Fatalf("possibly submitted turn repeated: clicks=%d err=%v", clicks, err)
		}
	})
	t.Run("chrome icon svgs are not answer visuals", func(t *testing.T) {
		// A real ChatGPT message with no ".markdown" container exposes the whole
		// block, including 16-24px UI icon SVGs. Those are page chrome, not answer
		// visuals: they must not become "[SVG: no accessible description...]"
		// placeholders nor near-blank 40x40 PNGs (a 20px icon at 2x).
		fixture := `<div data-message-author-role="user">ask</div>` +
			`<div data-talvt-turn-state="complete"><div data-chatgpt-search-message-ids="msg-1">` +
			`<h4 data-conversation-role="assistant">ChatGPT said:</h4><div>` +
			`<button><svg width="20" height="20" aria-hidden="true"><path d="M0 0h20v20H0z"/></svg></button>` +
			`<svg width="20" height="20"><path d="M0 0h20v20H0z"/></svg>` +
			`<div data-d-component="row"><svg width="1em" height="1em" data-d-component="icon"><path d="M0 0h10v10H0z"/></svg><span>Alpha</span></div>` +
			`<svg width="600" height="400" viewBox="0 0 600 400"><title>Diagram</title><rect width="600" height="400" fill="#eee"/></svg>` +
			`</div></div></div>`
		set(fixture)
		st, err := responseProbe(tab, nil)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(st.Last, "no accessible description") {
			t.Fatalf("chrome icon serialized as an answer visual: %q", st.Last)
		}
		if strings.Count(st.Last, "[SVG: Diagram]") != 1 {
			t.Fatalf("diagram fallback missing or duplicated: %q", st.Last)
		}
		// Rasterization must select only the substantive diagram. The previous
		// unfiltered scan also returned the two icon-sized SVGs as blank PNGs.
		var raw []struct {
			Alt     string `json:"alt"`
			DataURL string `json:"dataURL"`
		}
		js := "(" + rasterSVGJS + ")('msg-1')"
		if err := chromedp.Run(tab, chromedp.Evaluate(js, &raw, chromedp.EvalAsValue, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil {
			t.Fatal(err)
		}
		if len(raw) != 1 || raw[0].Alt != "Diagram" {
			t.Fatalf("rasterized chrome/icon svgs instead of the diagram: %+v", raw)
		}
		if !ValidRasterImage(raw[0].DataURL) {
			t.Fatalf("diagram did not rasterize to a valid PNG: %d bytes", len(raw[0].DataURL))
		}
	})
	old := `<div data-chatgpt-search-message-ids="old"><h4>ChatGPT said:</h4><div>OLD TOOL CALL</div></div>`
	set(old + `<div data-chatgpt-search-message-ids="analysis"><h4>ChatGPT said:</h4><div>Thought for 4 seconds</div></div>`)
	before, err := responseProbe(tab, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Two old nodes collapse into one; the total heading count does not grow.
	// A stale role node and unrelated markdown/footer must not override NEW.
	set(old + `<div data-message-author-role="assistant" data-message-id="old-role">STALE</div><h4>You said:</h4><div>tool output</div><div data-talvt-turn-state="complete"><div data-chatgpt-search-message-ids="new new"><h4 data-conversation-role="assistant">ChatGPT said:</h4><div><p>NEW</p><button>Copy</button></div></div></div><div class="markdown">UNRELATED</div><h4>Latest response</h4><div>FOOTER</div>`)
	st, err := responseProbe(tab, before.IDs)
	if err != nil {
		t.Fatal(err)
	}
	if st.Last != "NEW" || !st.Complete {
		t.Fatalf("wrong fresh scoped response: %+v", st)
	}
	set(old + `<h4>You said:</h4><div><div class="markdown">PRIVATE USER TEXT</div></div><div role="alert">Something went wrong</div>`)
	st, err = responseProbe(tab, before.IDs)
	if err != nil {
		t.Fatal(err)
	}
	if st.Last != "" || !st.ErrorText {
		t.Fatalf("old/user text leaked or error missed: %+v", st)
	}
	set(`<div data-message-author-role="user">request</div><div data-message-author-role="assistant" data-message-id="legacy"><div class="markdown"><div data-markdown-copy="code-block"><pre><code class="language-json">{"x":1}</code></pre></div></div></div>`)
	st, err = responseProbe(tab, before.IDs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(st.Last, `{"x":1}`) != 1 || !strings.Contains(st.Last, "```json") {
		t.Fatalf("nested code block corrupt: %q", st.Last)
	}
	set(`<div data-message-author-role="assistant" data-message-id="old-thought"><div data-thinking>PRIVATE OLD REASONING</div></div><h4>You said:</h4><div>request</div><div data-chatgpt-search-message-ids="thought-new"><h4>ChatGPT said:</h4><div><div data-testid="thought-block">Thought for 3 seconds
Scoped reasoning</div><p>Only answer</p></div></div>`)
	st, err = responseProbe(tab, before.IDs)
	if err != nil || st.Last != "Only answer" || st.Reasoning != "Scoped reasoning" || st.LastID != "thought-new" {
		t.Fatalf("reasoning/body scope regression: %+v %v", st, err)
	}
	probe := func(body string) string {
		t.Helper()
		set(old + `<h4>You said:</h4><div>PRIVATE USER</div><div data-message-id="rich"><h4>ChatGPT said:</h4><div><div class="markdown">` + body + `</div><button>Regenerate</button></div></div><div class="markdown">FOOTER</div>`)
		got, err := responseProbe(tab, before.IDs)
		if err != nil {
			t.Fatal(err)
		}
		return got.Last
	}
	t.Run("rich Markdown", func(t *testing.T) {
		math := probe(`<p>Potentially <span class="katex"><span class="katex-mathml"><math><semantics><mrow><mi>O</mi><mo>(</mo><msup><mi>n</mi><mn>2</mn></msup><mo>)</mo></mrow><annotation encoding="application/x-tex">O(n^2)</annotation></semantics></math></span><span class="katex-html" aria-hidden="true">O(n^2)</span></span>. Linear <span class="katex"><span class="katex-mathml"><math><semantics><mrow>O(n)</mrow><annotation encoding="application/x-tex">O(n)</annotation></semantics></math></span><span class="katex-html" aria-hidden="true">O(n)</span></span>.</p><span class="katex-display"><span class="katex"><span class="katex-mathml"><math><semantics><annotation encoding="application/x-tex">x^2 + y^2</annotation></semantics></math></span><span class="katex-html">DUPLICATE GLYPHS</span></span></span><p>Repeat O(n) O(n) intentionally.</p>`)
		if math != "Potentially O(n^2). Linear O(n).\n\nx^2 + y^2\n\nRepeat O(n) O(n) intentionally." {
			t.Fatalf("math duplicated or source/prose lost: %q", math)
		}
		// Custom HTML elements are implicitly unwrapped. These are NOT tests of
		// escaped literal text, and neither fixture proves a reasoning channel.
		if got := probe(`<p>Answer text.</p><escape></escape>`); got != "Answer text." {
			t.Fatalf("empty escape pair leaked: %q", got)
		}
		if got := probe(`<escape>` + "\n\n" + `</escape><p>Answer text.</p><escape>` + "\n\n" + `</escape>`); got != "Answer text." {
			t.Fatalf("escape markers leaked: %q", got)
		}
		// Real escaped literal TEXT, combined and split across text/span nodes.
		for _, body := range []string{
			`<p>&lt;escape&gt;&lt;/escape&gt;</p><p>Answer text.</p>`,
			`<p>&lt;escape&gt;<span>&lt;/escape&gt;</span></p><p>Answer text.</p>`,
			`<p><span>&lt;escape&gt;</span> <span>&lt;/escape&gt;</span></p><p>Answer text.</p>`,
		} {
			if got := probe(body); got != "Answer text." {
				t.Fatalf("literal empty artifact leaked: %q", got)
			}
		}
		if got := probe(`&lt;escape&gt;&lt;/escape&gt;`); got != "" {
			t.Fatalf("artifact-only answer fabricated: %q", got)
		}
		if got := probe(`<p>Example &lt;escape&gt;&lt;/escape&gt; stays literal.</p>`); got != "Example <escape></escape> stays literal." {
			t.Fatalf("prose altered: %q", got)
		}
		if got := probe(`<p><code>&lt;escape&gt;&lt;/escape&gt;</code></p><pre><code>&lt;escape&gt;&lt;/escape&gt;</code></pre>`); !strings.Contains(got, "`<escape></escape>`") || !strings.Contains(got, "```\n<escape></escape>\n```") {
			t.Fatalf("code altered: %q", got)
		}
		wire := `<opencode_bridge_call>{"name":"x","arguments":{"literal":"<escape></escape>"}}</opencode_bridge_call>`
		if got := probe(strings.ReplaceAll(strings.ReplaceAll(wire, "&", "&amp;"), "<", "&lt;")); got != wire {
			t.Fatalf("tool string altered: %q", got)
		}
		if got := probe(`<p>Use &lt;escape&gt; tags in config.</p>`); got != "Use <escape> tags in config." {
			t.Fatalf("escape text in prose altered: %q", got)
		}
		for _, tc := range []struct{ tex, want string }{
			{`R(i+t)<R(j+t)\qquad 0\le t\le k`, "R(i+t)<R(j+t)  0≤ t≤ k"},
			{`j,\ j+1,\ \ldots,\ j+k`, "j, j+1, …, j+k"},
			{`x_{i+1}^{2} \geq \alpha`, `x\_{i+1}^{2} ≥ α`},
			{`\frac{a}{b} + \unknown{x}`, "` \\frac{a}{b} + \\unknown{x} `"},
		} {
			body := `<span class="katex-display"><span class="katex"><math><semantics><annotation encoding="application/x-tex">` + strings.ReplaceAll(tc.tex, "<", "&lt;") + `</annotation></semantics></math><span class="katex-html">DUPLICATE</span></span></span>`
			if got := probe(body); got != tc.want {
				t.Fatalf("terminal math %q: got %q want %q", tc.tex, got, tc.want)
			}
		}
		got := probe(`<h1>Title</h1><h4>Detail</h4><p>A <strong>bold</strong>, <em>soft</em>, <del>gone</del> <code>a` + "`" + `b</code> and <a href="https://example.com/a(b)?q=x&amp;y=2" title="a &quot;quote&quot;">[link]</a>.<br>Next</p><ul><li>First<ul><li>Nested</li></ul></li><li>Second</li></ul><ol start="3"><li>Third</li><li>Fourth</li></ol><blockquote><p>Quote</p><p>More</p></blockquote><table><thead><tr><th>Name</th><th>Value</th></tr></thead><tbody><tr><td>A|B</td><td><code>x|y</code><br>next</td></tr></tbody></table><hr><div role="toolbar">PRIVATE TOOLBAR</div><div data-thinking>PRIVATE THOUGHT</div><span hidden>PRIVATE HIDDEN</span>`)
		for _, want := range []string{"# Title", "#### Detail", "**bold**", "*soft*", "~~gone~~", "``a`b``", `[\[link\]](<https://example.com/a(b)?q=x&y=2> "a \"quote\"")`, "  \nNext", "- First\n  \n  - Nested", "3. Third\n4. Fourth", "> Quote\n> \n> More", "| Name | Value |\n| --- | --- |\n| A\\|B | `x\\|y`<br>next |", "\n\n---"} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q in:\n%s", want, got)
			}
		}
		for _, forbidden := range []string{"PRIVATE", "FOOTER", "Regenerate", "OLD TOOL CALL"} {
			if strings.Contains(got, forbidden) {
				t.Errorf("leaked %q: %s", forbidden, got)
			}
		}
		got = probe(`<img alt="Comparison diagram" src="https://example.com/diagram.png"><img src="https://chatgpt.com/backend-api/files/x?token=PRIVATE"><svg role="img"><title>Elimination</title><text>rotation 4 eliminated</text></svg><canvas></canvas><p>Literal \le in prose <code>\qquad O(n^2)</code>.</p>`)
		for _, want := range []string{
			"[Image: Comparison diagram] [source](<https://example.com/diagram.png>)",
			"[Image: no accessible description; visual content not extracted]",
			"[SVG: Elimination; rotation 4 eliminated]", "[Canvas: no accessible description; visual content not extracted]",
			"Literal \\\\le in prose `\\qquad O(n^2)`.",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("missing fallback %q: %s", want, got)
			}
		}
		if strings.Contains(got, "PRIVATE") {
			t.Fatal("authenticated visual URL leaked")
		}
		// Semantic attributes/nesting from the inspected real comparison card;
		// ordinary div paragraphs must not be flattened along with these rows.
		card := `<div data-d-component="box"><p>The same mismatch survives when both starts shift.</p>`
		for i, pair := range [][2]string{{"abac", "abad"}, {"bac", "bad"}, {"ac", "ad"}, {"c", "d"}} {
			label := fmt.Sprintf("t = %d", i)
			caption := fmt.Sprintf("Rotation %d beats rotation %d", i, i+4)
			card += `<div data-d-component="row" data-d-direction="row"><div data-d-component="box"><p>` + label + `</p></div><div data-d-component="box"><div data-d-component="row" data-d-direction="row"><p>` + pair[0] + `</p><p>&lt;</p><p>` + pair[1] + `</p></div><p>` + caption + `</p></div><svg data-d-component="icon"><path d="M5.636 5.636"></path></svg></div>`
		}
		card += `<p><strong>Four rotations eliminated after one comparison spanning four characters.</strong></p></div><div><p>Separate paragraph.</p><p>Still separate.</p></div>`
		got = probe(card)
		for i, comparison := range []string{"abac < abad", "bac < bad", "ac < ad", "c < d"} {
			want := fmt.Sprintf("- t = %d — %s Rotation %d beats rotation %d", i, comparison, i, i+4)
			if !strings.Contains(got, want) {
				t.Errorf("comparison row missing %q: %s", want, got)
			}
		}
		if !strings.Contains(got, "Separate paragraph.\n\nStill separate.") || strings.Contains(got, "[SVG:") {
			t.Fatalf("card grouping leaked icon or merged paragraphs: %s", got)
		}
	})
	t.Run("code whitespace and safe fences", func(t *testing.T) {
		raw := "{\n  \"fence\": \"```\",\n\n\n  \"path\": \"a\\\\b\"\n}\n"
		got := probe(`<div data-markdown-copy="code-block"><span data-markdown-copy="exclude">JSON</span><pre><code class="language-json">` + raw + `</code></pre></div>`)
		if got != "````json\n"+raw+"````" {
			t.Fatalf("code changed: %q", got)
		}
		got = probe(`<ul><li>Example<pre><code class="language-c++">  a\n\n\n  b</code></pre></li></ul>`)
		if !strings.Contains(got, "  ```c++\n    a\\n\\n\\n  b\n  ```") {
			t.Fatalf("nested code layout: %q", got)
		}
	})
	t.Run("literal wire protocol and plain text", func(t *testing.T) {
		raw := "<opencode_bridge_call>\n{\n  \"name\": \"echo\",\n  \"arguments\": {\"text\": \"a\\\\b_*`\"}\n}\n</opencode_bridge_call>"
		html := strings.ReplaceAll(strings.ReplaceAll(raw, "<", "&lt;"), ">", "&gt;")
		if got := probe(`<p>` + html + `</p>`); got != raw {
			t.Fatalf("wire protocol changed: %q", got)
		}
		for _, plain := range []string{"ACK", "A plain answer."} {
			if got := probe(`<p>` + plain + `</p>`); got != plain {
				t.Fatalf("plain answer changed: %q", got)
			}
		}
	})
	t.Run("exact collapsed user recovery and bound serialized source", func(t *testing.T) {
		full := strings.Repeat("Long literal prompt_* with  two spaces\tand JSON \\\"quoted\\\".\n", 2200) + " … Show more"
		set(`<div data-turn-key="fixture-user"><h4>You said:</h4><div><span class="text">truncated prefix</span><button onclick="this.parentElement.querySelector('.text').textContent=window.__full;this.textContent='Show less'">Show more</button><button aria-label="Copy message">Copy</button></div></div><div data-talvt-turn-state="complete"><h4>ChatGPT said:</h4><div>finished</div></div>`)
		if err := chromedp.Run(tab, chromedp.Evaluate(`window.__full = `+quoteJS(full), nil)); err != nil {
			t.Fatal(err)
		}
		var expanded bool
		if err := chromedp.Run(tab, chromedp.Evaluate(recoveryExpandJS, &expanded)); err != nil || !expanded {
			t.Fatal("scoped expansion failed", err)
		}
		var corr struct {
			User, UserID string
			Answers      int
		}
		if err := chromedp.Run(tab, chromedp.Evaluate(recoveryUserJS, &corr)); err != nil || corr.User != strings.TrimSpace(full) || corr.UserID != "fixture-user" || corr.Answers != 1 {
			t.Fatal("full exact binding or literal UI suffix lost", err)
		}
		set(`<div><h4>You said:</h4><div>old matching prompt</div><h4>ChatGPT said:</h4><div>old answer</div><h4>You said:</h4><div>genuinely different prompt</div><h4>ChatGPT said:</h4><div>new answer</div></div>`)
		if err := chromedp.Run(tab, chromedp.Evaluate(recoveryUserJS, &corr)); err != nil || corr.User != "genuinely different prompt" || corr.User == "old matching prompt" {
			t.Fatal("stale prompt binding", err)
		}
		// The fallback reads only the exact DOM identity, not the latest node.
		var messageID atomic.Value
		messageID.Store("fixture-user")
		var dilSource atomic.Value
		dilSource.Store(map[string]any{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/auth/session":
				_, _ = w.Write([]byte(`{"accessToken":"fixture-token"}`))
			case "/backend-api/conversation/fixture":
				_ = json.NewEncoder(w).Encode(map[string]any{"mapping": map[string]any{"fixture-user": map[string]any{"message": map[string]any{"id": messageID.Load(), "author": map[string]any{"role": "user"}, "content": map[string]any{"content_type": "text", "parts": []string{`\<wire\> \*literal\* [https://example.com](https://example.com) &#x20; \"quoted\"`}}, "metadata": map[string]any{"serialization_metadata": map[string]any{"render_format": "markdown"}}}}}})
			case "/backend-api/conversation/dil":
				_ = json.NewEncoder(w).Encode(dilSource.Load())
			default:
				_, _ = w.Write([]byte(`{}`))
			}
		}))
		defer server.Close()
		if err := chromedp.Run(tab, chromedp.Navigate(server.URL+"/c/fixture")); err != nil {
			t.Fatal(err)
		}
		text, err := originalUserText(tab, "fixture-user")
		if err != nil || strings.Join(strings.Fields(text), " ") != `<wire> *literal* https://example.com "quoted"` {
			t.Fatal("declared markdown did not decode exactly", err)
		}
		messageID.Store("different-node")
		if _, err := originalUserText(tab, "fixture-user"); err == nil {
			t.Fatal("mismatched backend identity accepted")
		}
		// Captured DIL null renderer: headings and duplicate search IDs are
		// present, but the finished wire response was consumed as invalid tags.
		if err := chromedp.Run(tab, chromedp.Navigate(server.URL+"/c/dil")); err != nil {
			t.Fatal(err)
		}
		raw := "<opencode_bridge_call>\n{\"name\":\"echo\",\"arguments\":{\"text\":\"a\\\\b_*`\"}}\n</opencode_bridge_call>"
		m := map[string]any{"id": "dil-answer", "author": map[string]any{"role": "assistant"}, "channel": "final", "recipient": "all", "status": "finished_successfully", "end_turn": true, "content": map[string]any{"content_type": "text", "parts": []string{raw}}, "metadata": map[string]any{"is_complete": true, "model_dil_v2": map[string]any{"code": "DIL.render(__dil.jsx(()=>{return null;}));", "fallbackMarkdown": "", "recoveryDiagnostics": []any{map[string]any{"code": "invalid_tag_syntax", "action": "recovered_parse"}}}}}
		node := map[string]any{"parent": "fixture-user", "message": m}
		mapping := map[string]any{"dil-answer": node, "fixture-user": map[string]any{"message": map[string]any{"id": "fixture-user", "author": map[string]any{"role": "user"}}}}
		publish := func() { dilSource.Store(map[string]any{"mapping": mapping}) }
		publish()
		dilHTML := old + `<div data-talvt-turn-state="complete"><div><h4>You said:</h4><div data-chatgpt-search-message-ids="fixture-user"><p>request</p></div></div><div data-chatgpt-search-message-ids="dil-answer dil-answer"><h4 data-conversation-role="assistant">ChatGPT said:</h4><div><div class="MarkdownRoot-rZKhxa"><div hidden></div><div class="DilRenderer-tB76Jj"><div class="DilResponseRoot-HfQrEh" data-dil-message-id="dil-answer"></div></div></div></div></div></div><h4>Latest response</h4><div>FOOTER</div>`
		set(dilHTML)
		got, err := responseProbe(tab, before.IDs)
		if err != nil || got.Last != raw || !got.Complete || got.LastID != "dil-answer" {
			t.Fatalf("bound null-DIL wire changed: %+v %v", got, err)
		}
		if err := chromedp.Run(tab, chromedp.Evaluate(recoveryUserJS, &corr)); err != nil || corr.UserID != "fixture-user" {
			t.Fatal("actual sibling user identity lost", corr, err)
		}
		got, err = responseProbe(tab, []string{"dil-answer"})
		if err != nil || got.Last != "" || got.Fresh {
			t.Fatal("baseline source answer leaked", got, err)
		}
		for field, wrong := range map[string]any{"id": "another-answer", "channel": "analysis", "recipient": "tool", "status": "in_progress", "end_turn": false} {
			valid := m[field]
			m[field] = wrong
			publish()
			got, err = responseProbe(tab, before.IDs)
			if err != nil || got.Last != "" {
				t.Fatalf("unsafe source %s accepted: %+v %v", field, got, err)
			}
			m[field] = valid
		}
		node["parent"] = "different-user"
		publish()
		got, err = responseProbe(tab, before.IDs)
		if err != nil || got.Last != "" {
			t.Fatal("unbound source adopted", got, err)
		}
		node["parent"] = "fixture-user"
		publish()
		for _, body := range []string{old + `<h4>You said:</h4><div>request</div>`, old + `<h4>You said:</h4><div>request</div><div data-talvt-turn-state="complete"><h4>ChatGPT said:</h4><div><div data-thinking>ONLY REASONING</div></div></div>`} {
			set(body)
			got, err = responseProbe(tab, before.IDs)
			if err != nil || got.Last != "" {
				t.Fatal("no assistant/reasoning became answer", got, err)
			}
		}
		set(dilHTML + `<button data-testid="stop-button">Stop</button>`)
		got, err = responseProbe(tab, before.IDs)
		if err != nil || got.Last != "" || !got.StopBtn {
			t.Fatal("generating source adopted", got, err)
		}
		set(strings.Replace(dilHTML, `<div class="DilResponseRoot-HfQrEh" data-dil-message-id="dil-answer"></div>`, `<div class="DilResponseRoot-HfQrEh" data-dil-message-id="dil-answer"><p>NEW COMPLETE ANSWER</p></div>`, 1))
		got, err = responseProbe(tab, before.IDs)
		if err != nil || got.Last != "NEW COMPLETE ANSWER" || !got.Complete {
			t.Fatal("actual rendered answer overridden", got, err)
		}
		// Real mixed-commentary DIL shape: rendered JSON has escaped Markdown
		// and collapsed whitespace; only the final source preserves arguments.
		mixed := "I will call the declared function.\n\n" + raw + "\n\nNo tools have run yet."
		m["content"] = map[string]any{"content_type": "text", "parts": []string{mixed}}
		m["metadata"] = map[string]any{"is_complete": true, "model_dil_v2": map[string]any{"code": "not executed", "fallbackMarkdown": mixed}}
		publish()
		visibleHTML := strings.Replace(dilHTML, `<div class="DilResponseRoot-HfQrEh" data-dil-message-id="dil-answer"></div>`, `<div class="DilResponseRoot-HfQrEh" data-dil-message-id="dil-answer"><p>I will call the declared function.</p><p>`+html.EscapeString(raw)+`</p><p>No tools have run yet.</p></div>`, 1)
		set(visibleHTML)
		got, err = responseProbe(tab, before.IDs)
		if err != nil || !got.SourceRequired || got.Last != mixed || got.LastID != "dil-answer" {
			t.Fatal("mixed DIL protocol source was transformed", err)
		}
		// Actual ec597 renderer: final assistant owner has a stable message ID
		// but no data-dil-message-id anywhere. Bind its full source, not the
		// Markdown-escaped visual JSON; ancestry/status validation still applies.
		plainHTML := strings.Replace(visibleHTML, ` data-dil-message-id="dil-answer"`, "", 1)
		m["metadata"] = map[string]any{"is_complete": true}
		publish()
		set(plainHTML)
		if got, err := responseProbe(tab, before.IDs); err != nil || got.Last != mixed || !got.SourceRequired {
			t.Fatal("non-DIL DOM-bound protocol source lost", err)
		}
		emptyPlain := strings.Replace(dilHTML, ` data-dil-message-id="dil-answer"`, "", 1)
		m["content"] = map[string]any{"content_type": "text", "parts": []string{"<escape>" + raw + "</escape>"}}
		publish()
		set(emptyPlain)
		if got, err := responseProbe(tab, before.IDs); err != nil || got.Last != "<escape>"+raw+"</escape>" {
			t.Fatal("completed non-DIL empty rendering lost exact protocol source", err)
		}
		// Real audit: hydration completed during the fourteen-second source
		// fetch. IDs/status/user ancestry stayed fixed but visual text changed.
		for _, change := range []struct {
			name, js string
			safe     bool
		}{
			{"hydrate same final", `document.querySelector('.DilResponseRoot-HfQrEh').textContent=` + quoteJS(raw), true},
			{"replace user identity", `document.querySelector('[data-chatgpt-search-message-ids="fixture-user"]').setAttribute('data-chatgpt-search-message-ids','different-user')`, false},
			{"replace assistant identity", `document.querySelector('[data-chatgpt-search-message-ids="dil-answer dil-answer"]').setAttribute('data-chatgpt-search-message-ids','different-answer')`, false},
		} {
			set(emptyPlain)
			js := `window.__auditFetch=window.fetch;window.fetch=async(...args)=>{const r=await window.__auditFetch(...args);if(String(args[0]).includes('/backend-api/conversation/')){` + change.js + `}return r;};`
			if err := chromedp.Run(tab, chromedp.Evaluate(js, nil)); err != nil {
				t.Fatal(err)
			}
			got, err := responseProbe(tab, before.IDs)
			if restoreErr := chromedp.Run(tab, chromedp.Evaluate(`window.fetch=window.__auditFetch;delete window.__auditFetch`, nil)); restoreErr != nil {
				t.Fatal(restoreErr)
			}
			if change.safe && (err != nil || got.Last != "<escape>"+raw+"</escape>") {
				t.Fatal("same final hydration rejected", err)
			}
			if !change.safe && got.Last != "" {
				t.Fatal("changed identity adopted during source read", change.name, err)
			}
		}
		m["content"] = map[string]any{"content_type": "text", "parts": []string{"ordinary prose"}}
		publish()
		set(emptyPlain)
		if got, err := responseProbe(tab, before.IDs); err != nil || got.Last != "" {
			t.Fatal("non-DIL empty rendering adopted unverified prose", err)
		}
		// Actual 2026-10-17 history import: completed final source contains
		// exactly ACK, but its non-DIL renderer shows an empty answer.
		m["content"] = map[string]any{"content_type": "text", "parts": []string{"ACK"}}
		publish()
		set(emptyPlain)
		if got, err := responseProbe(tab, before.IDs); err != nil || got.Last != "ACK" || !got.SourceVerified {
			t.Fatal("exact completed import acknowledgement was lost", err)
		}
		if err := chromedp.Run(tab, chromedp.Evaluate(`window.__ackFetch=window.fetch;window.__ackReads=0;window.fetch=(...a)=>{if(String(a[0]).includes('/backend-api/conversation/'))window.__ackReads++;return window.__ackFetch(...a)}`, nil)); err != nil {
			t.Fatal(err)
		}
		text, _, waitErr := (&Client{}).waitForAnswer(context.Background(), tab, time.Second, time.Millisecond, nil, before.IDs)
		var reads int
		if err := chromedp.Run(tab, chromedp.Evaluate(`window.__ackReads`, &reads), chromedp.Evaluate(`window.fetch=window.__ackFetch;delete window.__ackFetch;delete window.__ackReads`, nil)); err != nil {
			t.Fatal(err)
		}
		if waitErr != nil || text != "ACK" || reads != 1 {
			t.Fatal("completed source was redundantly fetched for stability polls", waitErr, text, reads)
		}
		// Captured race: the outer probe saw zero bytes; by the source
		// readback's recheck the SAME final ACK had hydrated. The former
		// nonempty-prose guard returned empty again, preserving stale state.
		hydratedHTML := strings.Replace(emptyPlain, `<div class="DilResponseRoot-HfQrEh"></div>`, `<div class="DilResponseRoot-HfQrEh">ACK</div>`, 1)
		set(hydratedHTML)
		var hydratedACK string
		baselineJSON, _ := json.Marshal(before.IDs)
		readACK := fmt.Sprintf(`(async()=>{const recheck=()=>%s(%s);const st=recheck();%s})()`, responseProbeJS, string(baselineJSON), nullDILWireJS)
		if err := chromedp.Run(tab, chromedp.Evaluate(readACK, &hydratedACK, chromedp.EvalAsValue, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil || hydratedACK != "ACK" {
			t.Fatal("empty-to-visible ACK readback race lost exact final", err, hydratedACK)
		}
		// The real hydration also mounts a nonempty DIL root whose fallback
		// is ACK, not the narrowly handled null-code protocol failure.
		set(strings.Replace(dilHTML, `<div class="DilResponseRoot-HfQrEh"></div>`, `<div class="DilResponseRoot-HfQrEh">ACK</div>`, 1))
		if err := chromedp.Run(tab, chromedp.Evaluate(readACK, &hydratedACK, chromedp.EvalAsValue, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil || hydratedACK != "ACK" {
			t.Fatal("hydrated DIL ACK incorrectly required null-render diagnostics", err, hydratedACK)
		}
		node["parent"] = "different-user"
		publish()
		if err := chromedp.Run(tab, chromedp.Evaluate(readACK, &hydratedACK, chromedp.EvalAsValue, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil || hydratedACK != "" {
			t.Fatal("hydrated ACK bypassed source ancestry", err)
		}
		set(emptyPlain)
		if got, _ := responseProbe(tab, before.IDs); got.Last != "" {
			t.Fatal("unrelated acknowledgement adopted")
		}
		node["parent"] = "fixture-user"
		m["content"] = map[string]any{"content_type": "text", "parts": []string{mixed}}
		publish()
		set(plainHTML)
		node["parent"] = "different-user"
		publish()
		if _, err := responseProbe(tab, before.IDs); err == nil {
			t.Fatal("non-DIL wrong user ancestry accepted")
		}
		node["parent"] = "fixture-user"
		m["status"] = "in_progress"
		publish()
		if _, err := responseProbe(tab, before.IDs); err == nil {
			t.Fatal("non-DIL unfinished source accepted")
		}
		m["status"] = "finished_successfully"
		m["metadata"] = map[string]any{"is_complete": true, "model_dil_v2": map[string]any{"code": "not executed", "fallbackMarkdown": mixed}}
		publish()
		set(visibleHTML)
		artifactSource := "<escape></escape>\n" + raw
		m["content"] = map[string]any{"content_type": "text", "parts": []string{artifactSource}}
		publish()
		if _, err := responseProbe(tab, before.IDs); err == nil {
			t.Fatal("source fallback reintroduced escape artifact")
		}
		literalSource := `<opencode_bridge_call>{"name":"literal","arguments":{"value":"<escape>"}}</opencode_bridge_call>`
		m["content"] = map[string]any{"content_type": "text", "parts": []string{literalSource}}
		publish()
		if got, err := responseProbe(tab, before.IDs); err != nil || got.Last != literalSource {
			t.Fatal("source tool string altered", err)
		}
		m["content"] = map[string]any{"content_type": "text", "parts": []string{mixed}}
		publish()
		for field, wrong := range map[string]any{"id": "another-answer", "channel": "analysis", "recipient": "tool", "status": "in_progress", "end_turn": false} {
			valid := m[field]
			m[field] = wrong
			publish()
			if _, err := responseProbe(tab, before.IDs); err == nil {
				t.Fatalf("visible unsafe source %s fell back to rendered arguments", field)
			}
			m[field] = valid
		}
		node["parent"] = "different-user"
		publish()
		if _, err := responseProbe(tab, before.IDs); err == nil {
			t.Fatal("visible unbound source accepted")
		}
		node["parent"] = "fixture-user"
		publish()
		// Actual October 14 post-tool follow-up: prior call is valid but the
		// fresh final source is genuinely empty, with only a reasoning recap.
		m["content"] = map[string]any{"content_type": "text", "parts": []string{""}}
		m["metadata"] = map[string]any{"is_complete": true, "model_dil_v2": map[string]any{"code": "return null;", "fallbackMarkdown": ""}}
		mapping["recap"] = map[string]any{"parent": "fixture-user", "message": map[string]any{"id": "recap", "author": map[string]any{"role": "assistant"}, "content": map[string]any{"content_type": "reasoning_recap"}}}
		node["parent"] = "recap"
		mapping["prior-call"] = map[string]any{"message": map[string]any{"id": "prior-call", "content": map[string]any{"content_type": "text", "parts": []string{raw}}}}
		publish()
		set(dilHTML)
		emptyProof := func() bool {
			t.Helper()
			var empty bool
			js := "(async () => {const uid = 'fixture-user', id = 'dil-answer'; const recheck = () => " + responseProbeJS + "([]); " + completedEmptyJS + "})()"
			if err := chromedp.Run(tab, chromedp.Evaluate(js, &empty, chromedp.EvalAsValue, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil {
				t.Fatal(err)
			}
			return empty
		}
		if !emptyProof() {
			t.Fatal("exact finished empty follow-up not proved")
		}
		for field, wrong := range map[string]any{"id": "prior-call", "channel": "analysis", "recipient": "tool", "status": "in_progress", "end_turn": false} {
			valid := m[field]
			m[field] = wrong
			publish()
			if emptyProof() {
				t.Fatalf("unsafe completed-empty %s accepted", field)
			}
			m[field] = valid
		}
		for field, wrong := range map[string]any{
			"author":   map[string]any{"role": "user"},
			"metadata": map[string]any{"is_complete": false},
			"content":  map[string]any{"content_type": "text", "parts": []string{raw}},
		} {
			valid := m[field]
			m[field] = wrong
			publish()
			if emptyProof() {
				t.Fatalf("wrong completed-empty %s accepted", field)
			}
			m[field] = valid
		}
		node["parent"] = "different-user"
		publish()
		if emptyProof() {
			t.Fatal("wrong empty ancestry accepted")
		}
		node["parent"] = "recap"
		publish()
		for _, html := range []string{dilHTML + `<button data-testid="stop-button">Stop</button>`, dilHTML + `<div role="alert">Something went wrong</div>`, strings.Replace(dilHTML, `fixture-user`, `different-user`, 1), old + `<h4>You said:</h4><div>new result</div>`} {
			set(html)
			if emptyProof() {
				t.Fatal("changed/generating/stale boundary accepted")
			}
		}
	})
}

func TestFinishedEmptyBoundAndGenerationGrace(t *testing.T) {
	now := time.Unix(100, 0)
	w := emptyAnswerWatch{Started: now}
	if w.expired(responseState{StopBtn: true}, now) {
		t.Fatal("active generation failed")
	}
	if w.expired(responseState{}, now.Add(time.Second)) {
		t.Fatal("no rendering grace")
	}
	if !w.expired(responseState{}, now.Add(16*time.Second)) {
		t.Fatal("finished empty hung")
	}
	w = emptyAnswerWatch{Started: now}
	if w.expired(responseState{}, now.Add(29*time.Second)) {
		t.Fatal("startup grace missing")
	}
	w.expired(responseState{}, now.Add(30*time.Second))
	if !w.expired(responseState{}, now.Add(45*time.Second)) {
		t.Fatal("missing start hangs indefinitely")
	}
	w = emptyAnswerWatch{Started: now}
	w.expired(responseState{Complete: true}, now)
	if w.expired(responseState{Last: "answer"}, now.Add(16*time.Second)) {
		t.Fatal("completed answer failed")
	}
}

func quoteJS(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestRasterImageValidation(t *testing.T) {
	png := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	if !ValidRasterImage(png) {
		t.Fatal("valid PNG data URL rejected")
	}
	gif := "data:image/gif;base64,R0lGODdhAQABAPAAAP8AAAAAACwAAAAAAQABAAACAkQBADs="
	if ValidRasterImage(gif) {
		t.Fatal("non-PNG media accepted")
	}
	if ValidRasterImage("data:image/png;base64,!!!not-base64!!!") {
		t.Fatal("undecodable base64 accepted")
	}
	if ValidRasterImage("data:image/png;base64,") {
		t.Fatal("empty payload accepted")
	}
	// JPEG bytes under a PNG prefix must not pass the signature check.
	jpeg := "data:image/png;base64,/9j/4AAQSkZJRgABAQEASABIAAD/2wBDAAgGBgcGBQgHBwcJCQgKDBQNDAsLDBkSEw8UHRofHh0aHBwgJC4nICIsIxwcKDcpLDAxNDQ0Hyc5PTgyPC4zNDL/2wBDAQkJCQwLDBgNDRgyIRwhMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMv/wAARCAABAAEDASIAAhEBAxEB/8QAFQABAQAAAAAAAAAAAAAAAAAAAAv/xAAUEAEAAAAAAAAAAAAAAAAAAAAA/8QAFQEBAQAAAAAAAAAAAAAAAAAAAAX/xAAUEQEAAAAAAAAAAAAAAAAAAAAA/9oADAMBAAIRAxEAPwCwAA8A/9k="
	if ValidRasterImage(jpeg) {
		t.Fatal("non-PNG bytes accepted")
	}
	big := "data:image/png;base64," + strings.Repeat("iVBORw0KGgo", 700000)
	if ValidRasterImage(big) {
		t.Fatal("oversized payload accepted")
	}
}

func TestRecoveredSnapshotThreadCarriesImages(t *testing.T) {
	// gateThreadAnswer is what ReadThreadTurn returns; the images captured from
	// the recovered DOM must survive into the returned TurnResult, otherwise the
	// adoption/auto-resync paths serve the "[SVG: ...]" text fallback while a
	// live turn of the same answer serves a PNG.
	img := RasterImage{Alt: "diagram", DataURL: "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg=="}
	snap := threadSnapshot{
		answerNext: true, answers: 1,
		visible: responseState{Last: "here is the diagram", Complete: true},
		images:  []RasterImage{img},
	}
	got, err := gateThreadAnswer("https://chatgpt.com/c/x", snap)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Images) != 1 || got.Images[0].DataURL != img.DataURL {
		t.Fatalf("adopted snapshot dropped images: %+v", got)
	}
}

func TestSanitizeRasterAlt(t *testing.T) {
	if got := sanitizeRasterAlt("Elimination; rotation 4\neliminated"); got != "Elimination; rotation 4 eliminated" {
		t.Fatalf("newline not folded: %q", got)
	}
	if got := sanitizeRasterAlt("a]b[c"); got != "abc" {
		t.Fatalf("brackets leaked: %q", got)
	}
	if got := sanitizeRasterAlt("   "); got != "diagram" {
		t.Fatalf("empty alt has no fallback: %q", got)
	}
	if got := sanitizeRasterAlt(strings.Repeat("x", 200)); len([]rune(got)) != 100 {
		t.Fatalf("alt not capped: %d runes", len([]rune(got)))
	}
}
