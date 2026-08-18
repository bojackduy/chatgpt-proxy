# Handoff: chatgpt-proxy

> **Created:** 2026-08-18
> **Stack:** Go 1.26 · gorilla/websocket v1.5.3 · zero other deps
> **Location:** `~/Documents/AI_workspaces/chatgpt-proxy`
> **Status:** ✅ MVP ทำงานครบ (unit + integration test ผ่าน, live smoke test ผ่าน)
> **Goal:** ให้ ChatGPT Web (หรือ MCP client ใดๆ) ควบคุมเครื่อง local ผ่าน tunnel **outbound-only** — ไม่ต้องเปิด public port

---

## TL;DR — สิ่งที่ทำเสร็จแล้ว

```
ChatGPT Web ──HTTPS(MCP Streamable HTTP)──▶ [Relay] ◀──WebSocket(outbound)── [Agent บนเครื่อง]
```

- **Relay** (`cmd/relay`) — server สาธารณะ รับ MCP ผ่าน `POST /mcp`, forward `tools/list`/`tools/call` ไปหา agent ผ่าน WebSocket tunnel
- **Agent** (`cmd/agent`) — รันบนเครื่อง local, **dial ออก** ไปหา relay (ไม่ต้องเปิด port), execute tools จริง, reconnect อัตโนมัติแบบ exponential backoff
- **Policy** — บล็อกคำสั่งทำลายล้าง/ลบโดย default (`ALLOW_DELETE=true` ถึงจะยอม) + จำกัดขอบเขตด้วย `WORKSPACE_ROOT`

### Tools ที่มี (12 ตัว)
`read_file` · `search_text` · `apply_patch` · `workspace_list` · `git_status` · `git_diff` · `git_log` · `shell` · `process_list` · `process_start` · `process_stop` · `system_info`

---

## โครงสร้างโปรเจกต์

```
chatgpt-proxy/
├── cmd/
│   ├── relay/main.go        # entrypoint relay (env: RELAY_ADDR, AGENT_TOKEN, CLIENT_TOKEN, AGENT_TIMEOUT)
│   └── agent/main.go        # entrypoint agent (env: RELAY_URL, AGENT_TOKEN, AGENT_NAME, WORKSPACE_ROOT, ALLOW_DELETE)
├── internal/
│   ├── mcp/mcp.go           # MCP/JSON-RPC protocol types + helpers (hand-rolled, ไม่ใช้ SDK)
│   ├── relay/
│   │   ├── server.go        # MCP Streamable HTTP endpoint + /ws/agent + /healthz
│   │   └── hub.go           # agent registry (round-robin pick) + tunnel request/response correlation
│   ├── agent/agent.go       # outbound WS client + tool dispatch + reconnect
│   └── tools/
│       ├── tools.go         # aggregate Tools struct + List() + Call() dispatcher
│       ├── file.go          # read_file, search_text, apply_patch, workspace_list
│       ├── git.go           # git_status, git_diff, git_log
│       ├── shell.go         # shell, process_list/start/stop
│       ├── system.go        # system_info
│       └── policy.go        # destructive-command denylist
├── bin/                     # build artifacts (git-ignored)
└── README.md / docs/        # docs ครบชุด
```

---

## Verified ✅ (อย่าโม้เกินจริง — นี้คือหลักฐาน)

| รายการ | ผล | หลักฐาน |
|---|---|---|
| `go build ./...` + `go vet ./...` + `gofmt` | ✅ clean | — |
| `go test ./...` | ✅ ผ่านทั้งหมด | relay + tools packages |
| relay ↔ agent tunnel (WS) | ✅ | `TestToolsListAndCall`, `TestInitialize` |
| policy บล็อก `rm -rf` | ✅ | `TestPolicyBlocksDestructiveShell` |
| workspace restriction | ✅ | `TestWorkspaceRestriction` |
| SSE response | ✅ | `TestSSEResponse` |
| **Live smoke test** (binary จริง) | ✅ | relay :8791 + agent จริง → initialize / tools/list (12 tools) / system_info / บล็อก `rm -rf` / shell echo / apply_patch นอก workspace ถูกปฏิเสธ |

### Bug ที่เจอระหว่าง dev และแก้แล้ว
1. **`Agent.Stop()` panic `close of nil channel`** — `stopCh`/`done` ไม่ถูก init → แก้ด้วย lazy init ผ่าน `sync.Once` (`agent.go: init()`)
2. **Agent ส่ง token ผ่าน `Authorization` header แต่ relay เช็คแค่ `?token=` query** → bad handshake → แก้ให้ relay รับได้ทั้งสองแบบ (`server.go: handleAgentWS`)
3. **`shell` result แสดงเป็น Go struct ดิบ** (`{0 hello-agent }`) → แก้ให้ format เป็น `exit code / stdout / stderr` ใน `tools.go: Call()`
4. **`workspace_list` root ว่าง (default `.`) ถูก `checkPath` ปฏิเสธ** เมื่อ cwd ของ agent อยู่นอก `WORKSPACE_ROOT` → แก้ให้ default เป็น workspace root ตัวแรกเมื่อตั้งไว้ (`file.go: WorkspaceList`)
5. **`git_status/diff/log` คืน `isError: false` ทั้งที่ git ล้มเหลว (exit 128)** → แก้ให้ `gitRun` คืน error เมื่อ exit code ≠ 0 (`git.go`)

---

## Known limitations / TODO (P0 → P3)

| Priority | รายการ | หมายเหตุ |
|---|---|---|
| **P1** | `GET /mcp` (SSE stream) ยังไม่รองรับ — รองรับแค่ `POST` | MCP client ส่วนใหญ่ (ChatGPT, Cursor) ใช้ POST ได้ แต่ client บางตัวอาจต้องใช้ GET |
| **P1** | Session เป็นแบบ stateless — ออก `Mcp-Session-Id` แต่ไม่ validate | ใช้ได้จริง แต่ยังไม่ใช่ production-grade |
| **P1** | ยังไม่มี **rate limiting** บน `/mcp` | จำเป็นก่อนเปิด public |
| **P2** | Routing เลือก agent แค่แบบ round-robin ตัวแรก | ยังไม่มี concept "client X → agent Y" |
| **P2** | `apply_patch` แบบ unified diff ต้องพึ่ง binary `patch` บนเครื่อง | fallback เป็น edits (find/replace) ได้เสมอ |
| **P2** | Relay ไม่มี TLS ในตัว — ต้อง reverse proxy ข้างหน้า (Caddy/Nginx/CF Tunnel) | ตั้งใจไว้แบบนี้อยู่แล้ว (ดู docs/DEPLOYMENT) |
| **P3** | `process_start` ยังไม่ reap child (zombie) | minor |
| **P3** | ยังไม่มี logging/observability กลาง | ใช้ stdout ของ process ก่อน |

### Tools ที่ยังไม่มี (จากแรงบันดาลใจที่ใบ้มา — เติมได้ต่อ)
`office` (Excel/Word) · `clipboard` · `screenshot`/`vision` · `window` (Windows) · `scheduler` (Scheduled Task) · `notification` · `dom_cdp` (ควบคุม Chrome) · `codex_run` (เรียก Codex CLI) · `audio`/`screen_record` · `web_fetch` — วิธีเพิ่ม: ดู `docs/EXTENDING.md`

---

## ขั้นต่อไป (ลำดับแนะนำ)

### Phase 1 — เอาไปใช้จริง
- [ ] Deploy relay ไป server สาธารณะ + HTTPS (ดู `docs/DEPLOYMENT.md` — มีทั้ง Caddy / fly.io / Cloudflare Tunnel)
- [ ] รัน agent บนเครื่อง local พร้อม `WORKSPACE_ROOT` + `AGENT_TOKEN`/`CLIENT_TOKEN` แยกกัน
- [ ] เชื่อม ChatGPT Web → Settings → MCP servers → `https://<relay>/mcp` + Bearer `CLIENT_TOKEN`
- [ ] ทดสอบ: ขอให้ ChatGPT อ่านไฟล์/ค้นโค้ด/รัน `git status` บนเครื่อง

### Phase 2 — Hardening (ก่อนเปิดให้คนอื่น / เปิด public)
- [ ] เพิ่ม rate limiting บน `/mcp`
- [ ] เพิ่ม audit log (ใครเรียก tool อะไร เมื่อไหร่) ฝั่ง relay
- [ ] รองรับ `GET /mcp` SSE + session validation
- [ ] ตั้งใจตรวจ policy regex ให้ครอบคลุม (โดยเฉพาะ Windows: `del`, `rmdir`, `format`)

### Phase 3 — เพิ่ม tools / ฟีเจอร์
- [ ] เพิ่ม tools ตามรายการ "ยังไม่มี" ข้างบน
- [ ] ระบบ approval: คำสั่งอันตราย → ส่ง notification ให้เจ้าของเครื่อง approve ก่อน execute (แทนการบล็อกทิ้ง)
- [ ] หลาย agent / หลายเครื่อง ต่อ relay ตัวเดียว เลือก target ได้

---

## หมายเหตุสำคัญสำหรับคนรับช่วง

1. **MCP protocol เป็นแบบ hand-rolled** (`internal/mcp`) — จงใจไม่ใช้ official SDK เพื่อควบคุม wire format และลด dependency ระวังเวลาแก้ต้องเช็คทั้ง 2 ฝั่ง (relay ↔ agent ใช้ JSON-RPC รูปแบบเดียวกัน)
2. **Policy อยู่ฝั่ง agent เท่านั้น** — ถ้า agent ถูก bypass (โดน hack ฝั่ง local) policy ก็ไร้ผล ต้องเชื่อใจว่าเครื่องที่รัน agent เป็นของเรา
3. **`CLIENT_TOKEN` ว่าง = `/mcp` ไม่มี auth** — ห้ามลืมตั้งก่อนเปิด public
4. **Token ทั้งคู่ควรเป็น secret คนละตัว** ไม่ควรใช้ตัวเดียวกัน
5. ดู `README.md` สำหรับ quick start, `docs/ARCHITECTURE.md` สำหรับ protocol deep-dive
