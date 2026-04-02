# Product Spec

Goal: Fix the four high-severity audit findings in orchestration and workspace validation logic without adding features.

## Scope
- Sanitize untrusted worker/system summaries and block reasons before assigning `job.LeaderContextSummary`.
- Add `sanitizeLeaderContext(text string) string` and apply it in `runWorkerStep` and `runSystemStep`.
- Strengthen workspace validation to require absolute paths and resolve symlinks before directory checks.
- Unify duplicated workspace validation into a shared helper if it can be done without introducing an import cycle.
- Restrict `Steer` to jobs in `running`, `waiting_leader`, or `waiting_worker` status.
- Add `SupervisorDirective` to `domain.Job`, render it separately in the leader prompt before job state, and clear it after the leader turn consumes it.
- Add or update focused tests covering the four fixes.
- Run `go build ./...` and `go test ./...` for verification.
- Update directly affected docs only if they describe the changed steering/workspace behavior.

## Non-Goals
- Do not add new product features or change workflow semantics beyond the four listed fixes.
- Do not redesign prompt formats beyond the new isolated supervisor-directive section.
- Do not broaden sanitization beyond removing lines that start with `[SUPERVISOR]` from untrusted leader-context inputs.
- Do not refactor unrelated orchestrator, MCP, or provider code.
- Do not change evaluator-gate, approval, harness, or worker-spawn principles.
- Do not add Windows-only logic to core validation behavior.

## Proposed Steps
- Inspect current implementations of `runWorkerStep`, `runSystemStep`, `Steer`, leader-response handling in the run loop, both `validateWorkspaceDir` copies, `domain.Job`, and `buildLeaderPrompt` to identify exact write points and any import-cycle risk for shared validation.
- Implement `sanitizeLeaderContext(text string) string` in orchestrator code and route every untrusted worker/system summary or block-reason assignment to `job.LeaderContextSummary` through that helper, leaving trusted supervisor-authored text out of this path.
- Create a shared workspace validation helper, preferably in `internal/orchestrator/workspace.go`, that rejects non-absolute paths with `filepath.IsAbs`, resolves symlinks with `filepath.EvalSymlinks`, then performs the existing directory/stat checks; switch both orchestrator and MCP call sites to use it.
- Update `Steer` so it returns an error unless the job is in `running`, `waiting_leader`, or `waiting_worker`, and stop writing directives into `LeaderContextSummary`.
- Add `SupervisorDirective string` to `domain.Job`, have `Steer` write directives there, update `buildLeaderPrompt` to render that directive as its own section before job state when non-empty, and clear the field immediately after the next leader response is received/consumed in the run loop.
- Add or update unit tests for sanitization, workspace validation, steer authorization, supervisor-directive prompt rendering, and directive clearing; then run `go build ./...` and `go test ./...`, fixing any regressions and updating only directly impacted docs if needed.

## Acceptance
