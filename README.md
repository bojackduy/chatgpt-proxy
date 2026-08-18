# chatgpt-proxy

An **MCP server** that lets any AI agent (Hermes Agent, Claude Desktop,
Codebuff-style assistants, …) use **ChatGPT Web** — for free, through your own
logged-in session, no API key.

The agent calls `ask_chatgpt(prompt)`, the proxy drives chatgpt.com in a
dedicated Chrome profile via **CDP** (Chrome DevTools Protocol), and returns
the answer as plain text.

```
┌────────────────────┐   MCP (stdio / HTTP)    ┌───────────────┐   CDP    ┌───────────────┐
│  AI agent          │ ───────────────────────▶│ chatgpt-proxy │ ───────▶ │ Chrome        │
│  (Hermes, you, …)  │   ask_chatgpt(prompt)   │  (local MCP   │          │ (chatgpt.com) │
│                    │ ◀───────────────────────│   server)     │ ◀─────── │ your session  │
└────────────────────┘   answer as text        └───────────────┘          └───────────────┘
```

- **Free & unlimited** — rides your ChatGPT Web account (login once, session
  persists in a dedicated Chrome profile).
- **Browser automation, not API** — uses the real web UI via CDP, so it
  survives UI changes better than reverse-engineered endpoints.
- **Standard MCP** — works with any MCP client over `stdio` or Streamable HTTP.

## Quick start

```bash
# 1. Build
go build -o bin/chatgpt-proxy ./cmd/proxy

# 2. First run — open Chrome and log in once
./bin/chatgpt-proxy -wait-login
#    → a Chrome window opens on chatgpt.com; log in there.
#    → the session is saved to ~/.chatgpt-proxy and reused forever.

# 3. Run the MCP server (stdio — what most agents spawn)
./bin/chatgpt-proxy
```

After step 3 the proxy reads MCP messages from stdin and writes responses to
stdout. Wire it into any MCP client.

### Connect Hermes Agent (stdio)

Add to your Hermes `~/.hermes/` MCP config:

```yaml
mcp_servers:
  - name: chatgpt
    command: /Users/you/.../chatgpt-proxy/bin/chatgpt-proxy
```

Then Hermes can call `ask_chatgpt` like any other tool.

### Streamable HTTP mode (for remote / other clients)

```bash
./bin/chatgpt-proxy -transport http -addr 127.0.0.1:8787
# POST http://127.0.0.1:8787/mcp   (SSE or JSON)
# GET  http://127.0.0.1:8787/healthz
```

## Tools

| Tool             | Description                                                                 |
|------------------|-----------------------------------------------------------------------------|
| `ask_chatgpt`    | Send a prompt to ChatGPT Web and return the answer as text.                  |
| `chatgpt_status` | Check that the bridge is ready (logged in, composer present, profile path). |

### `ask_chatgpt` arguments

| Arg               | Type    | Default | Meaning                                  |
|-------------------|---------|---------|------------------------------------------|
| `prompt`          | string  | —       | **Required.** The prompt to send.        |
| `new_session`     | boolean | `true`  | Start a fresh conversation each call. Set `false` to continue the previous one. |
| `timeout_seconds` | number  | `180`   | Max seconds to wait for the answer.      |

## Flags

| Flag             | Default                       | Meaning                                        |
|------------------|-------------------------------|------------------------------------------------|
| `-transport`     | `stdio`                       | `stdio` or `http`                              |
| `-addr`          | `127.0.0.1:8787`              | Listen address (http transport)                |
| `-chrome-port`   | `9222` (auto-picks if busy)   | Chrome remote-debugging port                   |
| `-profile`       | `~/.chatgpt-proxy`            | Chrome profile dir (holds the ChatGPT login)   |
| `-chrome`        | auto-detected                 | Chrome binary path override                    |
| `-open`          | —                             | Open Chrome with the profile and exit          |
| `-wait-login`    | —                             | Open Chrome and poll until logged in, then exit |

## API (HTTP mode, for debugging)

```bash
# initialize
curl -X POST http://127.0.0.1:8787/mcp \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"curl","version":"1"}}}'

# ask
curl -X POST http://127.0.0.1:8787/mcp \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ask_chatgpt","arguments":{"prompt":"hello"}}}'
```

## Project layout

```
cmd/proxy/              MCP server entrypoint (stdio + HTTP)
internal/mcp/           MCP / JSON-RPC protocol types
internal/chatgpt/       Chrome CDP automation of chatgpt.com
internal/server/        MCP dispatch + tool implementations + transports
legacy/                 the earlier relay/agent (outbound tunnel) — kept for reference
```

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `not logged in` | The Chrome window opened but you didn't log in. Run `-wait-login` and log in once. |
| `chatgpt composer not found` | Page layout changed, or a modal is up. Open the proxy Chrome window and check, then retry. |
| `timed out waiting for an answer` | Long response or slow network. Raise `timeout_seconds`. |
| `chrome binary not found` | Pass `-chrome /path/to/chrome`, or install Chrome. |
| Response looks truncated | The proxy auto-clicks "Continue generating" up to 3 times; very long answers still stop there. |
| Port 9222 was busy | The proxy auto-picks the next free port — no action needed. |

> ⚠️ This uses your personal ChatGPT session. Only expose the HTTP transport to
> agents you trust; `stdio` mode binds to nothing on the network.

## Tests

```bash
go test ./...
```

Covers MCP dispatch (initialize, tools/list, tools/call, error paths, SSE +
JSON HTTP responses). The browser layer is verified by the live flow — run
`-wait-login` once, then call `ask_chatgpt`.
