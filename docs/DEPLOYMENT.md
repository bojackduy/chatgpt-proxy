# chatgpt-proxy — Deployment & Connecting ChatGPT

This guide walks through getting chatgpt-proxy **actually working** with
ChatGPT Web, end to end.

## What you need

1. A **public server** (any VPS, or a serverless/container host) — this runs
   the relay.
2. A **domain** pointed at that server, with HTTPS (ChatGPT's remote MCP
   connector refuses plain `http://`).
3. Your **local machine** — runs the agent (outbound connection only).

---

## Step 1 — Build the binaries

```bash
go build -o bin/relay ./cmd/relay
go build -o bin/agent ./cmd/agent
```

## Step 2 — Deploy the relay

### Option A: plain VPS + Caddy (simplest, auto-HTTPS)

Install Caddy on the server, then a single `Caddyfile`:

```
relay.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

Run the relay behind it:

```bash
export AGENT_TOKEN="$(openssl rand -hex 24)"
export CLIENT_TOKEN="$(openssl rand -hex 24)"
export RELAY_ADDR="127.0.0.1:8080"
./bin/relay
```

Caddy terminates TLS and gives you `https://relay.example.com/mcp`.

> Save `AGENT_TOKEN` and `CLIENT_TOKEN` somewhere safe — you'll need both.

### Option B: fly.io (serverless-ish, free tier)

Add a `Dockerfile`:

```dockerfile
FROM golang:1.26 AS build
WORKDIR /app
COPY . .
RUN CGO_ENABLED=0 go build -o /relay ./cmd/relay

FROM alpine
COPY --from=build /relay /relay
ENV RELAY_ADDR=:8080
EXPOSE 8080
ENTRYPOINT ["/relay"]
```

```bash
fly launch --no-deploy
fly secrets set AGENT_TOKEN=... CLIENT_TOKEN=...
fly deploy
```

fly.io gives you an `https://` URL automatically.

### Option C: Cloudflare Tunnel (no public IP needed)

Run the relay on any box (even a free one), then:

```bash
cloudflared tunnel create chatgpt-proxy
cloudflared tunnel route dns chatgpt-proxy relay.example.com
cloudflared tunnel run chatgpt-proxy
```

> **Quick tunnel (no account, great for testing):** `scripts/start-local.sh`
> runs relay + agent + a throwaway `https://*.trycloudflare.com` tunnel in
> one command — perfect for a first ChatGPT Web test.
>
> ⚠️ **Use `--protocol http2`.** cloudflared's default QUIC transport can be
> unstable on some networks (tunnel registers but requests never answer).
> Forcing HTTP/2 over TCP makes the tunnel reliable:
> `cloudflared tunnel --url http://127.0.0.1:8080 --protocol http2`

with `config.yml`:

```yaml
tunnel: chatgpt-proxy
ingress:
  - hostname: relay.example.com
    service: http://localhost:8080
  - service: http_status:404
```

## Step 3 — Run the agent on your machine

```bash
export RELAY_URL="wss://relay.example.com/ws/agent"
export AGENT_TOKEN="<the relay's AGENT_TOKEN>"
export AGENT_NAME="macbook-pro"
export WORKSPACE_ROOT="/Users/you/Projects"   # optional, comma-separated for many
./bin/agent
```

Expected log:

```
agent: starting, workspace roots=[...]
agent: connected to relay wss://relay.example.com/ws/agent
```

Verify the relay sees you:

```bash
curl https://relay.example.com/healthz
# {"ok":true,"agents":1}
```

## Step 4 — Connect ChatGPT Web

1. Open **ChatGPT** (web at chatgpt.com or the desktop app).
2. Click your profile → **Settings** → **MCP servers** (or **Connectors**,
   depending on your ChatGPT version).
3. **Add server**:
   - **URL:** `https://relay.example.com/mcp`
   - **Authentication:** Bearer token → paste `CLIENT_TOKEN`.
4. ChatGPT should list the 12 tools. Try asking:
   - *"List the files in my workspace"* → `workspace_list`
   - *"Search for 'TODO' in my project"* → `search_text`
   - *"Read src/main.go"* → `read_file`
   - *"Run git status"* → `git_status`

> If ChatGPT doesn't show the tools, check the relay log for the
> `tools/list` call and the agent log for `tool call: ...`. Also confirm the
> URL is `https://` and the Bearer token matches `CLIENT_TOKEN`.

Any other MCP client works identically — Claude Desktop, Cursor, `mcp`
CLI, etc. just point at the same `/mcp` URL with the same Bearer token.

---

## Security checklist (before opening to others)

- [ ] `CLIENT_TOKEN` set (empty = anyone can call `/mcp`).
- [ ] `AGENT_TOKEN` set and different from `CLIENT_TOKEN`.
- [ ] Relay behind HTTPS.
- [ ] `WORKSPACE_ROOT` set on the agent so it can't wander the whole disk.
- [ ] `ALLOW_DELETE` **not** set (destructive commands stay blocked).
- [ ] Consider adding rate limiting (TODO, see `HANDOFF.md`).

## Troubleshooting

| Symptom | Fix |
|---|---|
| `websocket: bad handshake` in agent log | Wrong `AGENT_TOKEN`, or relay not up / wrong port. |
| `/healthz` shows `agents:0` | Agent not connected; check its log, confirm `RELAY_URL` scheme is `wss://`. |
| ChatGPT: "could not reach server" | URL must be `https://`; check the domain resolves and TLS works (`curl https://relay.example.com/healthz`). |
| Tool call times out | Long commands — raise `AGENT_TIMEOUT` on the relay, or pass `timeout_seconds` in `shell`. |
