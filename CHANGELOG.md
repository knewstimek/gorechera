# Changelog

## v2026.04.04.2

> **CRITICAL HOTFIX** -- The evaluator feedback loop (the core quality gate of the harness) was non-functional since v2026.04.04.1. Jobs could pass with known defects. All users should upgrade immediately.

### Fixed
- **Evaluator gate bypass (CRITICAL)**: `mergeEvaluatorReport` had `if rulePassed { providerPassed = true }` which silently discarded the LLM evaluator's judgment whenever build/test/steps passed mechanically. The evaluator could find concrete DEFECT findings, return `passed: false`, and the merge logic would override it to `true`. This meant the evaluator retry loop -- the central quality mechanism -- was effectively dead code. Removed the override; both rule-based and provider verdicts must now independently agree.
- **Automated checks not enforced (CRITICAL)**: `PreCheckResults` (grep/file_exists/file_unchanged/no_new_deps) were only informational context for the LLM evaluator, not a mechanical gate. A failed grep check could not block a job regardless of the result. Now any failed automated check mechanically blocks the report as a final demote-only gate.

### Added
- **Compact status mode**: `gorchera_status` now defaults to `compact=true`, returning a lightweight view that omits heavy fields (goal, task_text, verification_contract, diff_summary, changed_files, constraints, role_profiles, role_overrides, planning_artifacts, prompt_overrides). Steps reduced to index/target/task_type/status/summary/error_reason. Events capped to last 10. Reduces polling token cost from ~10-15k to ~1-2k per call. Use `compact=false` for full job data.

## v2026.04.04.1

### Breaking
- **3-agent pipeline**: reviewer role removed. Evaluator now performs code review + gate. Pipeline is director -> executor -> evaluator. review/audit task types removed from leader schema.
- **Default pipeline_mode**: changed from light to balanced (THOROUGH evaluator depth).

### Added
- **Evaluator strictness levels** (lenient/normal/strict): all levels default FAIL, improvements found = fail. Strict mode uses adversarial 3-input tracing, domain completeness checks, and extensibility demands. Strictness is now injected into evaluator prompt (was only in leader prompt before).
- **Extreme ambition level**: production-grade quality demands -- fuzz testing, benchmarks, edge case handling, extensible design. Added to both executor and evaluator guidance.
- **Automated checks**: planner generates structured automated_checks in verification_contract (grep/file_exists/file_unchanged/no_new_deps). Executed mechanically before evaluator LLM call, results injected into evaluator payload.
- **Workspace change detection**: pre/post worker execution file tracking. Git fast path (git diff --stat parsing) with SHA-256 hash fallback for non-git workspaces. .gitignore + default excludes (node_modules, .cache, vendor, etc.). Changed files reported per step.
- **Evaluator payload enrichment**: diff_summary and error_reason inlined per step, artifact paths included, changed_files and automated_check_results added. Summary limit raised to 500 chars.
- **Evaluator fix loop**: evaluator "failed" now re-enters leader retry loop (was instant job termination). Leader receives evaluator findings in context to dispatch fix steps.
- **Production presets**: examples/role-profiles.sample.json updated with production (strict+extreme), spark+claude-eval combo, and prompt_overrides examples.
- prompt_overrides support in ChainGoal (was missing -- BUG-1 fix).
- **Executor self-check**: engine_build_cmd/engine_test_cmd injected into executor prompt for pre-submission build/test verification. Reduces fix loop round-trips.
- **Evaluator test correctness check**: all strictness levels now verify test expected values are correct, not just that tests pass.

### Fixed
- Leader schema: system_action changed to anyOf [null, object] for OpenAI strict mode compatibility.
- Leader prompt: [no test files] guidance added (engine artifact is readable by leader agent via workspace access).
- automated_checks schema: all properties in required array for OpenAI strict mode.
- verification_contract required: automated_checks included for OpenAI strict mode.
- prompt_overrides: # REPLACE with empty content now correctly clears base prompt (was silently ignored).
- prompt_overrides: types.go comment updated to remove stale "reviewer" key reference.

## v2026.04.04

### Fixed
- Leader schema: system_action nullable (anyOf null pattern) -- reduces token waste and schema retry errors.
- Leader prompt: [no test files] engine artifact guidance for AI agents with workspace access.

## v2026.04.03

### Added
- ambition_text: custom ambition guidance injected into executor and evaluator prompts. With ambition_level=custom: replaces default text entirely (falls back to medium if blank). With low/medium/high: prepended to default text. Both gorchera_start_job and per-goal gorchera_start_chain support this parameter.
- SUPERVISOR_GUIDE.md: Ambition Levels section now documents exact default prompt text for all levels and shows custom/prepend usage examples.
- Prompt overrides: per-role prompt customization via .gorchera/prompts/{role}.md files (prepend or replace) and gorchera_start_job prompt_overrides parameter
- Schema retry: director/executor/evaluator retry up to 2 times on schema validation failure before marking the step failed
- pre_build_commands: gorchera_start_job accepts a pre_build_commands list; engine runs these commands before go build/test (language-agnostic setup, e.g. go mod tidy, npm install)
- engine_build_cmd / engine_test_cmd: override engine verification commands per job (e.g. npm run build / npm test for Node projects); empty = default Go commands
- Worktree notification: terminal notifications for isolated worktree jobs include workspace_mode, workspace_dir, requested_workspace_dir, diff_stat
- PendingApproval guard: ResumeWithOptions rejects resume on jobs with pending approval (must use approve/reject)
- Orchestrator-specific audit checklist in CLAUDE.md (evaluator gate bypass, approval policy bypass)
- Supervisor Guide (docs/SUPERVISOR_GUIDE.md): goal writing, ambition levels, invariants, provider presets, operational tips

### Fixed
- Flaky test: TestToolStatusWaitReturnsBlockedForOperatorCancellation -- sync Cancel + wait=false snapshot

### Security (Audit V3: 3 HIGH, 4 MEDIUM, 2 LOW fixed)
- H1: Provider CLI environment allowlist (providerEnv) -- was leaking full parent env including API keys
- H2: Lease file path traversal guard (validateLeaseID)
- H3: gorchera_diff pathspec injection block (reject .. and : magic)
- M1: SSE stream termination on blocked/cancelled jobs
- M2: Atomic write TOCTOU fix -- .bak rename pattern instead of remove+rename
- M3: rg removed from CategoryCommand (restrict to CategorySearch)
- M4: git diff --stat 10s timeout via exec.CommandContext
- L1: API error messages sanitized -- no internal paths exposed to client
- L2: listenEvents goroutine leak fixed with done channel

### Changed
- CLAUDE.md cleaned up, README streamlined

## v2026.04.03-rc3

### Added
- Context compaction: role-specific compact payloads for executor/reviewer/evaluator (30-40% token reduction per call)
  - Executor: receives workspace info + previous failure only (not full job JSON)
  - Reviewer: receives step summaries + diff evidence + contract (not full job JSON)
  - Evaluator: receives compact step summaries + role profiles (not raw step data)
  - Director keeps full job state for planning+dispatch
- Evaluator gate consistency check: evaluatorTextContradicts() detects failure language in evaluator text that contradicts passed=true, demotes to failed as final override
- Provider preset profiles in examples/role-profiles.sample.json (cross-provider, codex-only, claude-only, balanced, full strict)

### Fixed
- Evaluator gate bypass: evaluator could say "gate failure, not a pass" while passed=true and job would still complete as done
- pipeline_mode default changed from balanced to light (skip reviewer for simple tasks)

### Changed
- MCP tool descriptions updated for 4-role pipeline architecture
- README "Note on Overhead" replaced with "Pipeline Modes" section explaining light/balanced/full

## v2026.04.03-rc2

### Added
- Pipeline architecture redesign: 6-role -> 4-role (director/executor/reviewer/evaluator)
  - director = planner + leader merged (single AI call for plan+dispatch)
  - tester role removed; engine runs go build/test automatically after executor (rule-based, not AI)
  - Engine verification: build/test results stored as step artifacts, consumed by evaluator
- pipeline_mode parameter: light (skip reviewer) / balanced (default) / full (fix loops + parallel workers)
- resume extra_steps (1-20 bounded) for blocked max_steps_exceeded jobs
- Terminal notification: JSON-RPC 2.0 notifications/job_terminal on done/failed/blocked
  - Cancel race fix: notify only from final persisted terminal state
  - Startup recovery buffering: queue notifications until callback registered + writer installed
- role_overrides added to gorchera_start_job MCP schema (was only in start_chain)
- Legacy job compatibility: jobs without engine artifacts pass evaluator (no retroactive blocking)

### Fixed
- MCP reflect path for ResumeWithOptions (was looking for wrong method name)
- Parallel engine verification SaveJob after failure (crash-safe step state)

### Changed
- Evaluator step coverage is now pipeline_mode-aware (light requires implement only, balanced adds review)
- DefaultRoleProfiles: tester slot reuses executor profile for backward compatibility

## v2026.04.03-rc1

### Added
- In-memory job cache for real-time status API (memory truth + disk backup)
- JobStatusPlanning state for planner phase visibility
- task_why, invariants_to_preserve, scope_boundary in worker prompts
- ambition_level parameter (low/medium/high) for worker autonomy control
- --effort flag support in codex provider
- gorchera_diff MCP tool for workspace diff inspection
- Supervisor goal template and role profile guide in CLAUDE.md/README
- CloneJob deep-copy for cache snapshot isolation

### Fixed
- Status API stale data during job execution (was reading disk only)
- Audit V2 CRITICAL XSS-1: showToast innerHTML -> DOM construction
- Audit V2 HIGH XSS-2: makeBadge class attribute escaping
- Audit V2 HIGH H1: validatePlanningArtifact value -> pointer receiver
- Audit V2 HIGH H2: bearer token constant-time comparison
- Audit V2 HIGH H3: generic HTTP error messages (no internal error leak)
- Cancel cache race condition (cacheUpdate instead of cacheRemove)

### Changed
- Reviewer/evaluator/tester prompts hardened with role-specific behavior
- Leader prompt includes invariants and task_why convention
- Planner schema includes invariants_to_preserve (required, backward compatible)

## v2026.04.02 -- First Release

### Core Engine
- 6-role pipeline: planner -> leader -> executor/reviewer/tester -> evaluator
- 4 strictness levels: strict, normal, lenient, auto (planner-recommended)
- 4 context modes: full, summary, minimal, auto (step-count-based)
- Evaluator gate: complete must pass evaluateCompletion() before done
- Evaluator rubric: optional multi-axis scoring with per-axis thresholds
- Adaptive decomposition: planner recommends strictness and max_steps when auto
- Enhanced planner: codebase analysis before spec, concrete improvements, acceptance criteria
- Error classification: 12 error types with 3-strike retry policy
- Token/cost tracking per job and step

### Job Chaining
- Sequential multi-goal execution with automatic advancement
- Chain-level controls: pause, resume, cancel, skip
- Chain result forwarding: previous job summary injected into next planner context

### Provider Adapters
- Codex (GPT) adapter with --fresh flag (hang prevention)
- Claude adapter (tested with sonnet)
- Mock adapter (testing)
- Per-role provider/model selection via role_overrides
- Role overrides on both start_job and start_chain

### MCP Server (17+ tools)
- gorchera_start_job, gorchera_start_chain
- gorchera_status, gorchera_chain_status (with wait + wait_timeout)
- gorchera_steer (supervisor directive injection)
- gorchera_pause/resume/cancel_chain, gorchera_skip_chain_goal
- gorchera_approve, gorchera_reject, gorchera_retry, gorchera_resume, gorchera_cancel
- gorchera_events, gorchera_artifacts, gorchera_list_jobs

### Supervisor Features
- Mid-flight steering via gorchera_steer
- SUPERVISOR injection prevention (sanitizeLeaderContext)
- Synchronous wait with configurable timeout (default 30s)
- Supervisor guidelines documented in README

### Security (audit: 10 HIGH fixed)
- Path traversal: ID validation regex + path prefix checks
- Data race: stopRequested read under mutex
- Environment leakage: minimalEnv() allowlist replaces os.Environ()
- Authentication: bearer token middleware + localhost binding (HTTP API)
- Context propagation: r.Context() in handlers, shutdownCtx in service
- Error logging: fire-and-forget goroutines now log errors

### Documentation
- ARCHITECTURE.md, IMPLEMENTATION_STATUS.md, PRINCIPLES.md
- CODING_CONVENTIONS.md with extension guides
- BLOG_COMPARISON.md (Anthropic harness engineering comparison)
- AUDIT_REPORT.md (36 findings, 10 HIGH resolved)
- Supervisor guidelines and overhead note in README

### Self-Improvement
- 30+ jobs successfully modifying own codebase
- Audit -> fix pipeline proven (orchestrator audits and fixes itself)
- Blog idea adoption pipeline (read comparison -> implement features)
