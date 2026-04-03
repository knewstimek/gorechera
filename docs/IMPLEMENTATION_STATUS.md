# Gorchera Implementation Status

Last verified: 2026-04-03

## Build And Test

```bash
go build ./...   # PASS
go test ./...    # PASS
```

## Implemented

### Core orchestration

- Bounded orchestrator loop with persisted job state, step state, ordered events, and atomic JSON storage.
- Provider-backed planner and evaluator phases, plus persisted planning artifacts and verification contracts.
- Target pipeline redesign is in flight: control-plane surfaces now expose `pipeline_mode`, bounded resume `extra_steps`, and terminal-state notifications needed for the director/executor/[engine build+test]/reviewer/evaluator model.
- Evaluator-gated completion with strictness levels:
  - `strict`: requires succeeded `implement`, `review`, and `test`
  - `normal`: requires succeeded `implement`; verification can be satisfied by succeeded `test`, `build`, or `command`
  - `lenient`: can accept provider pass with minimal structural coverage
  - `auto`: defers level selection to the planner phase; planner's `recommended_strictness` and `recommended_max_steps` are adopted before the sprint contract is built; falls back to `normal` if recommendation is absent or unrecognised
- Evaluator rubric scoring: `VerificationContract.RubricAxes` defines per-axis thresholds (`name`, `weight`, `min_threshold`). The provider evaluator returns `RubricScores` (0.0-1.0 per axis). `mergeEvaluatorReport()` enforces thresholds additively -- rubric can only demote a passing report, never promote a failing one.
- Planner prompt includes role profiles (provider/model per role) to inform `recommended_strictness` and `recommended_max_steps`; chain context section injected when `job.ChainContext` is present.
- Jobs and chain goals now carry `ambition_level` (`low | medium | high`); omitted or unrecognized input defaults to `medium`.
- Role-specific worker prompts are differentiated:
  - executor: implementation-focused with ambition-aware autonomy guidance (`low` = fix only, `medium` = allow directly related improvements, `high` = allow justified structural expansion)
  - reviewer: adversarial review focused on counterexamples, regressions, lifecycle/restart/retry/recovery/idempotency issues, and state-transition safety
  - audit: routed through the reviewer role with the same adversarial prompt family, but constrained to risk discovery and contract validation
  - tester: verification-focused with executable evidence preferred over narrative claims
- Evaluator prompt is gate-oriented, ambition-aware, and no longer treats a single succeeded implement step as sufficient evidence by itself.
- Leader prompt includes a conditional high-risk review/audit trigger before completion for lifecycle/concurrency/deduplication/external-pricing/auth/UI-event-boundary changes.
- Leader summarize throttling: after two consecutive summarize turns, the service forces completion evaluation instead of allowing endless summary churn.
- Repeated blocked-reason protection: the same blocked reason three times in a row escalates to job failure.
- Startup recovery hardening:
  - `runLoop()` is single-flight per job ID within a process, so duplicate `Resume()` / recovery attempts are suppressed.
  - `RecoverJobs()` now schedules recoverable jobs with bounded concurrency (`2`) instead of unbounded fan-out.
  - Startup recovery is disabled by default, even for `serve` / `mcp`.
  - Operators must opt in explicitly with `-recover` or `-recover-jobs job1,job2`.
  - `-recover-jobs` limits startup recovery to the selected recoverable job IDs.
  - Active jobs keep lightweight lease files under `.gorchera/leases/`.
  - `InterruptRecoverableJobs()` only blocks stale recoverable jobs whose lease heartbeat expired.
  - `Shutdown()` marks still-owned recoverable jobs as interrupted instead of leaving them in `waiting_*`.
  - `serve` / `mcp` run the stale-job sweep automatically before serving traffic; one-shot CLI commands stay read-only unless they are explicit control actions.
- Workspace isolation:
  - `workspace_mode=shared` keeps the requested workspace unchanged.
  - `workspace_mode=isolated` creates a detached git worktree at repository `HEAD`.
  - The job stores both `RequestedWorkspaceDir` and the actual isolated `WorkspaceDir`.
  - Promotion from isolated workspaces is manual for now; the detached worktree stays available for supervised diff review and merge.
  - Worktree notification: when an isolated worktree job reaches a terminal state, the `notifications/job_terminal` payload includes `workspace_mode`, `workspace_dir`, `requested_workspace_dir`, and `diff_stat` (output of `git diff --stat` between the worktree and the requested workspace HEAD).
- MCP stdio smoke coverage:
  - `cmd/mcp-smoke` runs isolated end-to-end MCP scenarios against a real `gorchera mcp` subprocess.
  - `basic` validates `initialize -> tools/list -> start_job -> status(wait=true)` using the mock provider.
  - `isolated` validates `workspace_mode=isolated` end-to-end and confirms the job runs inside a detached git worktree instead of the requested workspace.
  - `recovery` seeds recoverable jobs under an isolated `.gorchera` root, restarts `mcp`, and verifies startup recovery completes only the explicitly requested jobs without touching the main workspace state.
  - `interrupt` seeds stale recoverable jobs, starts `mcp` without recovery flags, and verifies startup interruption blocks them by default.
- Model-aware token/cost accounting:
  - token counts remain heuristic (`~4 chars/token`)
  - estimated cost now uses provider/model-specific input/output pricing instead of a single flat per-token constant
  - Claude aliases (`opus` / `sonnet` / `haiku`) map to current Anthropic pricing tiers
  - Codex aliases map to current GPT-5 pricing tiers for estimation

### Provider integration

- Real Claude CLI adapter:
  - `--permission-mode dontAsk`
  - `--output-format json`
  - `--json-schema`
  - stdin prompt delivery
  - role-specific model selection
  - tolerant JSON envelope extraction for `structured_output`, `parsed_output`, and object-valued `result`
- Real Codex CLI adapter:
  - `codex exec`
  - `--output-schema`
  - prefers `--ephemeral` for current Codex CLI builds and falls back to `--fresh` for legacy builds
  - workspace-write sandbox
  - stdin prompt delivery
  - role-specific GPT-family model selection
- Mock adapter remains available for end-to-end tests.
- Role-based provider resolution is implemented:
  - `job.RoleOverrides[role]` first
  - `job.RoleProfiles[role]` second
  - job provider third
  - `mock` fallback last
- MCP `gorchera_start_job` now accepts a structured `role_overrides` object with per-role `{provider, model}` overrides and persists it onto the started job as `map[string]RoleOverride`.
- MCP and HTTP job start surfaces now also accept `workspace_mode` (`shared` | `isolated`).
- `fallback_provider` is honored if the primary provider lookup fails.
- `fallback_model` is honored narrowly at runtime:
  - exactly one retry on the already-selected provider adapter
  - only after a provider command failure occurs before any structured response is produced
  - disabled when blank or equal to the primary model
  - does not replace `fallback_provider` lookup or retry invalid structured output

### Prompt overrides

- Workspace file overrides: `.gorchera/prompts/{role}.md` is loaded at job start for each role (director, executor, reviewer, evaluator). File content is prepended to the built-in base prompt. If the first line is `# REPLACE`, the base prompt is replaced entirely instead.
- Job parameter overrides: `gorchera_start_job` accepts `prompt_overrides` (map of role -> text). Always prepend; replace mode is not available via job parameters.
- Priority: job parameter > workspace file > default prompt. When both exist for a role, job parameter is prepended first.

### Schema retry and pre_build_commands

- Schema retry: director, executor, and evaluator roles retry up to 2 additional times when the provider returns a response that fails schema validation. After 3 total failures the step is marked failed with `schema` classification.
- pre_build_commands: `gorchera_start_job` (and the HTTP/CLI equivalents) accept a `pre_build_commands` string list. The engine runs these commands sequentially before invoking `go build`/`go test`, enabling language-agnostic setup (e.g. `go mod tidy`, `npm install`, `pip install -r requirements.txt`). Failures abort the engine phase and are reported as a `build` step failure.

### Structured provider errors and retry behavior

- Structured provider error kinds now include:
  - `missing_executable`
  - `probe_failed`
  - `command_failed`
  - `invalid_response`
  - `unsupported_phase`
  - `auth_failure`
  - `quota_exceeded`
  - `rate_limited`
  - `billing_required`
  - `session_expired`
  - `network_error`
  - `transport_error`
- Recommended action mapping is implemented:
  - retry: `rate_limited`, `network_error`
  - block: `auth_failure`, `billing_required`, `session_expired`
  - fail: everything else
- Provider retries use 3 attempts with exponential backoff starting at 250 ms.
- Worker failure classification persists structured step reasons for timeout, schema, file access, test, and build failures.

### Chain system

- Persisted `JobChain` state with sequential goal execution.
- Per-goal fields for provider, strictness level, ambition level, context mode, max steps, job ID, and goal status.
- Automatic next-goal start after evaluator-approved completion of the current goal.
- Chain result forwarding: completed job's `Summary` and `EvaluatorReportRef` are packaged as `ChainContext` and passed to the next goal's planner prompt.
- Per-goal `role_overrides`: each `ChainGoal` supports `map[string]RoleOverride` overrides; MCP `gorchera_start_chain` exposes these as per-goal `role_overrides` objects with `{provider, model}` values.
- Terminal propagation from blocked/failed chained jobs to chain failure.
- Chain controls are implemented:
  - pause
  - resume
  - cancel
  - skip current goal
- Chain-specific statuses are implemented:
  - chain: `running`, `paused`, `done`, `failed`, `cancelled`
  - goal: `pending`, `running`, `done`, `failed`, `skipped`

### Context shaping and steering

- Leader `context_mode` is implemented with `full`, `summary`, `minimal`, and `auto` payload shapes.
  - `auto` is passed through by `normalizeContextMode()` and resolved at runtime by `autoContextMode()` in the payload builder: step count < 10 = `full`, 10-20 = `summary`, > 20 = `minimal`.
- Context mode is normalized per job and per chain goal.
- Supervisor steering is implemented:
  - MCP tool `gorchera_steer`
  - service-level `Steer()`
  - highest-priority prompt section for the next leader turn
  - directive cleared after the leader call

### Workspace and execution safety

- Fail-fast workspace directory validation for job start, async job start, chain start, and MCP start tools.
- Absolute-path, existence, directory, and symlink-aware workspace checks are implemented.
- Approval policy blocks network access, credential access, deploy, git push, workspace-external writes/commands, and mass delete.
- Runtime `command` task type is now supported in `run_system`.
- Runtime command allowlists are enforced per category.

### Artifacts and diff visibility

- Planning, worker, and system artifacts are materialized atomically under `.gorchera/artifacts/<jobID>/`.
- Worker artifacts prefer real file content via `WorkerOutput.FileContents`.
- Successful single-worker steps collect `git diff --stat` into `Step.DiffSummary` when available.
- Parallel worker fan-out is implemented with `max_parallel_workers = 2` and disjoint target/write-scope checks.

### Harness and control plane

- Runtime harness global inventory and job-scoped ownership surfaces are implemented.
- TOCTOU ownership race is fixed through `claimHarness` / `releaseHarnessClaim` and `harnessInflight`.
- HTTP API is implemented for job and harness control.
- CLI is implemented for job lifecycle and harness lifecycle.
- MCP server is implemented with:
  - job lifecycle tools
  - chain lifecycle tools
  - `gorchera_start_job.pipeline_mode` (`light` | `balanced` | `full`, default `balanced`)
  - `gorchera_start_job.ambition_level`
  - `gorchera_start_job.role_overrides`
  - `gorchera_start_job.prompt_overrides` (per-role prepend; workspace file overrides also supported via `.gorchera/prompts/{role}.md`)
  - `gorchera_start_chain` per-goal `ambition_level`
  - `gorchera_start_chain` per-goal `role_overrides`
  - `gorchera_resume.extra_steps` with MCP-side bounds (`1..20`)
  - PendingApproval guard: `gorchera_resume` (and service-level `ResumeWithOptions`) rejects resume attempts on jobs that have a pending operator approval; callers must use `gorchera_approve` or `gorchera_reject` first
  - `wait=true` polling for job and chain status with configurable `wait_timeout` (default 30s, 0=5min maximum)
  - `gorchera_steer`
  - JSON-RPC terminal notifications via `notifications/job_terminal` with `{job_id, status, summary}`
  - buffered notification writes so terminal notifications can be queued before stdio is fully ready
  - terminal notification forwarding for cancellation paths that surface as `job_cancelled`, including interrupted chain-goal transitions

## In-Memory Job Cache And JobStatusPlanning (2026-04-02)

UX fix: status API now returns current in-memory state without disk round-trips between writes.

- `Service` carries a `sync.RWMutex` + `map[string]*domain.Job` jobCache field.
- `Service.Get()` checks the cache first; falls back to disk only on a cache miss.
- `addEvent`, `touch`, and `accumulateTokenUsage` write the updated job into the cache immediately after each mutation.
- `List()` overlays in-flight cache entries on top of the disk listing so callers see live state.
- Terminal states (`done`, `failed`, `blocked`) are evicted from the cache after the final write to bound memory usage.
- `domain.JobStatusPlanning` added: set during `ensurePlanning()` so the UI shows a distinct planning state instead of collapsing the planner phase into `starting`.
- `domain.CloneJob()` deep-copy helper added to protect the cache from aliasing when callers mutate returned values.

## Security Audit V2 Fixes (2026-04-02)

CRITICAL and HIGH findings from `docs/AUDIT_REPORT_V2.md` have been fixed. `go build ./...` and `go test ./...` pass with no regressions.

### XSS fixes (web/app.js)

- **XSS-1 (`showToast`):** Replaced `toast.innerHTML = \`<span>${message}</span>\`` with DOM `createElement`/`textContent` to eliminate the unescaped innerHTML sink.
- **XSS-2 (`makeBadge`):** Applied `esc()` to the `class` attribute interpolation (`badge-${status}`) in addition to the text content.

### Go backend fixes

- **H1 (`internal/orchestrator/planning.go`):** Changed `validatePlanningArtifact` parameter from value `domain.PlanningArtifact` to pointer `*domain.PlanningArtifact` so the acceptance-criteria fallback assignment is no longer a dead write.
- **H2 (`internal/api/server.go`):** Bearer token comparison switched from `string !=` to `crypto/subtle.ConstantTimeCompare` to eliminate the timing side-channel.
- **H3 (`internal/api/server.go`):** All `http.Error(w, err.Error(), ...)` call sites replaced with a generic `"internal error"` message; the original error is logged server-side via `log.Printf`.

## Security Audit Fixes (2026-04-02)

All 10 HIGH severity findings from `docs/AUDIT_REPORT.md` have been fixed. `go build ./...` and `go test ./...` pass with no regressions.

### Path traversal input validation (HIGH-01, HIGH-02, HIGH-03)

- **HIGH-01 (`internal/store/state_store.go`):** Added `validIDRegexp` (`^[a-zA-Z0-9_-.]+$`) and `validateID()`. Applied at entry of `SaveJob`, `LoadJob`, `SaveChain`, `LoadChain`. IDs like `../../etc/passwd` are now rejected with an error.
- **HIGH-02 (`internal/store/artifact_store.go`):** Applied `validateID(jobID)` at entry of all four `Materialize*` methods, reusing the shared helper.
- **HIGH-03 (`internal/api/views.go`):** Added `safeReadFile(root, path)` that resolves both paths via `filepath.Abs` and enforces `strings.HasPrefix` containment. All `os.ReadFile` call sites in `loadArtifactView`, `loadSprintContract`, `loadEvaluatorReport` now go through `safeReadFile`.
- Tests: `state_store_test.go` (ID validation), `views_test.go` (path containment).

### Data race fix (HIGH-04)

- **HIGH-04 (`internal/runtime/lifecycle.go`):** Moved `rec.stopRequested` read inside a dedicated `m.mu.Lock/Unlock` block immediately after `cmd.Wait()` in `watchProcess()`, eliminating the race with `Stop()`.
- Test: race-detector test in `lifecycle_test.go`.

### Environment variable leakage fix (HIGH-05, HIGH-06)

- **HIGH-05 (`internal/runtime/runner.go`):** Replaced `os.Environ()` with `minimalEnv()` in `Runner.Run()`. `minimalEnv()` allowlists PATH, SYSTEMROOT, HOME, TEMP, TMP, Windows profile dirs (LOCALAPPDATA, APPDATA, USERPROFILE), shell helpers (COMSPEC, PATHEXT), and Go toolchain vars (GOCACHE, GOPATH, GOROOT, GOPROXY). Secrets such as `ANTHROPIC_API_KEY` are excluded.
- **HIGH-06 (`internal/runtime/lifecycle.go`):** Applied same `minimalEnv()` in `ProcessManager.Start()`.
- Tests: `runner_test.go`, `lifecycle_test.go` verify `ANTHROPIC_API_KEY` is absent from subprocess env.

### HTTP server security (HIGH-07, HIGH-08)

- **HIGH-07 (`cmd/gorchera/main.go`, `internal/api/server.go`):** Default listen address changed from `:8080` to `127.0.0.1:8080`. Added `authMiddleware()` that reads `GORCHERA_AUTH_TOKEN` env var; if set, enforces `Authorization: Bearer <token>` header (returns 401 on mismatch); passes through when unset (dev mode). `Handler()` wraps the mux with `authMiddleware`.
- **HIGH-08 (`internal/api/server.go`):** All 5 `context.Background()` calls in handler functions (Resume, Approve, Retry, Reject, Cancel) replaced with `r.Context()`.
- Tests: `server_test.go` -- auth middleware accepts/rejects, r.Context() regression guard.

### Service shutdown context (HIGH-09)

- **HIGH-09 (`internal/orchestrator/service.go`):** Added `shutdownCtx`/`shutdownCancel` fields to `Service` struct. `NewService` creates the context via `context.WithCancel(context.Background())`. Added `Shutdown()` method that calls `shutdownCancel()`. `startPreparedJobAsync` now uses `s.shutdownCtx` instead of `context.Background()`. No existing callers broken.
- Test: `service_test.go` -- `TestServiceShutdownCancelsContext`.

### MCP error logging (HIGH-10)

- **HIGH-10 (`internal/mcp/server.go`):** Fire-and-forget goroutines in `toolApprove`, `toolRetry`, `toolResume` now capture the returned error and log it via `log.Printf("[gorchera] %s failed for job %s: %v", ...)`. `//nolint:errcheck` comments removed.
- Test: `mcp/server_test.go` -- `TestToolApproveLogsErrorOnFailure`.

---

## Partially Implemented Or Intentionally Limited

- Execution-profile fields `effort`, `tool_policy`, and `max_budget_usd` are stored in domain types but are not enforced by the orchestrator.
- `fallback_model` is intentionally narrow: one same-provider retry only, and only for pre-structured provider command failures.
- `pipeline_mode` is currently exposed at the MCP/control-plane layer; full orchestrator enforcement depends on the corresponding core director/engine changes.
- MCP `extra_steps` forwarding is wired to use the core resume-extension hook when available; until that hook is present in the orchestrator layer, non-zero `extra_steps` requests are rejected instead of being silently ignored.
- HTTP `POST /jobs` accepts role profiles and max steps, but it does not expose `strictness_level` or `context_mode`.
- CLI `run` exposes `strictness` but does not expose `context_mode`.
- Chain lifecycle is exposed through MCP and service methods, but not through CLI or HTTP routes.
- Isolated worktree mode currently applies to single-job starts only; chain goals still run in the shared workspace passed to `StartChain()`.
- `Step.DiffSummary` is only populated for successful single-worker steps. Parallel worker steps do not currently collect a diff summary.

## Not Implemented

### Product/control-surface gaps

- No HTTP chain endpoints.
- No CLI chain start/status/control commands.
- No Web UI.
- No persisted global role-profile registry; role profiles are still job-scoped input.

### Spec gaps that remain

- No milestone-based leader session reset or provider-specific context strategy orchestration beyond the prompt payload modes.
- `SprintContract.ThresholdMinSteps` is still generated but not used as an independent evaluator gate.
- No artifact merge-rule validation beyond disjoint-scope parallel planning checks.
- No browser evaluator lifecycle, dev-server readiness orchestration, or restart policy.
- No operator identity audit trail on control-plane actions; events record the action but not who performed it.

## Current Risk Notes

- The workspace is already dirty outside this docs task. Pre-existing Go file modifications are present in the repository, so verification must distinguish this task's changes from unrelated local edits.
- Provider fallback is only partially model-aware. `fallback_model` now covers one same-provider retry for pre-structured command failures, but it does not recover invalid structured output or support multi-hop fallback chains.
- Token counts are still heuristic-only, so totals remain approximate even though pricing is now model-aware.
- The prompt contract still tells workers to use shell commands for file creation, but repository editing policy for this project is enforced outside the runtime prompt by the orchestrator workflow and code review, not by a separate worker sandbox contract inside Go code.
