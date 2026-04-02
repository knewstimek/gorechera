# Gorchera Codebase Audit Report

**Date:** 2026-04-02
**Auditors:** Executor workers B (GROUP 1) and C (GROUP 2) -- read-only analysis
**Scope:** All Go source files in cmd/gorchera/, internal/api/, internal/domain/, internal/mcp/, internal/orchestrator/, internal/policy/, internal/provider/, internal/runtime/, internal/schema/, internal/store/

---

## Executive Summary

A full read-only audit of the Gorchera codebase was conducted across 10 packages (39 Go source files). No Go source files were modified during this audit.

**Total findings: 36**

| Severity | Count |
|----------|-------|
| Critical | 0 |
| High | 10 |
| Medium | 15 |
| Low | 11 |

**Overall risk assessment:** MEDIUM-HIGH. The codebase is generally well-structured with good error handling in many paths and a sound TOCTOU harness-ownership fix. However, two classes of high-severity issues require immediate attention: (1) path traversal vulnerabilities in the store layer that allow arbitrary file system read/write via unsanitized job/chain IDs, and (2) environment variable leakage where all subprocess executions inherit the full parent process environment including secrets such as API keys. A data race on `rec.stopRequested` in the runtime layer is a confirmed race condition. The HTTP API has no authentication or authorization, which compounds all other issues if the service is reachable beyond localhost.

**Packages audited:**
- `cmd/gorchera/` -- CLI entrypoint
- `internal/api/` -- HTTP API server, harness endpoints, views
- `internal/domain/` -- domain types and constants
- `internal/mcp/` -- MCP stdio server
- `internal/orchestrator/` -- core orchestration loop, evaluator, planner, parallel worker
- `internal/policy/` -- policy evaluation
- `internal/provider/` -- Claude and Codex adapters, prompt protocol
- `internal/runtime/` -- subprocess runner, process manager, lifecycle
- `internal/schema/` -- JSON schema validation
- `internal/store/` -- state store, artifact store

---

## Table of Contents

1. [High Severity Findings](#high-severity-findings) (HIGH-01 through HIGH-10)
2. [Medium Severity Findings](#medium-severity-findings) (MED-01 through MED-15)
3. [Low Severity Findings](#low-severity-findings) (LOW-01 through LOW-11)
4. [Confirmed Clean Areas](#confirmed-clean-areas)
5. [Summary Statistics](#summary-statistics)

---

## High Severity Findings

---

### HIGH-01: Path traversal via jobID/chainID in StateStore **[RESOLVED]**

- **Severity:** high
- **File:** `internal/store/state_store.go`
- **Lines:** 27, 36, 47, 59, 133-138
- **Description:** Job and chain IDs are used directly in file path construction via
  `filepath.Join(s.jobsDir(), fmt.Sprintf("%s.json", jobID))`. `filepath.Join` normalizes `..`
  sequences, so a jobID of `../../etc/cron.d/evil` would produce a path like
  `/root/.gorchera/../../etc/cron.d/evil.json`. This allows a caller to write or overwrite
  arbitrary files on the filesystem. All four state-mutating methods (`SaveJob`, `SaveChain`,
  `LoadJob`, `LoadChain`) are affected.
- **Suggested fix:** Validate that `jobID` and `chainID` contain only safe characters before
  constructing paths. A simple allowlist: `regexp.MustCompile("^[a-zA-Z0-9_\\-.]+$")`. Return an
  error if the ID does not match.
- **Resolution:** Added `validIDRegexp` (`^[a-zA-Z0-9_-.]+$`) and `validateID()` in `state_store.go`. Applied at entry of `SaveJob`, `LoadJob`, `SaveChain`, `LoadChain`. Tests added in `state_store_test.go`.

---

### HIGH-02: Path traversal via jobID in ArtifactStore **[RESOLVED]**

- **Severity:** high
- **File:** `internal/store/artifact_store.go`
- **Lines:** 23, 54, 66, 82
- **Description:** Same path traversal via `jobID` as HIGH-01. `MaterializeWorkerArtifacts`,
  `MaterializeTextArtifact`, `MaterializeJSONArtifact`, and `MaterializeSystemResult` all construct
  `baseDir = filepath.Join(s.root, jobID)` without sanitizing `jobID`. An attacker-controlled jobID
  can escape the artifact root and read or write arbitrary files.
- **Suggested fix:** Same as HIGH-01 -- validate `jobID` against an allowlist pattern before use
  in path construction.
- **Resolution:** Applied `validateID(jobID)` at the entry of all four materialize methods in `artifact_store.go`, reusing the shared `validateID` from `state_store.go`.

---

### HIGH-03: Path traversal via job artifact ref paths in views.go **[RESOLVED]**

- **Severity:** high
- **File:** `internal/api/views.go`
- **Lines:** 126, 196, 210, 281
- **Description:** `os.ReadFile()` is called with paths that originate from job data fields
  (`job.EvaluatorReportRef`, `job.SprintContractRef`, planning artifact paths). These paths are
  stored from user-supplied input at job creation time. If a malicious caller provides a path like
  `../../etc/passwd` or an absolute path outside the workspace, the server will read and return
  that file's contents in the API response. This is a path traversal / information disclosure
  vulnerability.
- **Suggested fix:** Before calling `os.ReadFile(path)` in `BuildEvaluatorView`,
  `loadSprintContract`, `loadEvaluatorReport`, and `loadArtifactView`, validate that `path` is
  within the expected artifacts root directory. Use `filepath.Clean(path)` and check that the
  cleaned path has the expected prefix: `strings.HasPrefix(cleaned, allowedRoot)`.
- **Resolution:** Added `safeReadFile(root, path)` helper in `views.go` that resolves both paths via `filepath.Abs` and enforces `strings.HasPrefix` containment check. All `os.ReadFile` call sites in `loadArtifactView`, `loadSprintContract`, `loadEvaluatorReport` replaced with `safeReadFile`. Tests added in `views_test.go`.

---

### HIGH-04: Data race on rec.stopRequested in lifecycle.go **[RESOLVED]**

- **Severity:** high
- **File:** `internal/runtime/lifecycle.go`
- **Line:** 243
- **Description:** Data race on `rec.stopRequested`. The field is written by `Stop()` at line 123
  while holding `m.mu`, but `watchProcess()` reads it at line 243 before acquiring `m.mu` (which
  is acquired only at line 249). Under Go's memory model, this constitutes a data race -- the read
  at line 243 is not synchronized with the write at line 123.
- **Suggested fix:** Move the `rec.stopRequested` read inside the mutex-guarded block:
  ```go
  waitErr := cmd.Wait()
  finishedAt := time.Now().UTC()
  exitCode := exitCodeFromError(waitErr)
  m.mu.Lock()
  stopRequested := rec.stopRequested
  m.mu.Unlock()
  state := ProcessStateExited
  if stopRequested {
  ```
- **Resolution:** Moved `rec.stopRequested` read inside a dedicated `m.mu.Lock/Unlock` block immediately after `cmd.Wait()` in `watchProcess()`, eliminating the race with `Stop()`. Race-detector test added in `lifecycle_test.go`.

---

### HIGH-05: Full os.Environ() inherited by runner subprocesses **[RESOLVED]**

- **Severity:** high
- **File:** `internal/runtime/runner.go`
- **Line:** 54
- **Description:** `cmd.Env = append(os.Environ(), r.Environment...)` passes the full parent
  process environment to every subprocess. This means all environment variables present in the
  orchestrator process (API keys, database passwords, tokens, `ANTHROPIC_API_KEY`, etc.) are
  inherited by every executed command. A compromised or malicious command could read these values
  via its own environment.
- **Suggested fix:** Build a minimal environment rather than inheriting `os.Environ()`. Pass only
  the variables explicitly required (PATH at minimum). If full env inheritance is necessary,
  document it explicitly and ensure callers understand the security implication.
- **Resolution:** Replaced `os.Environ()` with `minimalEnv()` in `runner.go`. `minimalEnv()` allowlists PATH, SYSTEMROOT, HOME, TEMP, TMP, Windows profile dirs, shell helpers, and Go toolchain vars. All other env vars (including `ANTHROPIC_API_KEY`) are excluded. Tests added in `runner_test.go`.

---

### HIGH-06: Full os.Environ() inherited by ProcessManager subprocesses **[RESOLVED]**

- **Severity:** high
- **File:** `internal/runtime/lifecycle.go`
- **Line:** 78
- **Description:** Same environment inheritance issue as HIGH-05. `ProcessManager.Start()` also
  uses `cmd.Env = append(os.Environ(), m.Environment...)`, exposing all process environment
  variables to long-running child processes started via the harness.
- **Suggested fix:** Same as HIGH-05.
- **Resolution:** Replaced `os.Environ()` with `minimalEnv()` in `ProcessManager.Start()` in `lifecycle.go`, reusing the same helper from HIGH-05. Tests added in `lifecycle_test.go`.

---

### HIGH-07: No authentication or authorization on any HTTP endpoint **[RESOLVED]**

- **Severity:** high
- **File:** `internal/api/server.go`
- **Lines:** 17-33
- **Description:** The HTTP server exposes all endpoints (job management, harness control, event
  streaming) without any authentication or authorization mechanism. Any client with network access
  can start jobs, approve/reject actions, cancel jobs, and control harness processes. This is a
  critical attack surface if the service is reachable beyond localhost.
- **Suggested fix:** Add token-based authentication middleware (e.g., a shared secret in
  `Authorization: Bearer` header checked in a wrapper around `http.Handler`). At minimum, bind the
  listener to `127.0.0.1` only in the server startup configuration.
- **Resolution:** Default listen address changed from `:8080` to `127.0.0.1:8080` in `main.go`. Added `authMiddleware()` in `server.go` that reads `GORCHERA_AUTH_TOKEN`; enforces `Authorization: Bearer <token>` returning 401 on mismatch; passes through in dev mode (env var unset). `Handler()` wraps the mux with `authMiddleware`. Tests added in `server_test.go`.

---

### HIGH-08: context.Background() instead of r.Context() in 5 HTTP handlers **[RESOLVED]**

- **Severity:** high
- **File:** `internal/api/server.go`
- **Lines:** 102, 117, 129, 155, 181
- **Description:** Multiple handler branches pass `context.Background()` to orchestrator calls
  (Resume, Approve, Retry, Reject, Cancel) instead of `r.Context()`. If the HTTP client
  disconnects or the request times out, these operations cannot be cancelled and will continue
  running indefinitely, wasting resources and potentially causing inconsistent state.
- **Suggested fix:** Replace `context.Background()` with `r.Context()` on all five call sites:
  `s.orchestrator.Resume(r.Context(), ...)`, `s.orchestrator.Approve(r.Context(), ...)`,
  `s.orchestrator.Retry(r.Context(), ...)`, `s.orchestrator.Reject(r.Context(), ...)`,
  `s.orchestrator.Cancel(r.Context(), ...)`.
- **Resolution:** All 5 `context.Background()` calls in handler functions replaced with `r.Context()` in `server.go`. Regression guard test added in `server_test.go`.

---

### HIGH-09: Context not propagated to async job goroutine **[RESOLVED]**

- **Severity:** high
- **File:** `internal/orchestrator/service.go`
- **Line:** 226
- **Description:** `startPreparedJobAsync` launches `runLoop` in a goroutine with
  `context.Background()` instead of the caller-provided `ctx`. If the caller cancels the context
  (e.g. an HTTP request times out or the MCP connection closes), the background job loop continues
  running indefinitely and cannot be interrupted via context cancellation.
- **Code:**
  ```go
  go func() {
      s.runLoop(context.Background(), job) //nolint:errcheck
  }()
  ```
- **Suggested fix:** Inject a service-level shutdown context (e.g. passed to `NewService`) and use
  that for all async goroutines. This allows the process to cleanly terminate background jobs on
  shutdown without leaking goroutines.
- **Resolution:** Added `shutdownCtx`/`shutdownCancel` fields to `Service` struct in `service.go`. `NewService` creates the context via `context.WithCancel`. Added `Shutdown()` method. `startPreparedJobAsync` now uses `s.shutdownCtx` instead of `context.Background()`. Test added in `service_test.go`.

---

### HIGH-10: Errors silently discarded in approve/retry/resume goroutines **[RESOLVED]**

- **Severity:** high
- **File:** `internal/mcp/server.go`
- **Lines:** 796, 830, 864
- **Description:** `toolApprove`, `toolRetry`, and `toolResume` all fire goroutines that call
  `service.Approve`, `service.Retry`, and `service.Resume` respectively, discarding their errors
  with `//nolint:errcheck`. The caller receives a stale pre-operation snapshot. If the service
  operation fails (e.g. state save error, chain handler error), the error is permanently lost and
  no notification reaches the MCP client.
- **Code (example):**
  ```go
  go func() {
      s.service.Approve(context.Background(), jobID) //nolint:errcheck
  }()
  ```
- **Suggested fix:** After the goroutine completes, emit an error event notification via
  `writeMessage` if an error occurred. At minimum, log errors to stderr so they appear in the
  host process log.
- **Resolution:** All three fire-and-forget goroutines in `toolApprove`, `toolRetry`, `toolResume` now capture the returned error and log it via `log.Printf("[gorchera] %s failed for job %s: %v", ...)`. `//nolint:errcheck` comments removed. Test added in `mcp/server_test.go`.

---

## Medium Severity Findings

---

### MED-01: TOCTOU race in writeAtomically Windows fallback

- **Severity:** medium
- **File:** `internal/store/state_store.go`
- **Lines:** 149-171
- **Description:** The Windows fallback path in `writeAtomically` checks `os.Stat(path)` at
  line 165, then calls `os.Remove(path)` at line 168, then `os.Rename(tmp, path)` at line 170.
  Between the stat and remove, another goroutine or process could create a different file at
  `path`, which then gets silently deleted. Between `os.Remove` and `os.Rename`, the tmp file
  could be deleted by another process, causing the rename to fail with a misleading error.
- **Suggested fix:** On Windows, use `MoveFileExW` with `MOVEFILE_REPLACE_EXISTING` flag via
  `golang.org/x/sys/windows` for atomic replace semantics. Alternatively, document the narrow
  race as acceptable given internal usage.

---

### MED-02: Single corrupt file fails entire ListJobs/ListChains

- **Severity:** medium
- **File:** `internal/store/state_store.go`
- **Lines:** 82-93 (ListJobs), 113-125 (ListChains)
- **Description:** If any single job or chain JSON file is unreadable or unparseable (corrupted,
  permission issue, partial write), `ListJobs` and `ListChains` fail entirely and return an error.
  This means one bad file makes the entire job list inaccessible, blocking the API and the MCP
  server.
- **Suggested fix:** Log or record a warning for individual file errors and continue iterating
  rather than returning immediately. Return a partial list plus an indication that some records
  were skipped. Example: use `continue` with an error counter instead of `return nil, err`.

---

### MED-03: JSON encode error ignored in writeJSON

- **Severity:** medium
- **File:** `internal/api/server.go`
- **Line:** 325
- **Description:** The return value of `json.NewEncoder(w).Encode(v)` is discarded with `_ = ...`.
  If JSON encoding fails (e.g., due to an unencodable value), the response body will be incomplete
  but `w.WriteHeader(status)` has already been called, so the status code cannot be changed. The
  client receives a success status with a malformed body.
- **Suggested fix:** Log the encoding error for diagnostics:
  `if err := json.NewEncoder(w).Encode(v); err != nil { log.Printf("writeJSON encode error: %v", err) }`.

---

### MED-04: logFile.Close() called inside mutex in watchProcess

- **Severity:** medium
- **File:** `internal/runtime/lifecycle.go`
- **Lines:** 257-259
- **Description:** `rec.logFile.Close()` is called inside the `m.mu.Lock()` section in
  `watchProcess`. File close can block on some filesystems (network mounts, flushing buffers).
  Holding the mutex while blocking on I/O prevents all other `ProcessManager` operations (Start,
  Stop, Status, List) from proceeding.
- **Suggested fix:** Capture the log file reference before locking, close it outside the lock:
  ```go
  logFile := rec.logFile
  m.mu.Lock()
  rec.handle.FinishedAt = finishedAt
  ...
  rec.logFile = nil
  close(rec.done)
  m.mu.Unlock()
  if logFile != nil {
      _ = logFile.Close()
  }
  ```

---

### MED-05: RequiredCommands populated with check descriptors instead of commands

- **Severity:** medium
- **File:** `internal/api/views.go`
- **Line:** 247
- **Description:** `deriveVerificationContract` sets `RequiredCommands` to the value of
  `requiredChecks` (strings like `"planner_artifacts_present"`,
  `"tester_executes_required_steps"`), not actual shell commands. The field name
  `RequiredCommands` implies executable commands, but it contains check descriptors. The
  verification contract schema validates that `RequiredCommands` is non-empty but does not
  validate format, so this silently produces semantically incorrect data.
- **Suggested fix:** Clarify the intent: either rename the field to `RequiredChecks` in the
  domain model, or populate `RequiredCommands` with actual commands and use a separate field
  for descriptive checks.

---

### MED-06: Windows-specific code in cross-platform core

- **Severity:** medium
- **File:** `internal/runtime/policy.go`
- **Line:** 74
- **Description:** `if runtime.GOOS == "windows"` is embedded in `normalizeExecutable`,
  introducing Windows-specific behavior into the core runtime policy. The project's stated
  principle is "Windows 전용 코드를 코어에 넣지 않음 -- cross-platform 중립". The behavior
  difference (stripping spaces from executable names on Windows) also means the allowlist check
  behaves differently per OS, creating potential security inconsistencies.
- **Suggested fix:** Remove the Windows-specific space-stripping logic, or move it to a
  build-tag-separated file. Executable names with spaces are not a normal pattern for the
  allowlisted tools; if needed, document a clear cross-platform normalization contract.

---

### MED-07: run_system task_type validated by exclusion list -- fragile

- **Severity:** medium
- **File:** `internal/schema/validate.go`
- **Line:** 108
- **Description:** `run_system` task type validation uses an exclusion list (checking that
  task_type is NOT `none`, `summarize`, `implement`, `review`, `test`) rather than an inclusion
  list. If new task types are added to `leaderTaskTypes`, they will silently be permitted for
  `run_system` without any deliberate decision. This is fragile defense-in-depth.
- **Suggested fix:** Invert to an allowlist: define `systemTaskTypes` containing only `"search"`,
  `"build"`, `"lint"`, `"command"` and check membership in that set.

---

### MED-08: loadSprintContract/loadEvaluatorReport silently ignore errors

- **Severity:** medium
- **File:** `internal/api/views.go`
- **Lines:** 196-204, 207-219
- **Description:** `loadSprintContract` and `loadEvaluatorReport` silently ignore both file-read
  errors and JSON parse errors, returning `nil`. The caller in `BuildVerificationView` treats
  `nil` as "not available" and proceeds to build a derived contract. If a file is corrupt or
  unreadable, the caller gets a degraded result without any indication that data was missing,
  potentially causing incorrect verification state.
- **Suggested fix:** Return `(T, error)` from these helpers so callers can distinguish "file not
  present" from "file corrupt/unreadable". Alternatively, include an error string in the view so
  API consumers can detect the degraded state.

---

### MED-09: Dead mutation in validatePlanningArtifact

- **Severity:** medium
- **File:** `internal/orchestrator/planning.go`
- **Line:** 263
- **Description:** `validatePlanningArtifact` receives `plan domain.PlanningArtifact` by value.
  At line 263 it does `plan.Acceptance = job.DoneCriteria` to fix up an empty acceptance list,
  but since `plan` is a local copy, this assignment has no effect on the caller. The caller's
  `PlanningArtifact` retains an empty `Acceptance` field.
- **Code:**
  ```go
  func validatePlanningArtifact(plan domain.PlanningArtifact, job domain.Job) error {
      ...
      if len(plan.Acceptance) == 0 {
          plan.Acceptance = job.DoneCriteria // has no effect -- local copy
      }
      return nil
  }
  ```
- **Suggested fix:** Either change the signature to `*domain.PlanningArtifact` and modify in
  place, or perform the fixup in the caller (`runPlannerPhase`) after
  `validatePlanningArtifact` returns successfully.

---

### MED-10: listenEvents goroutine leaks -- EventChan is never closed

- **Severity:** medium
- **File:** `internal/mcp/server.go`
- **Lines:** 60-77, 84
- **Description:** `listenEvents` runs `for event := range s.service.EventChan()`. The channel
  returned by `EventChan()` is created in `NewService` and never closed. The goroutine blocks
  forever waiting for new events once `Run()` exits (e.g. stdin EOF). This is a goroutine leak
  for the entire process lifetime.
- **Suggested fix:** Add a `shutdown chan struct{}` to `Service`. Close it in a `Stop()` method.
  In `listenEvents`, select on both the event channel and the shutdown signal:
  ```go
  for {
      select {
      case event, ok := <-s.service.EventChan():
          if !ok { return }
          s.writeMessage(...)
      case <-s.done:
          return
      }
  }
  ```

---

### MED-11: json.MarshalIndent errors silently ignored in prompt builders

- **Severity:** medium
- **File:** `internal/provider/protocol.go`
- **Lines:** 154, 184, 299, 352, 416, 421, 422
- **Description:** All prompt builder functions (`buildPlannerPrompt`, `buildEvaluatorPrompt`,
  `buildLeaderJobPayload`, `buildSummaryPayload`, `buildMinimalPayload`, `buildWorkerPrompt`)
  ignore `json.MarshalIndent` errors using the blank identifier `_`. If marshaling fails (e.g.
  due to an unmarshalable value), an empty or partial payload is silently inserted into the
  prompt sent to the AI provider, causing hard-to-diagnose provider failures.
- **Code (example):**
  ```go
  payload, _ := json.MarshalIndent(job, "", "  ")
  ```
- **Suggested fix:** Check the error and return it as a `ProviderError` from the calling
  function, or at minimum write a warning to stderr before continuing.

---

### MED-12: classifyScope has redundant condition

- **Severity:** medium
- **File:** `internal/orchestrator/service.go`
- **Line:** 1649
- **Description:** The condition
  `if rel == "." || (!strings.HasPrefix(rel, "..") && rel != "..")` contains a redundant
  subexpression. When `!strings.HasPrefix(rel, "..")` is true, `rel` does not start with `".."`
  so `rel != ".."` is always also true -- the second half of the AND is never false when the
  first half is true. This makes the logic harder to audit and could mask a future edit
  introducing a real bug.
- **Suggested fix:** Simplify to:
  ```go
  if rel == "." || !strings.HasPrefix(rel, "..") {
      return policy.ResourceWorkspaceLocal
  }
  ```

---

### MED-13: isTerminalJobStatus includes undocumented "cancelled" literal and treats blocked as terminal

- **Severity:** medium
- **File:** `internal/mcp/server.go`
- **Lines:** 681-686
- **Description:** `isTerminalJobStatus` checks for a string literal `"cancelled"` that does not
  correspond to any constant in `domain.JobStatus`. Additionally, `JobStatusBlocked` is treated
  as terminal, but blocked jobs can be resumed -- they are not truly final. This causes the
  `gorchera_status wait=true` poller to stop early when a job is merely blocked, even if the
  caller intended to wait for a definitive done/failed state.
- **Code:**
  ```go
  case string(domain.JobStatusDone), string(domain.JobStatusFailed), string(domain.JobStatusBlocked), "cancelled":
      return true
  ```
- **Suggested fix:** Clarify the intended polling semantics. If wait=true should return on any
  non-progressing state, document this. If it should only return on truly final states, remove
  `JobStatusBlocked` from the terminal set. Add a `JobStatusCancelled` constant if needed rather
  than using a string literal.

---

### MED-14: toolApprove returns stale pre-operation snapshot

- **Severity:** medium
- **File:** `internal/mcp/server.go`
- **Lines:** 791-803
- **Description:** `toolApprove` fetches the job with `service.Get` (capturing its current
  status), then fires a goroutine that calls `service.Approve`. The snapshot returned to the
  caller shows the pre-approval status. Between `Get` and the goroutine execution, the job may
  be further modified by concurrent calls. The returned snapshot is misleading without strong
  documentation.
- **Suggested fix:** Add a timestamp field to the response so callers can detect staleness:
  ```go
  "snapshot_time": time.Now().UTC().Format(time.RFC3339),
  ```

---

### MED-15: verificationSatisfiedNormal ignores the contract parameter

- **Severity:** medium
- **File:** `internal/orchestrator/evaluator.go`
- **Lines:** 293-299
- **Description:** `verificationSatisfiedNormal` accepts a `contract VerificationContract`
  parameter but immediately suppresses it with `_ = contract`. The function checks only for a
  hardcoded `[]string{"implement"}` required step. If a future contract specifies different
  required steps for normal mode, this function will silently ignore them.
- **Code:**
  ```go
  func verificationSatisfiedNormal(job domain.Job, contract VerificationContract) (bool, []string) {
      _ = contract
      missing := missingRequiredSteps(&job, []string{"implement"})
      ...
  }
  ```
- **Suggested fix:** Either use `contract.RequiredStepTypes` filtered to normal-mode relevant
  types, or rename the parameter to `_` at the function signature and add a comment explaining
  the design intent (normal mode always requires only "implement").

---

## Low Severity Findings

---

### LOW-01: run_workers hardcoded to exactly 2 tasks

- **Severity:** low
- **File:** `internal/schema/validate.go`
- **Line:** 84
- **Description:** `run_workers` is hardcoded to require exactly 2 tasks
  (`len(msg.Tasks) != 2`). This prevents the leader from ever dispatching 3 parallel workers
  even if the orchestrator supports it. This is a design constraint baked into the schema
  validator rather than being configurable.
- **Suggested fix:** Change to a range check (e.g., `len(msg.Tasks) < 2 || len(msg.Tasks) > maxParallelWorkers`) and derive the limit from a shared constant or configuration.

---

### LOW-02: Log filename collision possible under millisecond-level concurrency

- **Severity:** low
- **File:** `internal/runtime/lifecycle.go`
- **Line:** 271
- **Description:** `processLogName` appends `time.Now().UTC().Format("20060102-150405.000")` to
  create a unique filename. Under high concurrency (multiple processes started within the same
  millisecond), two log files could get the same name, causing one to silently overwrite the
  other because `os.Create` truncates existing files.
- **Suggested fix:** Include the PID or a monotonic counter in the filename after acquiring the
  process entry, or use `os.CreateTemp` to guarantee uniqueness.

---

### LOW-03: Global harness process endpoint unrestricted

- **Severity:** low
- **File:** `internal/api/harness.go`
- **Lines:** 54-91
- **Description:** `handleHarness` at the `/harness` route (not job-scoped) does not restrict
  which processes can be started -- there is no job scope or ownership check at this layer. Any
  caller can start a global harness process without associating it with a job. Combined with the
  lack of authentication (HIGH-07), this is an unrestricted process execution endpoint.
- **Suggested fix:** Document clearly that global harness endpoints are intended for operator use
  only, and consider requiring a separate elevated token for `/harness/processes POST`.

---

### LOW-04: Artifact names not length-limited

- **Severity:** low
- **File:** `internal/store/artifact_store.go`
- **Lines:** 99-105
- **Description:** `sanitizeArtifactName` uses `filepath.Base` to strip directory components
  from artifact names, which is correct. However, the function does not limit the length of the
  sanitized name. An attacker-controlled artifact name of thousands of characters would produce
  a very long filename that could fail on filesystems with 255-byte name limits, causing an
  unhandled error at the OS level.
- **Suggested fix:** Add a max-length truncation after the replacer, before returning:
  `if len(name) > 200 { name = name[:200] }`.

---

### LOW-05: HTTP API server has no read/write timeouts

- **Severity:** low
- **File:** `cmd/gorchera/main.go`
- **Line:** 535
- **Description:** `http.ListenAndServe(*addr, server.Handler())` uses no `http.Server` struct,
  so `ReadTimeout`, `WriteTimeout`, `IdleTimeout`, and `ReadHeaderTimeout` are all zero
  (unlimited). A slow or adversarial client can hold connections open indefinitely, exhausting
  file descriptors.
- **Suggested fix:**
  ```go
  srv := &http.Server{
      Addr:              *addr,
      Handler:           server.Handler(),
      ReadHeaderTimeout: 10 * time.Second,
      ReadTimeout:       30 * time.Second,
      WriteTimeout:      60 * time.Second,
      IdleTimeout:       120 * time.Second,
  }
  log.Fatal(srv.ListenAndServe())
  ```

---

### LOW-06: resolveAdapter is dead code

- **Severity:** low
- **File:** `internal/provider/provider.go`
- **Lines:** 113-115
- **Description:** `(*SessionManager).resolveAdapter` is defined but never called anywhere in
  the codebase. It is unreachable dead code that adds maintenance surface.
- **Code:**
  ```go
  func (m *SessionManager) resolveAdapter(job domain.Job, role domain.RoleName) (Adapter, error) {
      return m.adapterForProfile(m.resolveProfile(job, role))
  }
  ```
- **Suggested fix:** Remove the `resolveAdapter` method. If it was intended for tests, move it
  to a test file.

---

### LOW-07: newJobID / newChainID collision risk under concurrent creation

- **Severity:** low
- **File:** `internal/orchestrator/service.go`
- **Lines:** 1574-1579
- **Description:** IDs are generated as `"job-20060102-150405.000"` (millisecond precision). Two
  concurrent calls within the same millisecond produce the same ID. The state store will
  overwrite the first job with the second. Under load or parallel test execution this is
  reproducible.
- **Suggested fix:** Append a short random or atomic suffix:
  ```go
  func newJobID(now time.Time) string {
      return fmt.Sprintf("job-%s-%04x", now.Format("20060102-150405.000"), rand.Uint32()&0xFFFF)
  }
  ```

---

### LOW-08: outputPath built with string concatenation instead of filepath.Join

- **Severity:** low
- **File:** `internal/provider/codex.go`
- **Line:** 90
- **Description:** `outputPath` is constructed as
  `outputDir + string(os.PathSeparator) + "result.json"`. While this works on all current
  platforms, it is inconsistent with the rest of the codebase which uses `filepath.Join`.
- **Code:**
  ```go
  outputPath := outputDir + string(os.PathSeparator) + "result.json"
  ```
- **Suggested fix:** Use `filepath.Join(outputDir, "result.json")`.

---

### LOW-09: Scanner buffer size silently kills MCP server on oversized requests

- **Severity:** low
- **File:** `internal/mcp/server.go`
- **Line:** 87
- **Description:** The MCP stdin scanner is capped at 1 MB. A JSON-RPC request larger than 1 MB
  causes `scanner.Scan()` to return false with `bufio.ErrTooLong`, and `Run()` returns this
  error, terminating the entire MCP server process. For jobs with very large goal text or many
  accumulated steps, this limit may be hit unexpectedly.
- **Suggested fix:** Increase the buffer limit to a safer value (e.g. 64 MB), or handle
  `bufio.ErrTooLong` and return a JSON-RPC error for that specific request instead of killing
  the server.

---

### LOW-10: collectWorkspaceDiffSummary silently discards git stderr

- **Severity:** low
- **File:** `internal/orchestrator/service.go`
- **Lines:** 1668-1673
- **Description:** `collectWorkspaceDiffSummary` runs `git diff --stat` but does not capture
  stderr. If git produces an error (e.g. "not a git repository", permission denied), the error
  message is silently discarded and an empty string is returned. This makes debugging worker
  failures harder.
- **Suggested fix:** Capture stderr and include it in a debug log, or return a descriptive
  fallback string that indicates the diff could not be computed.

---

### LOW-11: profilePrompt is defined but never called

- **Severity:** low
- **File:** `internal/provider/protocol.go`
- **Line:** 455
- **Description:** `profilePrompt` formats role profile information as a string but is never
  called from anywhere in the codebase. It is dead code.
- **Code:**
  ```go
  func profilePrompt(role domain.RoleName, job domain.Job) string {
  ```
- **Suggested fix:** Remove `profilePrompt` or add a call to it where role profile context would
  be useful (e.g. in the evaluator or leader prompt).

---

## Confirmed Clean Areas

The following areas were audited and found to be correctly implemented:

- **TOCTOU harness ownership** (`internal/orchestrator/service.go:1520-1548`): `claimHarness` +
  `releaseHarnessClaim` correctly atomically check ownership and mark inflight within a single
  lock. Pattern is sound.
- **Path traversal in resolveSystemWorkdir** (`internal/orchestrator/service.go:1627-1633`):
  Uses `filepath.Clean` + `filepath.Join` correctly; relative paths are resolved under the
  workspace root.
- **Nil dereference guards**: `leader.SystemAction` checked before use at line 1583.
  `job.PendingApproval` checked before dereference at lines 342 and 378.
- **Type assertions in MCP arg helpers**: All `args[key].(type)` assertions use the two-value
  form with `ok` checks (e.g. `intArgDefault`, `boolArgDefault`).
- **Non-blocking event channel**: `addEvent` uses a non-blocking select with default to avoid
  stalling the orchestrator goroutine when the event buffer is full.
- **Parallel worker goroutine closure**: `internal/orchestrator/parallel.go:259-265` each
  goroutine captures a distinct index `i` via parameter, avoiding the classic loop variable
  capture race.
- **Artifact cleanup in codex.go**: Both `schemaPath` directory and `outputDir` are correctly
  deferred for cleanup via `os.RemoveAll` regardless of success or failure.
- **Error classification** (`internal/orchestrator/errors.go:140-162`): `classifyCommandError`
  normalizes to lowercase before matching patterns, reducing false negatives from mixed-case
  error messages.
- **Parallel worker result merging** (`internal/orchestrator/parallel.go:272-307`): After
  `wg.Wait()`, results are merged sequentially with no concurrent map or slice access.
- **Harness map initialization**: All four harness maps are initialized in `NewService`; no nil
  map panics possible.

---

## Summary Statistics

| ID | Severity | File | Issue Summary |
|----|----------|------|---------------|
| HIGH-01 | high | internal/store/state_store.go | Path traversal via jobID/chainID -- **RESOLVED** |
| HIGH-02 | high | internal/store/artifact_store.go | Path traversal via jobID in ArtifactStore -- **RESOLVED** |
| HIGH-03 | high | internal/api/views.go | Path traversal via job artifact ref paths -- **RESOLVED** |
| HIGH-04 | high | internal/runtime/lifecycle.go | Data race on rec.stopRequested -- **RESOLVED** |
| HIGH-05 | high | internal/runtime/runner.go | Full os.Environ() inherited by runner subprocesses -- **RESOLVED** |
| HIGH-06 | high | internal/runtime/lifecycle.go | Full os.Environ() inherited by ProcessManager -- **RESOLVED** |
| HIGH-07 | high | internal/api/server.go | No authentication or authorization -- **RESOLVED** |
| HIGH-08 | high | internal/api/server.go | context.Background() in 5 HTTP handlers -- **RESOLVED** |
| HIGH-09 | high | internal/orchestrator/service.go | context.Background() in async job goroutine -- **RESOLVED** |
| HIGH-10 | high | internal/mcp/server.go | Errors discarded in approve/retry/resume goroutines -- **RESOLVED** |
| MED-01 | medium | internal/store/state_store.go | TOCTOU race in writeAtomically Windows fallback |
| MED-02 | medium | internal/store/state_store.go | Single corrupt file fails entire ListJobs/ListChains |
| MED-03 | medium | internal/api/server.go | JSON encode error ignored in writeJSON |
| MED-04 | medium | internal/runtime/lifecycle.go | logFile.Close() inside mutex |
| MED-05 | medium | internal/api/views.go | RequiredCommands populated with check descriptors |
| MED-06 | medium | internal/runtime/policy.go | Windows-specific code in cross-platform core |
| MED-07 | medium | internal/schema/validate.go | run_system validated by exclusion list |
| MED-08 | medium | internal/api/views.go | loadSprintContract/loadEvaluatorReport ignore errors |
| MED-09 | medium | internal/orchestrator/planning.go | Dead mutation in validatePlanningArtifact |
| MED-10 | medium | internal/mcp/server.go | listenEvents goroutine leak |
| MED-11 | medium | internal/provider/protocol.go | json.MarshalIndent errors ignored |
| MED-12 | medium | internal/orchestrator/service.go | Redundant condition in classifyScope |
| MED-13 | medium | internal/mcp/server.go | isTerminalJobStatus undocumented "cancelled" literal |
| MED-14 | medium | internal/mcp/server.go | toolApprove returns stale snapshot |
| MED-15 | medium | internal/orchestrator/evaluator.go | verificationSatisfiedNormal ignores contract |
| LOW-01 | low | internal/schema/validate.go | run_workers hardcoded to exactly 2 tasks |
| LOW-02 | low | internal/runtime/lifecycle.go | Log filename collision under concurrency |
| LOW-03 | low | internal/api/harness.go | Global harness endpoint unrestricted |
| LOW-04 | low | internal/store/artifact_store.go | Artifact names not length-limited |
| LOW-05 | low | cmd/gorchera/main.go | HTTP server no read/write timeouts |
| LOW-06 | low | internal/provider/provider.go | resolveAdapter is dead code |
| LOW-07 | low | internal/orchestrator/service.go | newJobID/newChainID collision risk |
| LOW-08 | low | internal/provider/codex.go | outputPath string concatenation vs filepath.Join |
| LOW-09 | low | internal/mcp/server.go | Scanner 1MB limit kills MCP server on large requests |
| LOW-10 | low | internal/orchestrator/service.go | git stderr silently discarded |
| LOW-11 | low | internal/provider/protocol.go | profilePrompt dead code |

**Total: 0 critical, 10 high, 15 medium, 11 low = 36 findings**
