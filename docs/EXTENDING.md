# chatgpt-proxy — Extending: adding new tools

Adding a tool is a two-step job: **implement it** (in `internal/tools`) and
**register it** (in `List()` and `Call()`). That's it — the relay and agent
pick it up automatically because they forward `tools/list` / `tools/call`
verbatim.

## Step 1 — Implement the tool

Create a file in `internal/tools/` (or add to an existing one). A tool is
just a method on the `*Tools` struct that returns `(string, error)`:

```go
// internal/tools/uptime.go
package tools

import "time"

// UptimeArgs are the arguments of the uptime tool.
type UptimeArgs struct {
    // optional fields...
}

// Uptime reports how long the machine has been running.
func (t *Tools) Uptime(args UptimeArgs) (string, error) {
    // ... real implementation ...
    return time.Since(bootTime).Round(time.Second).String(), nil
}
```

Conventions:

- **Arguments**: a struct with `json` tags; decode via the existing
  `jsonArgs[T]` helper in `Call()`. Empty args struct is fine.
- **Errors**: return `(string, error)`; `Call()` wraps errors into an
  `isError: true` tool result automatically.
- **Safety**: if the tool can do something destructive, gate it on
  `t.Policy` (see `internal/tools/policy.go`). If it touches the filesystem
  or runs commands, call `t.checkPath(...)` first so `WORKSPACE_ROOT` is
  respected.
- **Reuse**: shell out via `t.Shell(...)` or `execOutput(...)`; read files
  via `os.ReadFile`; check policy with `t.Policy.CheckShell(...)`.

## Step 2 — Register the tool

Two places in `internal/tools/tools.go`:

### 2a. Add the dispatch case in `Call()`

```go
case "uptime":
    var a UptimeArgs
    if a, err = jsonArgs[UptimeArgs](args); err == nil {
        result, err = t.Uptime(a)
    }
```

If the tool takes no arguments, you can skip `jsonArgs`:

```go
case "uptime":
    result, err = t.Uptime(UptimeArgs{})
```

### 2b. Add the definition in `List()`

```go
{
    Name:        "uptime",
    Description: "Report how long the machine has been running.",
    InputSchema: map[string]any{
        "type": "object",
        "properties": map[string]any{},
    },
},
```

`InputSchema` is a standard JSON Schema — describe your arguments so the LLM
client knows how to call the tool.

## Step 3 — Test it

Add a case to `internal/tools/tools_test.go`:

```go
func TestUptime(t *testing.T) {
    tl := NewTools(nil)
    out, err := tl.Uptime(UptimeArgs{})
    if err != nil {
        t.Fatal(err)
    }
    if out == "" {
        t.Error("expected a duration string")
    }
}
```

Then:

```bash
go test ./internal/tools/
go build ./... && go vet ./...
```

## Tool checklist (before shipping)

- [ ] Uses `t.checkPath(...)` if it touches paths / runs commands.
- [ ] Destructive behavior is gated on `t.Policy` (or clearly documented).
- [ ] Has a `Description` an LLM can understand, and a correct `InputSchema`.
- [ ] Registered in **both** `Call()` and `List()`.
- [ ] Has a unit test.

## Ideas from the original inspiration list

| Tool | Notes |
|---|---|
| `office` (Excel/Word) | macOS: `osascript`/AppleScript; Windows: COM. |
| `clipboard` | `pbpaste`/`pbcopy` (macOS), `clip` (Windows). |
| `screenshot` | `screencapture` (macOS) / PowerShell (Windows). |
| `notification` | macOS `osascript -e 'display notification'`; Windows toast. |
| `scheduler` | `crontab` / Windows `schtasks`. |
| `web_fetch` | `http.Get` in Go — no shell needed. |
| `codex_run` | Shell out to a local `codex` CLI. |
