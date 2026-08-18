# chatgpt-proxy — Architecture

This document explains how the pieces fit together, the wire protocols, and
the message flows. Read this before modifying the code.

## Overview

```
                          Internet / HTTPS                      Outbound WebSocket
┌─────────────┐   MCP Streamable HTTP (JSON-RPC 2.0)   ┌──────────────────┐   (no inbound ports)   ┌──────────────────┐
│ ChatGPT Web │ ────── POST /mcp ─────────────────────▶│   Relay Server   │ ──────────────────────▶│   Local Agent    │
│ (MCP client)│ ◀───── json | text/event-stream ───────│   (public host)  │ ◀──────────────────────│   (your machine) │
└─────────────┘                                        └──────────────────┘                        └──────────────────┘
                                                               │                                        │
                                                     forward tools/list, tools/call         execute tools locally
```

Key property: **the agent is the one that dials out**. The relay never needs
to reach the agent, so the agent can sit behind NAT / CGNAT / a hotel Wi-Fi /
a phone hotspot. The only thing that must be publicly reachable is the relay.

## Components

| Component | Package | Role |
|---|---|---|
| Relay | `internal/relay` | Public gateway. Exposes MCP over HTTP, holds agent connections, correlates requests. |
| Agent | `internal/agent` | Outbound WS client. Receives forwarded JSON-RPC, dispatches to tools, returns results. |
| Tools | `internal/tools` | The local execution layer + safety policy. |
| Protocol | `internal/mcp` | Shared MCP / JSON-RPC 2.0 types used by both sides. |

## Wire protocols

### 1. Client ↔ Relay: MCP Streamable HTTP

The relay implements the MCP **Streamable HTTP** transport (a subset):

- `POST /mcp` with `Content-Type: application/json` and a single JSON-RPC
  message in the body.
- Response is `application/json` by default, or `text/event-stream` (SSE)
  when the client sends `Accept: text/event-stream`.
- `initialize` responses carry an `Mcp-Session-Id` header. The relay is
  **stateless** — the session id is issued but not validated (see
  `internal/relay/server.go: handleMCP`).
- `GET /mcp` (SSE stream keep-alive) is **not** implemented; POST-only.

Methods handled by the relay itself:

| Method | Behavior |
|---|---|
| `initialize` | Echoes the client's protocol version, advertises `tools` capability, returns `serverInfo`. |
| `ping` | Returns `{}`. |
| `notifications/initialized` | `202 Accepted`, no body. |
| `resources/*`, `prompts/*`, `logging/*` | Return empty / method-not-supported so clients degrade gracefully. |

Methods forwarded to the agent through the tunnel:

| Method | Behavior |
|---|---|
| `tools/list` | Forwarded to the agent; the agent builds the tool list from `internal/tools`. |
| `tools/call` | Forwarded with `params` (tool name + arguments) untouched. |

### 2. Relay ↔ Agent: WebSocket tunnel

The agent connects to `wss://<relay>/ws/agent` (optionally `?token=...` or an
`Authorization: Bearer` header). Each WebSocket text message is a JSON-RPC
2.0 message — the **same shape** the client uses, so the relay is a thin
pass-through.

On connect the agent sends a relay-level hello (not JSON-RPC):

```json
{ "type": "hello", "name": "macbook-pro" }
```

The relay then forwards requests:

```json
{ "jsonrpc": "2.0", "id": "<hex>", "method": "tools/call", "params": { "name": "read_file", "arguments": { "path": "/x/y.go" } } }
```

and the agent replies:

```json
{ "jsonrpc": "2.0", "id": "<hex>", "result": { "content": [ { "type": "text", "text": "..." } ], "isError": false } }
```

**Request correlation** (`internal/relay/hub.go`): the relay assigns a fresh
request id per forwarded call, stores a `chan []byte` in the agent's
`inflight` map, and waits (bounded by `AGENT_TIMEOUT`). When the agent's
response arrives, `deliver()` routes it back by id. The relay then rewraps
the result under the **client's** original id.

**Heartbeat**: the agent sends a WebSocket control `PingMessage` every 20s so
both sides detect dead connections; the relay re-reads each agent's socket.

**Reconnect**: the agent re-dials with exponential backoff
(1s → 2s → 4s → … capped at 30s) and re-sends its hello. The relay
re-registers it (replacing any stale connection for the same id).

## Request lifecycle (a `tools/call` end to end)

```
ChatGPT                         Relay                            Agent
  │  POST /mcp tools/call         │                                 │
  ├──────────────────────────────▶│  pick agent (round-robin)       │
  │                               ├────────────────────────────────▶│
  │                               │  JSON-RPC tools/call (new id)   │
  │                               │                                 │  execute tool
  │                               │                                 │  (policy check → run)
  │                               │◀────────────────────────────────┤
  │                               │  JSON-RPC result                │
  │  json | SSE response          │                                 │
  │◀──────────────────────────────┤                                 │
```

## Safety model

- **Policy is enforced on the agent** (`internal/tools/policy.go`), because
  that's where the destructive operation would actually run. A blocked
  command returns `isError: true` with a message explaining why.
- **Workspace roots** (`WORKSPACE_ROOT`) are also enforced on the agent:
  `read_file`, `apply_patch`, `shell` (cwd), `search_text` and
  `workspace_list` all refuse paths outside the allowed roots.
- **Two tokens**: `AGENT_TOKEN` (agent↔relay) and `CLIENT_TOKEN`
  (client↔relay) are checked in different places and should be different
  secrets. Empty `CLIENT_TOKEN` = `/mcp` is unauthenticated.

## Code map (where to change things)

| You want to… | Edit |
|---|---|
| Add a tool | `internal/tools/tools.go` (`List` + `Call`) + a new file in `internal/tools/`. See `docs/EXTENDING.md`. |
| Change the safety policy | `internal/tools/policy.go` (`DefaultDenyPatterns`). |
| Change tunnel transport | `internal/agent/agent.go` + `internal/relay/hub.go`. |
| Add an MCP method | `internal/relay/server.go` (`dispatch`) + `internal/mcp/mcp.go` (method constants). |
| Change timeouts | `RELAY_ADDR`/`AGENT_TIMEOUT` env on relay; `timeout_seconds` per shell call. |

## Testing

- `internal/tools/tools_test.go` — unit tests for file tools + policy.
- `internal/relay/server_test.go` — **integration**: spins up a real relay
  over `httptest`, connects a real agent over WebSocket, and drives the full
  MCP flow (initialize, tools/list, tools/call, policy blocking, SSE).

Run everything with `go test ./...`.
