# chatgpt-proxy

Give ChatGPT Web (or any MCP client) control of your local machine through an
**outbound-only tunnel** — no public ports, no port forwarding, no router
config. Inspired by setups that wire ChatGPT Web to a local machine via an
"OpenAI Secure MCP Tunnel" style bridge.

```
┌─────────────┐   HTTPS / MCP Streamable HTTP   ┌──────────────────┐   WebSocket (outbound)   ┌──────────────────┐
│ ChatGPT Web │ ───────────────────────────────▶│   Relay Server   │ ◀────────────────────────│  Local Agent     │
│ (MCP client)│   POST /mcp  (tools/list,       │   (public host)  │   ws://relay/ws/agent    │  (your machine)  │
└─────────────┘   tools/call, initialize)       └──────────────────┘   (no inbound ports!)    └──────────────────┘
```

- **Relay** — a public Go server. MCP clients connect to it over HTTPS; it
  forwards `tools/list` / `tools/call` to whichever agent is connected.
- **Agent** — runs on your machine, dials **out** to the relay, executes tools
  locally (read/write files, git, shell, processes, system info).
- **Policy** — full access to add/update/edit, but destructive operations
  (`rm -rf`, `git push --force`, `shutdown`, …) are **blocked by default**.
  Deletes need explicit opt-in. Access can be restricted to workspace roots.

## Quick start

> 📚 Full docs: [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) (protocol),
> [`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md) (deploy + connect ChatGPT),
> [`docs/EXTENDING.md`](docs/EXTENDING.md) (add tools).

### 0. Try it locally (no server needed)

**Easiest — one command, includes a public HTTPS tunnel you can paste into
ChatGPT right away:**

```bash
./scripts/start-local.sh
```

It starts relay + agent + a cloudflared quick tunnel (`--protocol http2`),
then prints the MCP URL and Bearer token. Keep the terminal open and add the
URL in ChatGPT Web → Settings → MCP servers.

**Or manually** — run relay and agent on the same machine to see the whole
flow in 30 seconds:

```bash
# terminal 1 — relay
export AGENT_TOKEN=dev-secret CLIENT_TOKEN=dev-client
./bin/relay

# terminal 2 — agent
export RELAY_URL=ws://127.0.0.1:8080/ws/agent AGENT_TOKEN=dev-secret
./bin/agent

# terminal 3 — call the MCP endpoint
curl -X POST http://127.0.0.1:8080/mcp \
  -H 'Content-Type: application/json' -H 'Authorization: Bearer dev-client' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
```

### 1. Build

```bash
go build -o bin/relay ./cmd/relay
go build -o bin/agent ./cmd/agent
```

### 2. Run the relay (on a public server)

```bash
export AGENT_TOKEN="$(openssl rand -hex 24)"   # agents authenticate with this
export CLIENT_TOKEN="$(openssl rand -hex 24)"  # MCP clients (ChatGPT) authenticate with this
export RELAY_ADDR=":8080"
./bin/relay
```

> ⚠️ Put the relay behind HTTPS (Caddy, Nginx, Cloudflare Tunnel, fly.io, …).
> ChatGPT's remote MCP connector requires `https://`.

### 3. Run the agent (on your local machine)

```bash
export RELAY_URL="wss://relay.example.com/ws/agent"
export AGENT_TOKEN="<same token as relay>"
export AGENT_NAME="macbook-pro"
export WORKSPACE_ROOT="/Users/you/Projects"    # optional; comma-separated for many
./bin/agent
```

The agent reconnects automatically with backoff. Logs show `connected to
relay ...` when the tunnel is up.

### 4. Connect ChatGPT Web

1. Open **ChatGPT** (web or desktop) → **Settings** → **MCP servers** → **Add server**.
2. Enter: `https://relay.example.com/mcp`
3. Authentication: **Bearer** token = `CLIENT_TOKEN`.
4. The tools from `tools/list` appear; you can now ask ChatGPT to read files,
   search code, apply patches, run git commands, or run shell commands on your
   machine.

Any other MCP client works too — Claude Desktop, Cursor, `mcp` CLI, etc.

## Tools

| Tool            | Description                                                        |
|-----------------|--------------------------------------------------------------------|
| `read_file`     | Read a file (with optional line window). Binary files rejected.    |
| `search_text`   | Recursive regex search with include/exclude globs.                 |
| `apply_patch`   | Find/replace edits or unified diffs (via `patch -p1`).             |
| `workspace_list`| List projects/subdirectories on the machine.                       |
| `git_status`    | `git status --short`                                               |
| `git_diff`      | `git diff`                                                         |
| `git_log`       | `git log --oneline -n N`                                           |
| `shell`         | Run a command; returns exit code, stdout, stderr.                  |
| `process_list`  | List running processes.                                            |
| `process_start` | Launch a detached command, returns pid.                            |
| `process_stop`  | Kill a process by pid.                                             |
| `system_info`   | Hostname, OS, CPU, memory.                                         |

## Safety model

- **Destructive commands are denied by default** (`internal/tools/policy.go`):
  `rm -rf`, `rmdir`, drive formats, `mkfs`, `shutdown`/`reboot`, `dd`, force
  pushes, `git reset --hard`, SQL drops/deletes, fork bombs, … To opt out on
  the **agent**: `ALLOW_DELETE=true ./bin/agent`.
- **Workspace restriction**: with `WORKSPACE_ROOT` set, file/patch/shell-cwd
  access outside those directories is refused.
- **Two separate tokens**: `AGENT_TOKEN` (agent ↔ relay) and `CLIENT_TOKEN`
  (MCP client ↔ relay). Different secrets, different privileges.
- Tool results are plain text; file writes preserve permissions
  (`0644` for new content).

## Configuration

### Relay env vars

| Var             | Default  | Meaning                                            |
|-----------------|----------|----------------------------------------------------|
| `RELAY_ADDR`    | `:8080`  | Listen address                                     |
| `AGENT_TOKEN`   | —        | **required**; token for `/ws/agent`                |
| `CLIENT_TOKEN`  | —        | Bearer token required on `/mcp` (empty = no auth)  |
| `AGENT_TIMEOUT` | `300`    | Seconds to wait for an agent reply                 |

### Agent env vars

| Var              | Default       | Meaning                                            |
|------------------|---------------|----------------------------------------------------|
| `RELAY_URL`      | —             | **required**; e.g. `wss://relay.example.com/ws/agent` |
| `AGENT_TOKEN`    | —             | **required**; must match the relay                 |
| `AGENT_NAME`     | `local-agent` | Display name                                       |
| `WORKSPACE_ROOT` | —             | Comma-separated allowed directories                |
| `ALLOW_DELETE`   | `false`       | `true` disables the destructive-command policy     |

## API (for debugging)

```bash
# health (agent count)
curl https://relay.example.com/healthz

# initialize
curl -X POST https://relay.example.com/mcp \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer <CLIENT_TOKEN>' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"curl","version":"1"}}}'

# list tools
curl -X POST https://relay.example.com/mcp \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer <CLIENT_TOKEN>' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'

# call a tool
curl -X POST https://relay.example.com/mcp \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer <CLIENT_TOKEN>' \
  -d '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"system_info","arguments":{}}}'
```

Responses are `application/json`; pass `Accept: text/event-stream` to get
SSE framing instead.

## Project layout

```
cmd/relay/           relay server entrypoint
cmd/agent/           local agent entrypoint
internal/mcp/        MCP / JSON-RPC protocol types
internal/relay/      MCP Streamable HTTP endpoint + WebSocket agent hub
internal/agent/      outbound WebSocket client + tool dispatch
internal/tools/      tool implementations + policy
```

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| Agent logs `websocket: bad handshake` | Wrong `AGENT_TOKEN`, or relay not running. Check the relay log. |
| `tools/list` returns `no agent is connected` | Agent not connected yet. Check `/healthz` → `agents` count, agent log should say `connected to relay`. |
| `apply_patch` says `outside the configured workspace roots` | `WORKSPACE_ROOT` restriction working as intended — point the file path inside the allowed roots. |
| Tool says `blocked by policy` | Destructive command denied. If you really want it, set `ALLOW_DELETE=true` on the **agent**. |
| ChatGPT can't reach the relay | Relay must be behind **HTTPS** (`https://`), not plain `http://`. |
| `patch` not found for unified diffs | Install `patch` (macOS: ships by default; Linux: `apt install patch`), or use `edits` (find/replace) instead. |

## Tests

```bash
go test ./...
```

Covers the full tunnel (relay ↔ agent over WebSocket, initialize, tools/list,
tools/call, policy blocking, SSE responses) plus unit tests for the tools.
