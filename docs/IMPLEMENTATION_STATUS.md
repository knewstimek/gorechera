# Gorchera Implementation Status

Last verified: 2026-04-02

## Build And Test

```bash
go build ./...   # PASS
go test ./...    # PASS
```

## Implemented

### Core orchestration

- Bounded orchestrator loop with persisted job state, step state, ordered events, and atomic JSON storage.
- Provider-backed planner and evaluator phases, plus persisted planning artifacts and verification contracts.
- Evaluator-gated completion with strictness levels:
  - `strict`: requires succeeded `implement`, `review`, and `test`
  - `normal`: requires succeeded `implement`; verification can be satisfied by succeeded `test`, `build`, or `command`
  - `lenient`: can accept provider pass with minimal structural coverage
- Leader summarize throttling: after two consecutive summarize turns, the service forces completion evaluation instead of allowing endless summary churn.
- Repeated blocked-reason protection: the same blocked reason three times in a row escalates to job failure.
- Rough token/cost accounting using serialized input/output heuristics.

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
  - workspace-write sandbox
  - stdin prompt delivery
  - role-specific GPT-family model selection
- Mock adapter remains available for end-to-end tests.
- Role-based provider resolution is implemented:
  - `job.RoleOverrides[role]` first
  - `job.RoleProfiles[role]` second
  - job provider third
  - `mock` fallback last
- MCP `gorchera_start_job` now accepts a structured `role_overrides` object with per-role `provider` / `model` overrides and persists it onto the started job.
- `fallback_provider` is honored if the primary provider lookup fails.

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
- Per-goal fields for provider, strictness level, context mode, max steps, job ID, and goal status.
- Automatic next-goal start after evaluator-approved completion of the current goal.
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

- Leader `context_mode` is implemented with `full`, `summary`, and `minimal` payload shapes.
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
  - `gorchera_start_job.role_overrides`
  - `wait=true` polling for job and chain status
  - `gorchera_steer`

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

- Execution-profile fields `effort`, `tool_policy`, `fallback_model`, and `max_budget_usd` are stored in domain types but are not enforced by the orchestrator. The currently active fields are `provider`, `model`, and `fallback_provider`.
- HTTP `POST /jobs` accepts role profiles and max steps, but it does not expose `strictness_level` or `context_mode`.
- CLI `run` exposes `strictness` but does not expose `context_mode`.
- Chain lifecycle is exposed through MCP and service methods, but not through CLI or HTTP routes.
- `Step.DiffSummary` is only populated for successful single-worker steps. Parallel worker steps do not currently collect a diff summary.

## Not Implemented

### Product/control-surface gaps

- No HTTP chain endpoints.
- No CLI chain start/status/control commands.
- No Web UI.
- No persisted global role-profile registry; role profiles are still job-scoped input.

### Spec gaps that remain

- No milestone-based leader session reset or provider-specific context strategy orchestration beyond the prompt payload modes.
- Evaluator scoring is still heuristic and step-coverage-based; full multi-axis scoring from the spec is not implemented.
- `SprintContract.ThresholdMinSteps` is still generated but not used as an independent evaluator gate.
- No artifact merge-rule validation beyond disjoint-scope parallel planning checks.
- No browser evaluator lifecycle, dev-server readiness orchestration, or restart policy.
- No operator identity audit trail on control-plane actions; events record the action but not who performed it.

## Current Risk Notes

- The workspace is already dirty outside this docs task. Pre-existing Go file modifications are present in the repository, so verification must distinguish this task's docs-only edits from unrelated local changes.
- Provider fallback is provider-aware but not model-aware. `fallback_model` is currently configuration-only.
- The prompt contract still tells workers to use shell commands for file creation, but repository editing policy for this project is enforced outside the runtime prompt by the orchestrator workflow and code review, not by a separate worker sandbox contract inside Go code.
