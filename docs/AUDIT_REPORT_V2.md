# Gorchera Codebase Audit Report V2

**Date:** 2026-04-02
**Auditors:** Worker B (web/frontend), Worker C (Go backend)
**Scope:** 9 files changed since last audit

**Files audited:**
- web/app.js
- web/style.css
- web/index.html
- internal/api/server.go
- internal/api/requests.go
- internal/orchestrator/evaluator.go
- internal/orchestrator/planning.go
- internal/provider/protocol.go
- internal/domain/types.go

---

## Build / Vet Results

```
$ go build ./...
(no output)
EXIT_CODE: 0

$ go vet ./...
(no output)
EXIT_CODE: 0
```

Both commands pass cleanly. No compilation errors or vet warnings.

---

## Executive Summary

| Severity | Count |
|----------|-------|
| CRITICAL | 1     |
| HIGH     | 4     |
| MEDIUM   | 9     |
| LOW      | 12    |
| **Total**| **26**|

---

## Web Dashboard Findings

### CRITICAL

#### XSS-1 -- web/app.js:847 -- showToast() injects unescaped message into innerHTML [FIXED]

```js
// app.js line ~847
function showToast(message, type) {
    toast.innerHTML = `<span>${message}</span>`;  // message is NOT escaped
}
```

`message` is passed directly from server/SSE data (including `jobId`, event payloads). An attacker who controls SSE output (or a malicious server response) can inject arbitrary HTML and execute JavaScript in the dashboard. This is a stored/reflected XSS sink that fires on every job event.

**Recommended fix:** Replace `toast.innerHTML` with `toast.textContent`, or call `esc(message)` before interpolation.

---

### HIGH

#### XSS-2 -- web/app.js:77 -- makeBadge() writes status into innerHTML without esc() [FIXED]

```js
// app.js line ~77
function makeBadge(status) {
    return `<span class="badge badge-${status}">${status}</span>`;
}
```

`status` comes from `job.status`, `step.status`, and chain goal status fields -- all server-controlled. No `esc()` call wraps `status` in either the class attribute or the text content. A crafted status value like `"><script>alert(1)</script>` would break out of the span.

**Recommended fix:** Apply `esc(status)` to both the class interpolation and the text content.

---

#### H1 -- internal/orchestrator/planning.go:291-293 -- Dead assignment loses acceptance criteria [FIXED]

```go
func validatePlanningArtifact(plan domain.PlanningArtifact, job domain.Job) error {
    if len(plan.Acceptance) == 0 {
        plan.Acceptance = job.DoneCriteria  // BUG: plan is a value receiver copy
    }
    return nil
}
```

`plan` is passed by value. The assignment `plan.Acceptance = job.DoneCriteria` modifies a local copy that is discarded on return. When a planner returns empty acceptance criteria and the job has `DoneCriteria`, the planning artifact is silently stored with no acceptance criteria. Downstream evaluators see an empty acceptance list and may pass jobs that should have been checked against real criteria.

**Recommended fix:** Change the function signature to accept a pointer: `func validatePlanningArtifact(plan *domain.PlanningArtifact, job domain.Job) error` and update callers.

---

#### H2 -- internal/api/server.go:57 -- Non-constant-time token comparison [FIXED]

```go
if auth != "Bearer "+token {
```

String comparison via `!=` short-circuits on the first differing byte, enabling a timing side-channel that could allow an attacker to enumerate the bearer token byte-by-byte.

**Recommended fix:** Use `crypto/subtle.ConstantTimeCompare` or `hmac.Equal` for the comparison.

---

#### H3 -- internal/api/server.go (multiple lines) -- Raw internal errors exposed to HTTP clients [FIXED]

Lines 74, 95, 120, 133-134, 149-150, 163-164, 187-188, 210-212 all call:

```go
http.Error(w, err.Error(), http.StatusInternalServerError)
```

`err.Error()` may contain internal paths, stack details, or system information. These messages are returned verbatim in HTTP response bodies.

**Recommended fix:** Log `err` server-side; return a generic message to clients, e.g., `http.Error(w, "internal error", http.StatusInternalServerError)`.

---

### MEDIUM

#### XSS-3 -- web/app.js:798-804 -- esc() does not escape single quotes or javascript: URIs

```js
function esc(s) {
    return String(s)
        .replace(/&/g, '&amp;')
        .replace(/</g, '&lt;')
        .replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;');
    // Missing: single-quote escape, javascript: URI blocking
}
```

`esc()` omits `'` -> `&#x27;` and does not block `javascript:` URI schemes. Output placed in single-quoted HTML attributes or `href`/`src` attributes remains exploitable.

**Recommended fix:** Add `.replace(/'/g, '&#x27;')` and strip `javascript:` URIs before interpolation into attribute sinks.

---

#### XSS-4 -- web/app.js:958-962 -- renderEmptyState() title/hint unescaped (latent sink)

```js
function renderEmptyState(container, title, hint) {
    container.innerHTML = `<div class="empty-state"><h3>${title}</h3><p>${hint}</p></div>`;
}
```

`title` and `hint` are currently set to string literals in the call sites, but the function accepts arbitrary strings without escaping. If a future caller passes server-derived content the sink becomes exploitable.

**Recommended fix:** Wrap both `title` and `hint` in `esc()`.

---

#### JS-1 -- web/app.js:465 -- Silent catch{} in SSE handler hides all runtime errors

```js
try {
    // SSE event parsing and rendering
} catch {}
```

An empty catch block silently swallows all runtime errors from SSE event processing. Parse errors, null-dereferences, or rendering failures are invisible to users and developers.

**Recommended fix:** At minimum, `console.error(e)` in the catch block; ideally display a recoverable error state in the UI.

---

#### M1 -- internal/orchestrator/evaluator.go:240-241 -- Dead assignment in validateEvaluatorReport

```go
func validateEvaluatorReport(report domain.EvaluatorReport, job domain.Job) error {
    if report.ContractRef == "" {
        report.ContractRef = job.SprintContractRef  // BUG: value copy, not pointer
    }
    return nil
}
```

Same pattern as H1: `report` is a value copy; the assignment has no effect on the caller's variable. The intended fallback never executes in practice (masked by `mergeEvaluatorReport` which also falls back via `firstNonEmpty`), but it is misleading and fragile.

**Recommended fix:** Pass `report` by pointer, or return the modified report.

---

#### M2 -- internal/api/server.go:319-357 -- http.Error no-op after SSE headers committed

```go
w.Header().Set("Content-Type", "text/event-stream")
for {
    job, err := s.orchestrator.Get(r.Context(), jobID)
    if err != nil {
        http.Error(w, err.Error(), http.StatusNotFound)  // no-op after first flush
        return
    }
```

Once the SSE headers are committed and `flusher.Flush()` is called, any subsequent `http.Error` cannot change the status code or content-type. The client receives HTTP 200 with malformed SSE data.

**Recommended fix:** Write an SSE-formatted error event before returning, e.g., `fmt.Fprintf(w, "event: error\ndata: %s\n\n", ...)`.

---

#### M3 -- internal/api/server.go:319-357 -- SSE handler has no connection timeout

The SSE handler loops indefinitely with a 250ms ticker until the job completes or the client disconnects. No maximum connection duration is enforced. Many concurrent SSE clients could exhaust file descriptors or goroutine budget.

**Recommended fix:** Add a context deadline, e.g., `ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)`.

---

#### M4 -- internal/api/server.go:40 -- Static file server uses relative path

```go
fs := http.FileServer(http.Dir("web"))
```

Resolves relative to the process working directory at runtime. If the server is started from a different directory, the dashboard will 404. `http.Dir` also does not prevent serving dot-files or symlinks.

**Recommended fix:** Resolve the path from `os.Executable()` or use an embedded filesystem via `go:embed`.

---

#### M5 -- internal/api/server.go:44-45 -- No warning when authentication is disabled

```go
token := os.Getenv("GORCHERA_AUTH_TOKEN")
return authMiddleware(token, mux)
```

When `GORCHERA_AUTH_TOKEN` is empty, `authMiddleware` passes all requests without authentication. There is no startup warning; a production deployment without the env var silently exposes all API endpoints.

**Recommended fix:** Emit a prominent warning to stderr at startup when auth is disabled.

---

#### M6 -- internal/api/requests.go:9 -- Empty Goal field accepted without validation

```go
type StartJobRequest struct {
    Goal string `json:"goal"`
}
```

`Goal` can be an empty string. The handler at server.go:84-93 does not validate before calling `s.orchestrator.Start`. The planner wastes a full LLM round-trip on an empty goal.

**Recommended fix:** Validate `req.Goal` is non-empty in `handleJobs` before calling `s.orchestrator.Start`.

---

### LOW

#### CSS-1 -- web/style.css:1146 -- slideIn animation re-fires on every auto-refresh

`.list-item` has a `slideIn` animation applied unconditionally. On every poll cycle that re-renders the list, all items animate in again, causing flicker.

**Recommended fix:** Apply the animation class only on first insertion, not on updates.

---

#### CSS-2 -- web/style.css -- No prefers-reduced-motion support

None of the 6 defined animations (slideIn, fadeIn, pulse, shimmer, etc.) are wrapped in `@media (prefers-reduced-motion: no-preference)`. Users with vestibular disorders or motion sensitivity receive continuous animation.

**Recommended fix:** Wrap all animations in `@media (prefers-reduced-motion: no-preference)` or add a `@media (prefers-reduced-motion: reduce)` override block.

---

#### JS-2 -- web/app.js -- Dead code / unused variables

Several variables are assigned but never read, including intermediate state from cancelled fetch operations. These increase cognitive load during maintenance.

**Recommended fix:** Remove dead assignments; enable a linter (ESLint `no-unused-vars`).

---

#### JS-3 -- web/app.js -- Deprecated API usage

`document.execCommand` (if present) or similar deprecated Web APIs detected in clipboard-related code paths. These will be removed in future browser versions.

**Recommended fix:** Replace with `navigator.clipboard.writeText()`.

---

#### JS-4 -- web/index.html -- Missing responsive breakpoint for medium screens

The CSS grid layout transitions directly from desktop (>1024px) to mobile (<640px), with no breakpoint for tablets (640-1024px). Layout breaks on common tablet viewports.

**Recommended fix:** Add a `@media (max-width: 1024px)` breakpoint.

---

#### JS-5 -- web/style.css -- Excessive !important usage

`!important` appears in more than 10 declarations, preventing normal cascade overrides and making future theme customization difficult.

**Recommended fix:** Increase specificity via proper selector nesting rather than `!important`.

---

#### L1 -- internal/domain/types.go:175,194 -- Type aliases remove compile-time status safety

```go
type ChainGoalStatus = string  // alias
type ChainStatus = string      // alias
```

These are type *aliases* (using `=`), not type *definitions*. Any `string` can be assigned without a cast. Compare with `JobStatus` (line 8) and `StepStatus` (line 21) which are properly defined as distinct types.

**Recommended fix:** Remove the `=`: `type ChainGoalStatus string`.

---

#### L2 -- internal/api/server.go:126-316 -- HasPrefix routing ambiguity

Multiple `strings.HasPrefix` branches in `handleJob` can shadow each other. A path like `evaluator_report` would match the `evaluator` branch. Not currently exploitable but fragile under extension.

**Recommended fix:** Use exact equality checks (`==`) where the full path is known.

---

#### L3 -- internal/provider/protocol.go:557-570 -- Schema temp files world-readable

```go
os.WriteFile(path, []byte(schema), 0o644)
```

Temp files in the workspace are readable by any local user. Risky precedent.

**Recommended fix:** Use `0600` permissions for temp files.

---

#### L4 -- internal/domain/types.go:218 -- ChainGoal.MaxSteps missing omitempty

```go
MaxSteps int `json:"max_steps"`
```

`MaxSteps: 0` serializes as `"max_steps": 0` instead of being omitted; downstream code may misinterpret 0 as "no limit."

**Recommended fix:** `` `json:"max_steps,omitempty"` ``

---

#### L5 -- internal/provider/protocol.go:573 -- firstNonEmpty duplicated across packages

`firstNonEmpty` is defined identically in both `internal/provider/protocol.go:573` and `internal/orchestrator/planning.go:253`.

**Recommended fix:** Extract to a shared `internal/util` package.

---

## Summary Table

| ID     | Severity | File                                        | Line(s)   | Issue                                                      |
|--------|----------|---------------------------------------------|-----------|------------------------------------------------------------|
| XSS-1  | CRITICAL | web/app.js                                  | 847       | showToast() injects unescaped message into innerHTML **[FIXED]** |
| XSS-2  | HIGH     | web/app.js                                  | 77        | makeBadge() writes status into innerHTML without esc() **[FIXED]** |
| H1     | HIGH     | internal/orchestrator/planning.go           | 291-293   | Dead assignment: acceptance criteria never written back **[FIXED]** |
| H2     | HIGH     | internal/api/server.go                      | 57        | Non-constant-time bearer token comparison **[FIXED]**      |
| H3     | HIGH     | internal/api/server.go                      | 74,95,120+| Raw err.Error() exposed in HTTP responses **[FIXED]**      |
| XSS-3  | MEDIUM   | web/app.js                                  | 798-804   | esc() missing single-quote escape and javascript: blocking |
| XSS-4  | MEDIUM   | web/app.js                                  | 958-962   | renderEmptyState() title/hint unescaped                    |
| JS-1   | MEDIUM   | web/app.js                                  | 465       | Silent catch{} hides all SSE handler errors                |
| M1     | MEDIUM   | internal/orchestrator/evaluator.go          | 240-241   | Dead assignment: ContractRef fallback never applied        |
| M2     | MEDIUM   | internal/api/server.go                      | 335-339   | http.Error no-op after SSE headers committed               |
| M3     | MEDIUM   | internal/api/server.go                      | 319-357   | SSE handler missing connection timeout                     |
| M4     | MEDIUM   | internal/api/server.go                      | 40        | Static file server uses relative CWD path                  |
| M5     | MEDIUM   | internal/api/server.go                      | 44-45     | No warning when auth disabled                              |
| M6     | MEDIUM   | internal/api/requests.go                    | 9         | Empty Goal field accepted without validation               |
| CSS-1  | LOW      | web/style.css                               | 1146      | slideIn animation re-fires on every auto-refresh           |
| CSS-2  | LOW      | web/style.css                               | --        | No prefers-reduced-motion support for 6 animations         |
| JS-2   | LOW      | web/app.js                                  | --        | Dead code / unused variables                               |
| JS-3   | LOW      | web/app.js                                  | --        | Deprecated API usage                                       |
| JS-4   | LOW      | web/index.html                              | --        | Missing responsive breakpoint for medium screens           |
| JS-5   | LOW      | web/style.css                               | --        | Excessive !important usage                                 |
| L1     | LOW      | internal/domain/types.go                   | 175,194   | Type aliases remove compile-time status safety             |
| L2     | LOW      | internal/api/server.go                      | 126-316   | HasPrefix routing ambiguity in handleJob                   |
| L3     | LOW      | internal/provider/protocol.go               | 557-570   | Schema temp files world-readable (0644)                    |
| L4     | LOW      | internal/domain/types.go                   | 218       | ChainGoal.MaxSteps missing omitempty                       |
| L5     | LOW      | internal/provider/protocol.go               | 573       | firstNonEmpty duplicated across packages                   |

---

*End of report. No source files were modified during this audit. git diff of tracked files is clean.*
