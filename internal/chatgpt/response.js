// Evaluate as a function. Stable message IDs survive reasoning collapse and
// virtualized turns; node tokens are a fallback for older renderers.
(baseline => {
  const q = s => document.querySelector(s);
  const headings = [...document.querySelectorAll('h4')];
  const users = [...document.querySelectorAll('[data-message-author-role="user"]'),
    ...headings.filter(h => (h.textContent || '').trim() === 'You said:')];
  const ordered = es => es.sort((a,b) => a.compareDocumentPosition(b) & Node.DOCUMENT_POSITION_FOLLOWING ? -1 : 1);
  const user = ordered(users).at(-1);
  const key = e => {
    const owner = e.closest('[data-chatgpt-search-message-ids], [data-message-id]');
    const id = owner?.getAttribute('data-chatgpt-search-message-ids') || owner?.getAttribute('data-message-id');
    if (id) return id.trim().split(/\s+/).at(-1);
    window.__bridgeNodes ||= new WeakMap();
    window.__bridgeNodeSeq ||= 0;
    if (!window.__bridgeNodes.has(e)) window.__bridgeNodes.set(e, 'node-' + ++window.__bridgeNodeSeq);
    return window.__bridgeNodes.get(e);
  };
  const heads = headings.filter(h => h.getAttribute('data-conversation-role') === 'assistant' || (h.textContent || '').trim() === 'ChatGPT said:');
  const roles = [...document.querySelectorAll('[data-message-author-role="assistant"]')];
  // Prefer a heading's scoped answer body. Never overwrite it with a stale
  // author-role node, a global markdown block, or the page footer.
  const candidates = ordered([...heads, ...roles.filter(e => !heads.some(h => h.parentElement.contains(e)))]);
  const fresh = candidates.filter(e => !baseline.includes(key(e)) && user && (user.compareDocumentPosition(e) & Node.DOCUMENT_POSITION_FOLLOWING));
  const last = fresh.at(-1);
  let text = '', reasoning = '';
  if (last) {
    const body = last.tagName === 'H4' ? last.nextElementSibling : last;
    const thoughts = body?.querySelector('[data-testid="thought-block"], [data-thinking]');
    if (thoughts) reasoning = (thoughts.innerText || thoughts.textContent || '').replace(/^Thought for (?:\d+\s*(?:seconds?|minutes?|hours?|[smh])\s*)+/, '').trim();
    const md = body?.querySelector('.markdown');
    const source = md || body;
    if (source) {
      const clone = source.cloneNode(true);
      // Language labels live in the code header, which the cleanup below
      // removes as toolbar UI — capture them first, keyed by wrapper. The
      // current renderer carries no language-* class on <code>; the header
      // label (TypeScript/Bash/JSON/…) or .cm-content[data-language] is the
      // only source. Validation keeps stray toolbar text out of the fence.
      const blockLang = new WeakMap();
      for (const w of clone.querySelectorAll('[data-markdown-copy="code-block"], [data-d-component="code-block"]')) {
        const cm = w.querySelector('.cm-content[data-language]');
        const cmLang = cm && cm.getAttribute('data-language');
        if (cmLang && /^[\w#+.-]{1,20}$/.test(cmLang)) { blockLang.set(w, cmLang.toLowerCase()); continue; }
        const labelEl = w.querySelector('[data-markdown-copy="exclude"] .truncate');
        const first = ((labelEl && labelEl.textContent) || '').trim().split(/\s+/)[0] || '';
        if (/^[\w#+.-]{1,20}$/.test(first)) blockLang.set(w, first.toLowerCase());
      }
      for (const e of clone.querySelectorAll('button, [role="toolbar"], [hidden], [aria-hidden="true"], script, style, [data-markdown-copy="exclude"], [data-testid="thought-block"], [data-thinking], [data-d-component="badge"]')) e.remove();
      // Serialize only this answer, never page HTML. textContent decodes escaped
      // literal protocol tags without ever interpreting them as HTML elements.
      const ticks = raw => '`'.repeat(Math.max(0, ...(raw.match(/`+/g) || []).map(s => s.length)) + 1);
      const escape = raw => raw.replace(/\\/g, '\\\\').replace(/([`*_\[\]~])/g, '\\$1');
      const children = (e, cell = false) => [...e.childNodes].map(n => render(n, cell)).join('');
      const block = s => '\n\n' + s + '\n\n';
      const codeBlocks = [];
      const render = (e, cell = false) => {
        if (e.nodeType === Node.TEXT_NODE) return escape(e.textContent.replace(/\s+/g, ' '));
        if (e.nodeType !== Node.ELEMENT_NODE) return '';
        const tag = e.tagName;
        if (tag === 'PRE' || e.matches('[data-markdown-copy="code-block"], [data-d-component="code-block"]')) {
          // Process the outer wrapper once; code text retains indentation and JSON.
          const code = e.querySelector('code');
          // CodeMirror renders one .cm-line div per line with no newline text
          // between them, so plain textContent glues lines together — rejoin.
          const cm = e.querySelectorAll('.cm-content .cm-line');
          const raw = cm.length
            ? [...cm].map(l => l.textContent).join('\n')
            : (code || e).textContent;
          const clsLang = /(?:^|\s)language-([\w.+-]+)/.exec(code?.className || '');
          const cmLang = e.querySelector('.cm-content[data-language]')?.getAttribute('data-language');
          const wrap = e.closest('[data-markdown-copy="code-block"], [data-d-component="code-block"]');
          const lang = clsLang ? clsLang[1] : (cmLang || (wrap && blockLang.get(wrap)) || '');
          const fence = '`'.repeat(Math.max(3, ticks(raw).length));
          codeBlocks.push(fence + lang + '\n' + raw + (raw.endsWith('\n') ? '' : '\n') + fence);
          return block('\u0000CODE' + (codeBlocks.length - 1) + '\u0000');
        }
        if (tag === 'CODE') {
          // Div-rendered block code without <pre> (whitespace-pre pane): fence
          // it instead of collapsing newlines into an inline span.
          if (!e.closest('pre') && e.textContent.includes('\n')) {
            const raw = e.textContent, fence = ticks(raw), fence3 = '`'.repeat(Math.max(3, fence.length));
            codeBlocks.push(fence3 + '\n' + raw + (raw.endsWith('\n') ? '' : '\n') + fence3);
            return block('\u0000CODE' + (codeBlocks.length - 1) + '\u0000');
          }
          const raw = e.textContent.replace(/\n/g, ' '), fence = ticks(raw);
          const pad = /^`|`$/.test(raw) || (/^ .* $/.test(raw) && /\S/.test(raw)) ? ' ' : '';
          return fence + pad + raw + pad + fence;
        }
        if (tag === 'BR') return cell ? '<br>' : '  \n';
        if (tag === 'HR') return block('---');
        if (tag === 'UL' || tag === 'OL') {
          let number = Number(e.getAttribute('start') || 1);
          return block([...e.children].filter(n => n.tagName === 'LI').map(li => {
            if (li.hasAttribute('value')) number = Number(li.getAttribute('value'));
            const marker = tag === 'OL' ? number++ + '. ' : '- ';
            const value = children(li).trim().replace(/\n{3,}/g, '\n\n');
            return marker + value.replace(/\n/g, '\n' + ' '.repeat(marker.length));
          }).join('\n'));
        }
        if (tag === 'TABLE') {
          const rows = [...e.querySelectorAll('tr')].filter(r => r.closest('table') === e);
          if (!rows.length) return '';
          const values = rows.map(r => [...r.children].filter(c => /^(TH|TD)$/.test(c.tagName)).map(c =>
            children(c, true).trim().replace(/\s*\n+\s*/g, '<br>').replace(/\|/g, '\\|')));
          const width = Math.max(...values.map(r => r.length));
          const row = r => '| ' + Array.from({length:width}, (_, i) => r[i] || '').join(' | ') + ' |';
          return block([row(values[0]), row(Array(width).fill('---')), ...values.slice(1).map(row)].join('\n'));
        }
        const value = children(e, cell);
        if (/^H[1-6]$/.test(tag)) return block('#'.repeat(Number(tag[1])) + ' ' + value.trim());
        if (tag === 'BLOCKQUOTE') return block(value.trim().replace(/\n{3,}/g, '\n\n').split('\n').map(s => '> ' + s).join('\n'));
        if (tag === 'STRONG' || tag === 'B' || e.hasAttribute('data-d-default-strong')) return '**' + value + '**';
        if (tag === 'EM' || tag === 'I' || e.getAttribute('data-d-font-style') === 'italic') return '*' + value + '*';
        if (tag === 'DEL' || tag === 'S' || tag === 'STRIKE') return '~~' + value + '~~';
        if (tag === 'A') {
          const href = e.getAttribute('href');
          if (!href || /^javascript:/i.test(href)) return value;
          const url = href.replace(/\\/g, '%5C').replace(/\s/g, c => encodeURIComponent(c)).replace(/</g, '%3C').replace(/>/g, '%3E');
          const title = e.getAttribute('title');
          return '[' + value + '](<' + url + '>' + (title ? ' "' + title.replace(/\\/g, '\\\\').replace(/"/g, '\\"') + '"' : '') + ')';
        }
        if (tag === 'P' || tag === 'DIV' || tag === 'SECTION' || tag === 'ARTICLE') return block(value.trim());
        return value;
      };
      // Tool-only answers are a literal wire protocol, not Markdown prose.
      // Preserve their JSON (including backslashes) and historical exact ACK.
      const raw = clone.textContent.trim();
      text = /^(?:<opencode_bridge_call>[\s\S]*<\/opencode_bridge_call>\s*)+$/.test(raw)
        ? raw : children(clone).trim().replace(/\n{3,}/g, '\n\n');
      // Expand after layout normalization, retaining blank lines even in nested
      // lists/quotes (each fenced line inherits its container's indentation).
      text = text.replace(/(^[^\S\n]*(?:> )*)?\u0000CODE(\d+)\u0000/gm, (_, prefix = '', i) =>
        codeBlocks[Number(i)].split('\n').map(line => prefix + line).join('\n'));
    }
  }
  const buttons = [...document.querySelectorAll('button')];
  const alerts = [...document.querySelectorAll('[role="alert"], [data-testid="error-notification"], .text-token-text-error')];
  const turn = last?.closest('[data-talvt-turn-state]');
  return {ids:candidates.map(key), lastID:last ? key(last) : '', lastAssistant:text, reasoning,
    fresh:!!last, complete:turn?.getAttribute('data-talvt-turn-state') === 'complete',
    stopBtn:!!q('[data-testid="stop-button"]') || buttons.some(b => /stop/i.test(b.getAttribute('aria-label') || '')),
    continueBtn:!!q('[data-testid="continue-generate-button"]') || buttons.some(b => /continue generating/i.test((b.textContent || '').trim())),
    errorText:alerts.some(e => /Something went wrong|There was an error|Unable to|failed|try again/i.test(e.innerText || e.textContent || ''))};
})
