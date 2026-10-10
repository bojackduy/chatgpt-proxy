# Backlog

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
