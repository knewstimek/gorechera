# Changelog

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
