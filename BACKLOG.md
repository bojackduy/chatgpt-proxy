# Backlog

## V2 subagent failures: uncertain turns and long waits

**Status:** investigation deferred; no fix implemented for this report.

**Reported symptom:** General ("Implement ActionBox backend MVP") and Explore
("Inspect browser ingestion options") both display session ID
`ses_ed530eb94ffe6jsmRTCDKTQa7L` and fail with the uncertain-submitted-turn
409/recover instruction; the parent run shows 17m54s elapsed.

**Known versus unknown:** that exact ID was absent from the inspected local
adapter log and journal. A shared ID in the UI is not proof of identical
outgoing request headers. Queueing, replay, recovery and retries may contribute
to elapsed time; the original submission failure has not been correlated.
A separate session had an incomplete tool wrapper followed by recovery comparing
a 10,154-byte rendered prompt with a 90,499-byte submitted prompt. Do not
attribute that separate failure to these subagents without evidence.

**Investigation scope:**
- Correlate parent/child session IDs, outgoing identity headers, resolved adapter
  journal keys and the actual adapter instance/state path. Use metadata-only
  request IDs and gate/lock-wait, submission, generation and recovery timings;
  do not log private prompts or credentials.
- Determine whether distinct child sessions accidentally share identity, or
  intentionally reuse one session. Keep parent, child and auxiliary histories
  isolated without guessing identity from agent names.
- Find the first backend/protocol failure before the repeated 409s. Inspect
  collapsed/virtualized user bubbles and recover complete submitted text when
  possible; preserve exact request binding, not prefix-only acceptance.
- Define bounded, actionable error handling for queue/recovery/retry waits.
  Never blindly clear Dirty or resubmit an uncertain tool turn.

**Acceptance:** a real v2 General/Explore reproduction records each actual
session identity and original failure; distinct sessions cannot poison each
other's journal; safe recoverable turns adopt/cache without duplicate execution;
truly ambiguous turns remain protected and produce useful diagnostics; elapsed
time is attributable to explicit phases rather than unexplained retry loops.
This is separate from the deferred concurrent-tab feature below.

## Terminal-friendly rich visual responses and math

**Status:** scoped implementation verified on 2026-10-11; broader visual support
and dual-host TUI validation remain partial. Not deployed in this run.

**Implemented evidence:** read-only inspection in an owned background CDP tab
under the profile turn lock captured the exact reported card. It is HTML/CSS
`data-d-component="row"` / `data-d-direction="row"`, with nested comparison
rows and explicit winner/elimination text, not a raster image. These semantic
leaf-content rows now become compact Markdown list rows; ordinary div
paragraphs, lists, tables and code keep their structure. Unlabeled decorative
row icons are omitted; their meaning remains in the explicit captions and
elimination summary, without guessing from success/danger colors.

Annotated math retains one source expression and emits supported Unicode/plain
math without dollar delimiters. Powers/subscripts keep ASCII notation and
grouping; unsupported commands preserve the entire original TeX in inline code.
Literal prose/code TeX and tool-call JSON are not converted. Images, SVG and
canvas expose accessible text or an explicit no-description fallback. Only
absolute HTTPS source URLs without credentials/query/fragment and outside
local/private/ChatGPT authenticated hosts are linked; unavailable/authenticated
sources are not invented or exposed. No OCR, new LLM calls or dependencies.

Existing `response_test.go` rich case extended (no new test files/functions).
Real captured answer replay in isolated headless Chrome verifies all four rows
and both reported math expressions; `go vet ./...`, `go test ./...`,
`go build ./cmd/...` and both named binary builds passed. Evidence and staged
binaries: `.opencode/loopd/goals/ce31ae43-e8ec-4d2d-9f00-9ba9d4e3c661/`
(`answer-3.html`, `before.md`, `after.md`, `progress.md`).

**Remaining limitations:** this is not a general visual renderer. Image-only
pixels, canvas geometry, SVG geometry and color-only meaning without accessible
semantic text are not interpreted. Other card layouts use normal extraction.
Actual visual verification in both OpenCode hosts remains outstanding; DOM
and Markdown correctness are proved, not host screenshot equivalence. Deployment
deferred: the shared adapter readiness probe returned 503 with an about:blank
tab/no composer, so no successful safe idle/restart boundary was established;
no restart, Dirty-state repair or unrelated browser recovery was attempted.

**Reported symptoms (screenshots):**
- A web visual card has compact rows `t = 0: abac < abad` through
  `t = 3: c < d`, winner captions, color and elimination marks. OpenCode instead
  puts each label, operand and comparison on separate widely spaced lines,
  losing grouping and making the explanation hard to follow.
- Display math appears literally as `$$`, `R(i+t)<R(j+t)\\qquad 0\\le t\\le k`,
  and `j,\\ j+1,\\ \\ldots,\\ j+k` instead of readable symbols and spacing.
- User reports increasingly image/visual-heavy web answers. Screenshots alone
  do not establish whether each answer is an actual image, SVG/canvas or an
  HTML/CSS card; identify the representation before selecting a conversion.

**Proposed scope and decisions:**
- Consider a small deterministic terminal-formatting layer after extraction,
  not a second general rendering engine. Prefer semantic Markdown tables,
  grouped lists or fenced aligned text for structured cards; preserve row
  relationships/captions and express color-only meaning with text labels.
- Inspect actual image/SVG/canvas outputs separately: preserve accessible
  descriptions and expose a usable artifact link/file or supported preview.
  Define an explicit fallback when content has no text equivalent. Do not
  silently drop it or invent an OCR/vision description; whether to add OCR or
  model-based descriptions is an unresolved opt-in design/cost decision.
- For OpenCode without math rendering, translate supported TeX into readable
  Unicode/plain text: `\\le` → `≤`, `\\ge` → `≥`, `\\ldots` → `…`, `\\qquad` →
  spacing; retain inequalities, ranges, powers/subscripts and expression order.
  Example: `R(i+t) < R(j+t), 0 ≤ t ≤ k` and `j, j+1, …, j+k`.
- Keep conversion scoped to identified math nodes, never code blocks, literal
  TeX examples, tool-protocol JSON or unrelated prose. Define safe fallback for
  unsupported fractions, matrices and commands rather than lossy guessing.
  Choose Unicode versus ASCII fallback and source-preservation behavior before
  implementation; retain accessible single-expression extraction.

**Acceptance:** actual source-DOM fixtures for the reported visual card and
math produce compact, readable OpenCode output; each comparison stays on one
row with its caption; ≤/ranges/powers preserve meaning without raw unsupported
math delimiters or duplicates; image-only output is explicitly represented;
bold/code/links/tables and tool-call JSON remain intact. Verify in both supported
OpenCode hosts, not only by inspecting generated Markdown.

## Genuine concurrent generations (proposal only)

**Status:** deferred; not implemented. Current adapter and cross-process
profile locks intentionally serialize whole turns. One browser/account is
not evidence that concurrent generation is impossible; account/server limits
are unverified.

**Gate first:** run a live two-tab overlapping-generation probe on the same
account. Record submission, generation-start and completion timestamps, prove
overlap rather than two queued requests, and observe rate limits/interruption.
Do not enable concurrency unless this probe succeeds.

**Proposed ownership and locking:**
- One adapter process owns generation orchestration, with a dedicated owned
  tab per active session and a small configurable tab pool (default size 2).
  Concurrency remains flag-gated/off initially.
- Per-session turn/recovery locks serialize duplicate requests, recovery and
  resync within a session; different sessions may generate independently.
- Hold only short state-persistence and browser initialization/reconnection/
  project-setup locks. Replace the profile-wide whole-generation lock only
  after tab ownership and overlap behavior are proven safe.
- MCP uses its own owned tabs and must never adopt or navigate adapter tabs.

**Acceptance before rollout:** timestamped overlap; prompt/answer and thread/
project isolation; canceling one request does not stop the other; duplicate
and recovery safety within each session; restart-safe persistence without
lost updates; pool exhaustion queues safely; reconnect retains ownership;
MCP asks/status cannot steal adapter tabs. Keep serial mode as fallback.

## Project-per-session (1 OpenCode session → 1 ChatGPT Project)

**Status:** IMPLEMENTED flag-gated default-off on 2026-10-10
(`CHATGPT_BRIDGE_PROJECT_PER_SESSION=1`; unset = byte-identical legacy).
Session record +1 field (`Project`); rotation replaces reset-replay with a
single context-card + last-~6-verbatim turn in the session's
`OpenCode Bridge — <repo>/<short8>` project; any infra failure falls back to
the shared-project path (`rotation_fallback`); steady-state/resync
untouched; existing sessions grandfathered. 2026-10-10 eager start added:
new sessions mint their project on turn one (`decision=fresh-project`,
`eager_fallback` pre-submission only) instead of waiting for rotation —
verified live (fresh session answered from its own new project, `Project`
persisted, session clean). `go vet` + `go test ./...` green,
bins rebuilt, launchd restarted, `/readyz` non-503. Phase-0 probe HIT
(fresh "OpenCode Probe" project, codeword PROBE-88 retrieved verbatim by both
the no-card chat B and the with-card chat C; ~30s answers; see
phase0-report.md) — the gate this design was waiting on.

**Idea:** instead of all sessions sharing one "OpenCode Bridge" project with one
thread each, map each OpenCode session to its own dedicated ChatGPT project
(`OpenCode Bridge — <short-session-id>`). Within a project, keep a working chat
plus retired history chats, and rely on ChatGPT's project-level retrieval
([confirmed by OpenAI docs](https://help.openai.com/en/articles/10169521-using-projects-in-chatgpt):
"Chats can reference other conversations within the same project") instead of
replaying full transcripts.

**What it kills:** reset-replay storms, chunk budgets for history, the
normalization arms race for long sessions.

**What it keeps:** delta sync within a working chat, per-thread tool teaching,
dirty/uncertainty guards, resync.

**Design (from plan-mode analysis):**
- Mapping: session ID → project URL + working thread URL (state file +1 field).
- Steady state unchanged (delta-continue the working thread).
- Mismatch/compaction/length triggers **rotation** (fresh working chat in the
  same project) instead of reset-replay: first message = compact context card
  (goal, key decisions, digested tool results) + last N turns verbatim; older
  chats left for retrieval. No replay pastes, ever.
- Fallback mandatory: any project/chat failure drops back to shared-project
  thread behavior; new machinery must never fail-closed.
- Existing sessions grandfathered on the shared project; new sessions get own
  project (flaggable).
- Project creation reuses the proven `ensureProject` UI-dialog flow,
  generalized per session.

**Must validate first (Phase 0 probe, gates everything):** create a project,
put a unique codeword fact in chat A, open chat B in the same project and ask
for it — with and without a context card. If retrieval misses, stop and report;
do not build further. "Can reference" ≠ guaranteed recall.

**Open decisions for the user:** sidebar noise (N sessions = N projects,
contradicts the original no-noise goal — cap/reuse strategy?); naming
(session-short-id vs repo/task name); if probe is weak, rotation-with-card
only or drop entirely; rollout behind a flag vs default.

**Related constraints:** new projects need default (not project-only) memory
for cross-chat references — verify the creation default; creation adds ~15-30s
first-turn latency; unknown creation rate limits.
