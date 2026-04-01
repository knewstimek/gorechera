# Gorechera Implementation Status

Last verified: 2026-04-02 (TOCTOU fix applied)

## Build & Test

```bash
go build ./...   # PASS
go test ./...    # PASS
```

Go 1.26, no external dependencies.

## Known Bugs

All known bugs have been fixed (2026-04-02, TOCTOU fixed separately).

### Fixed Bugs

| Bug | Fix | Files changed |
|-----|-----|---------------|
| BUG-1: runLoop switch missing default | Added default case calling failJob with "unrecognized leader action" | service.go |
| BUG-2: buildPlanningArtifact hardcoded | Removed hardcoded strings; seed values used directly, nil seed gets minimal fallback | planning.go |
| BUG-3: Worker artifacts metadata only | Added FileContents map to WorkerOutput; MaterializeWorkerArtifacts stores real content when available | types.go, artifact_store.go |
| BUG-4: phase parameter discarded | Removed unused phase parameter from runPhase and call sites | provider.go |
| BUG-5: verificationSatisfied string matching | Removed fragile strings.Contains check; now checks artifact + summary presence | verification.go |

### Audit Fixes (2026-04-02)

| Finding | Resolution | Files changed |
|---------|------------|---------------|
| completionRetryPending summarize cleanup | `summarize` now clears stale `blocked_reason` during completion retry, and the no-new-steps `complete` retry return now touches and persists job state before exit | service.go, service_test.go |
| classifyWorkerFailure validator detection | Worker failure classification now treats validator-style messages such as `is required`, `invalid`, and `validation failed` as schema violations | service.go, service_test.go |
| Minimal mode step counting | Minimal context payload now reports `blocked_steps` separately and only counts empty/`active` statuses as active | protocol.go, provider_test.go |
| Summary mode UTF-8 truncation | Summary payload truncation now slices by rune instead of bytes to avoid splitting multibyte UTF-8 text | protocol.go, provider_test.go |
| Parallel worker failed-status recovery | Parallel fan-out keeps the job running when a worker returns `status="failed"`, marks that step failed, and hands control back to the leader like the single-worker path | parallel.go, service_test.go |

### New Bugs Found

| Bug | Description | Severity | Status |
|-----|-------------|----------|--------|
| TOCTOU race in harness ownership | service.go: check-then-act on harness ownership is not atomic under concurrent requests | Medium | 수정 완료 |
| Fallback error swallowing | provider.go: fallback path discards original error, losing diagnostic context | Low | 수정 완료 |
| Temp file leak | codex.go: temp files not cleaned up on early return paths | Low | 수정 완료 |
| Stream command infinite block | main.go: stream subcommand blocks indefinitely with no timeout or cancellation | Medium | 수정 완료 |

## Implemented

- Go module, CLI entrypoint (20+ commands)
- File-based StateStore and ArtifactStore (JSON, atomic write)
- SessionManager, ProviderRegistry, provider adapter boundary
- MockAdapter: implement -> review -> test -> complete end-to-end loop
- Bounded orchestrator loop with state machine
- Structured leader/worker schema validation
- Approval policy (safe vs risky action classification)
- Runtime Runner (sync command execution) + ProcessManager (async process lifecycle)
- run_system leader action with approval policy enforcement
- HTTP API (25+ routes): job CRUD, events, artifacts, harness, SSE streaming
- CLI/HTTP control plane: approve, reject, retry, cancel, resume
- Runtime harness: global inventory + job-scoped ownership enforcement
- Role-based execution profiles (per-job, routing for leader/worker/planner/evaluator)
- Planner and evaluator as provider-backed phases
- Parallel worker fan-out (max_parallel_workers=2, disjoint write scope)
- Verification contract surface (read-only, derived from sprint contract + evaluator evidence)
- Pending approval state for blocked system actions + operator approve/reject
- Worker artifact real content storage via FileContents field
- Rough token/cost tracking on job + step state using serialized input/output heuristics (1 token / 4 chars, non-billing estimate)
- Normal strictness evaluator gate: `implement` required, `review` optional, rule-based override for provider blocked
- Claude adapter real integration (planner/leader/worker confirmed, --permission-mode dontAsk, --json-schema, stdin prompt)
- Codex adapter real integration (GPT full pipeline done convergence achieved, stdin prompt, workspace-write sandbox)
- MCP server (JSON-RPC 2.0 stdio, 10 tools, notification support)
- Evaluator strictness 3 levels (strict/normal/lenient) with per-level verification rules
- TOCTOU fix: atomic harness ownership via claimHarness/releaseHarnessClaim + harnessInflight map
- Fail-fast workspace directory validation during job creation in orchestrator Start/StartAsync and MCP start-job handling
- Normal mode done convergence: GPT-only pipeline ~89 seconds end-to-end

## Not Implemented

### Priority 1 -- Required for real operation

| Item | Spec ref | Current state |
|------|----------|---------------|
| Claude adapter (real) | spec:102-106 | **Done.** Real integration with --permission-mode dontAsk, --json-schema, --output-format json, stdin prompt, per-role model selection. |
| Codex adapter (real) | spec:102-106 | **Done.** Real integration with codex exec, --output-schema, workspace-write sandbox, stdin prompt. Full pipeline done convergence confirmed. |
| Error classification (10 types) | spec:419-432 | Only 5 types: missing_executable, probe_failed, command_failed, invalid_response, unsupported_phase. Missing: auth, quota, rate_limit, billing, session_expired, network, transport. |
| Error-specific retry/block policy | spec:433-478 | None. All errors result in simple fail. |

### Priority 2 -- Required for safe execution

| Item | Spec ref | Current state |
|------|----------|---------------|
| Retry 3-strike rule | spec:404-412 | None. Retry() increments RetryCount and restarts runLoop. No duplicate blocked_reason detection. |
| blocked_reason code standardization | spec:456-467 | Free-form strings. Spec requires: authentication_required, billing_required, quota_exceeded, etc. |
| Evaluator scoring axes (5) | spec:391-396 | None. Only step type coverage checked. Missing: functionality, code_quality, protocol_compliance, ux_or_product_quality, test_health. |
| ThresholdMinSteps unused | evaluator.go | Field exists in SprintContract but never read in evaluation logic. |
| Sprint contract negotiation | spec:304-321 | None. buildSprintContract() auto-generates. No leader confirmation step. |
| Artifact merge rule validation | spec:162 | None. Parallel worker results merged without rule validation. |

### Priority 3 -- Long-term goals

| Item | Spec ref | Current state |
|------|----------|---------------|
| Context strategy selection | spec:679-695 | None. Three strategies defined in spec but not implemented. |
| Leader milestone summarization | spec:709-717 | LeaderContextSummary field exists but no milestone-based compression or session reset. |
| Suspension/resume state preservation | spec:483-501 | Resume() is simple LoadJob + runLoop. No artifact snapshot, no resume-enabled blocked_reason distinction. |
| Audit log (who) | spec:624-630 | Events record operator commands but not operator identity. |
| Browser evaluator | spec:632-663 | None. |
| Web UI | spec:547-555 | None. |
| Dev server lifecycle | spec:632-663 | None. No readiness probe, port orchestration, restart policy. |
| Persisted role profile registry | spec:108-136 | Per-job only. No global registry. |

## Mock Provider Limitations

mock/mock.go is for testing only:
- Parallel fan-out triggers only when goal contains "parallel" (mock.go:47)
- RunPlanner hardcodes Gorechera-specific strings (mock.go:123-178)
- RunWorker creates no actual files, returns artifact names only (mock.go:207-233)
- RunEvaluator checks step type coverage only (mock.go:180-205)

## Self-Hosting Roadmap

Currently at Phase 1. Known bugs fixed. Next:

1. **Phase 1** (current): Real Claude adapter + provider-backed planning
2. **Phase 2**: Error classification + retry 3-strike + blocked_reason standardization
3. **Phase 3**: Run Gorechera on itself as a job, evaluator verifies `go build && go test`
4. **Phase 4**: Human-supervised self-hosting loop
