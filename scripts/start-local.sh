#!/usr/bin/env bash
# chatgpt-proxy — start relay + agent + public HTTPS tunnel in one command.
#
# Run this in YOUR terminal, keep it open, then paste the printed URL into
# ChatGPT Web → Settings → MCP servers.
#
# Env overrides:
#   AGENT_TOKEN   agent↔relay secret (default: random)
#   CLIENT_TOKEN  client↔relay secret (default: random)
#   WORKSPACE_ROOT  comma-separated dirs the agent may touch (default: this project)
#   RELAY_PORT    local relay port (default 8080)
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT="$(pwd)"

AGENT_TOKEN="${AGENT_TOKEN:-$(openssl rand -hex 16)}"
CLIENT_TOKEN="${CLIENT_TOKEN:-$(openssl rand -hex 16)}"
WORKSPACE_ROOT="${WORKSPACE_ROOT:-$ROOT}"
RELAY_PORT="${RELAY_PORT:-8080}"

echo "🔨 building binaries..."
go build -o bin/relay ./cmd/relay
go build -o bin/agent ./cmd/agent

echo "🚀 starting relay on :${RELAY_PORT} ..."
env AGENT_TOKEN="$AGENT_TOKEN" CLIENT_TOKEN="$CLIENT_TOKEN" RELAY_ADDR=":${RELAY_PORT}" ./bin/relay > /tmp/cgp-relay.log 2>&1 &
RELAY_PID=$!

echo "🤖 starting agent (workspace: ${WORKSPACE_ROOT}) ..."
env RELAY_URL="ws://127.0.0.1:${RELAY_PORT}/ws/agent" AGENT_TOKEN="$AGENT_TOKEN" AGENT_NAME="$(hostname -s)" WORKSPACE_ROOT="$WORKSPACE_ROOT" ./bin/agent > /tmp/cgp-agent.log 2>&1 &
AGENT_PID=$!

echo "🌐 opening public HTTPS tunnel (cloudflared http2) ..."
cloudflared tunnel --url "http://127.0.0.1:${RELAY_PORT}" --protocol http2 --no-autoupdate > /tmp/cgp-tunnel.log 2>&1 &
TUNNEL_PID=$!

cleanup() {
  echo ""
  echo "🛑 shutting down..."
  kill "$TUNNEL_PID" "$AGENT_PID" "$RELAY_PID" 2>/dev/null || true
  exit 0
}
trap cleanup INT TERM

# wait for the tunnel URL
PUBLIC_URL=""
echo "⏳ waiting for tunnel..."
for _ in $(seq 1 30); do
  PUBLIC_URL="$(grep -oE 'https://[a-z0-9-]+\.trycloudflare\.com' /tmp/cgp-tunnel.log 2>/dev/null | head -1 || true)"
  [ -n "$PUBLIC_URL" ] && break
  sleep 2
done
sleep 5  # let the edge start routing

if [ -z "$PUBLIC_URL" ]; then
  echo "❌ tunnel failed — check /tmp/cgp-tunnel.log"
  cleanup
fi

# wait until the public URL actually answers
echo "⏳ verifying public URL..."
for _ in $(seq 1 30); do
  if curl -s --max-time 5 "$PUBLIC_URL/healthz" 2>/dev/null | grep -q '"ok":true'; then
    break
  fi
  sleep 2
done

echo ""
echo "──────────────────────────────────────────────────────────────"
echo "  ✅ READY — connect ChatGPT Web now"
echo ""
echo "  MCP server URL:   ${PUBLIC_URL}/mcp"
echo "  Auth type:        Bearer"
echo "  Bearer token:     ${CLIENT_TOKEN}"
echo ""
echo "  ChatGPT Web → Settings → MCP servers → Add server"
echo "  (keep this terminal open — Ctrl+C to stop)"
echo "──────────────────────────────────────────────────────────────"
echo ""
echo "logs: /tmp/cgp-relay.log  /tmp/cgp-agent.log  /tmp/cgp-tunnel.log"
echo "health: curl ${PUBLIC_URL}/healthz"

# block until the user stops it
wait "$TUNNEL_PID"
