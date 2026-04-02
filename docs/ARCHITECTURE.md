# Gorchera Architecture

## Package Structure

```text
cmd/gorchera/main.go             -- CLI entrypoint and command routing
internal/
  api/
    server.go                    -- HTTP control plane for jobs and harness views
    harness.go                   -- Runtime harness HTTP helpers/views
    requests.go                  -- HTTP request DTOs
    views.go                     -- Verification/planning/profile/runtime views
  domain/types.go                -- Canonical domain types: Job, Step, JobChain, RoleProfiles, contracts
  mcp/server.go                  -- MCP stdio server, job/chain tools, wait polling, steer tool
  orchestrator/
    service.go                   -- Core runLoop, job lifecycle, chain lifecycle, steer, harness ownership
    planning.go                  -- Planner phase, strictness/context normalization, sprint contract build
    evaluator.go                 -- Completion gate, evaluator merge, strictness-aware verification
    verification.go              -- Verification contract build/load/prompt helpers
    parallel.go                  -- Parallel worker fan-out (max 2, disjoint target/scope checks)
    workspace.go                 -- Workspace path validation
  policy/policy.go               -- Approval decisions for workspace/network/delete/deploy/command actions
  provider/
    provider.go                  -- Registry and role-aware adapter selection
    protocol.go                  -- Prompt builders, context-mode payload shaping, JSON schemas
    errors.go                    -- Structured provider errors + recommended actions
    claude.go                    -- Claude CLI adapter
    codex.go                     -- Codex CLI adapter
    command.go                   -- Subprocess execution and CLI error classification
    mock/mock.go                 -- Mock provider for end-to-end tests
  runtime/
    runner.go                    -- Synchronous system command execution
    lifecycle.go                 -- Async process manager for harness processes
    policy.go                    -- Runtime executable allowlist per category
    types.go                     -- Runtime request/result/process types
  schema/validate.go             -- Leader/worker/planner/evaluator schema validation
  store/
    state_store.go               -- Atomic JSON persistence for jobs and chains
    artifact_store.go            -- Atomic artifact materialization
```

## State Model

```text
Job: starting -> waiting_leader -> waiting_worker -> running -> ... -> done / failed / blocked
Step: pending -> active -> succeeded / failed / blocked / skipped
ChainGoal: pending -> running -> done / failed / skipped
JobChain: running -> paused -> running -> done / failed / cancelled
```

Notes:
- `JobStatusQueued` exists in `types.go` but `Start` and `StartAsync` currently create jobs in `starting`.
- `complete` never transitions directly to `done`; `evaluateCompletion()` must pass first.
- `blockedReasonStrikeCount()` fails the job after the same blocked reason is recorded three times in a row.
- `runLoop()` is single-flight per job ID within a process. Duplicate `Resume()` / recovery attempts for the same job return the latest persisted snapshot instead of starting another provider turn.

## Recovery Semantics

- `RecoverJobs()` only runs for long-lived controller entry points (`gorchera serve`, `gorchera mcp`), not for one-shot CLI commands such as `run`, `status`, or `resume`.
- Recoverable jobs are the persisted non-terminal states: `starting`, `running`, `waiting_leader`, `waiting_worker`.
- Recovery schedules jobs oldest-first with a bounded concurrency of 2 so a restart cannot stampede the provider with every stale job at once.

## Core Loop

`Service.runLoop()` does the following:

1. `ensurePlanning()` generates `product_spec.md`, `execution_plan.json`, `sprint_contract.json`, and `verification_contract.json` when planning artifacts are missing.
2. Each leader turn persists `waiting_leader`, appends `leader_requested`, then calls `sessions.RunLeader`.
3. Provider calls go through `executeProviderPhase()`, which applies recommended provider actions:
   - retry: up to 3 attempts with exponential backoff starting at 250 ms
   - block: convert the job to `blocked`
   - fail: fail immediately
4. Leader output is JSON-unmarshaled and schema-validated. Invalid JSON or invalid schema currently fails the job immediately.
5. Leader actions:
   - `run_worker` and `run_workers`: dispatch worker tasks
   - `run_system`: run an allowlisted local command with approval checks
   - `summarize`: persist intermediate summary only
   - `complete`: run evaluator gate and only then mark `done`
   - `fail` / `blocked`: terminate accordingly
6. Consecutive `summarize` calls are capped. After two summarize turns, the service forces a completion evaluation path instead of allowing infinite summary loops.

## Chain System

Sequential chains are persisted as `JobChain` records under `.gorchera/state/chains`.

Start path:
- `StartChain()` validates the shared workspace directory.
- Each incoming `ChainGoal` is normalized:
  - `strictness_level`: `strict | normal | lenient`
  - `context_mode`: `full | summary | minimal`
  - `max_steps`: defaults to 8
  - `provider`: defaults to `mock`
- The chain is saved before the first goal starts.
- `startChainGoal()` creates a normal `Job` with `ChainID` and `ChainGoalIndex`, records the new `JobID` on the goal, marks the goal `running`, and starts the job asynchronously.

Completion semantics:
- `handleChainCompletion()` runs only after evaluator-approved job completion.
- If the chain is `paused`, the current goal is marked `done` and no next goal is started.
- Otherwise `advanceChain()` marks the current goal `done` and starts the next pending goal in the same workspace.
- If the last goal finishes, the whole chain becomes `done`.

Chain result forwarding:
- When a chain goal completes as `done`, `advanceChain()` builds a `ChainContext{Summary, EvaluatorReportRef}` from the completed job.
- This context is passed to `startChainGoal()` and attached to the next job as `job.ChainContext`.
- The planner prompt includes a "Previous chain step results" section when `job.ChainContext` is non-nil, so each goal can build on prior work.
- First-goal jobs serialize without a `chain_context` field (pointer + omitempty).

Terminal propagation:
- A chained job ending in `blocked` or `failed` marks the current goal `failed` and the whole chain `failed`.
- `cancelled` is terminal for the chain and prevents later advancement.

## Chain Controls

Chain controls are implemented in `service.go` and exposed through MCP.

Available controls:
- `PauseChain`: sets chain status to `paused`. It does not interrupt the current job; it stops post-job advancement.
- `ResumeChain`: sets status back to `running`. If the current goal already finished while paused, it advances immediately.
- `CancelChain`: interrupts the current goal job by blocking it, marks the current goal failed if still active, then marks the chain `cancelled`.
- `SkipChainGoal`: interrupts the current goal job, marks the goal `skipped`, and starts the next goal. Skipping the final goal marks the chain `done`.

Current control surfaces:
- MCP only: `gorchera_start_chain`, `gorchera_chain_status`, `gorchera_pause_chain`, `gorchera_resume_chain`, `gorchera_cancel_chain`, `gorchera_skip_chain_goal`
- No CLI chain commands
- No HTTP chain routes

## Context Modes

Leader prompts are shaped by `job.ContextMode` through `buildLeaderJobPayload()`:

- `full`: full marshaled job JSON
- `summary`: compact summary with all steps, but only the last two steps retain full detail
- `minimal`: aggregate counters plus the last step only
- `auto`: passed through to the payload builder unchanged; the builder selects the actual mode at runtime based on current step count

Normalization:
- Empty or unrecognized values are normalized to `full`
- `auto` is passed through so the payload builder can resolve it at runtime
- Chain goals carry their own `context_mode`, which is copied into the job created for that goal

## Auto Context Mode

When `context_mode` is set to `"auto"`, `normalizeContextMode()` in planning.go passes the value through unchanged. The leader payload builder (`buildLeaderJobPayload()` in protocol.go) then selects the actual mode at runtime via `autoContextMode()` (protocol.go):

Step-count thresholds used by `autoContextMode(model, stepCount)`:
- `stepCount < 10`: `full` -- full marshaled job JSON
- `10 <= stepCount <= 20`: `summary` -- compact summary, last two steps retain full detail
- `stepCount > 20`: `minimal` -- aggregate counters plus the last step only

The `model` parameter is accepted for forward compatibility (future per-model tuning) but is not used in the current threshold logic.

- This lets long-running jobs shift from `full` context to `summary` or `minimal` automatically as step count grows.
- The `auto` value is exposed in `gorchera_start_job` and per-goal in `gorchera_start_chain`.

Important detail:
- `SupervisorDirective` is removed from the serialized job payload and injected as a dedicated prompt section ahead of job state. This keeps the directive high-priority and prevents it from being duplicated inside summaries.

## Supervisor Steer

`Service.Steer()` injects a supervisor directive into an active job:

- Allowed only when job status is `running`, `waiting_leader`, or `waiting_worker`
- Stored as `job.SupervisorDirective` with a `[SUPERVISOR] ` prefix
- Emits a `supervisor_steer` event
- Exposed via MCP as `gorchera_steer`

Leader prompt behavior:
- The directive is inserted before current job state with explicit highest-priority instructions
- After a successful leader provider call, `runLoop()` clears `job.SupervisorDirective`
- The directive does not bypass evaluator gates, approval checks, harness ownership rules, or chain controls

## Role Profiles And Model Selection

Model/provider selection is role-based, not job-global.

Defaults from `DefaultRoleProfiles()`:
- planner, leader, evaluator -> `opus`
- executor, reviewer, tester -> `sonnet`

Resolution path in `SessionManager`:
1. Read the role-specific execution profile
2. If the role profile omits `provider`, fall back to `job.Provider`
3. If still empty, fall back to `mock`
4. If the selected provider is unavailable and `fallback_provider` is set, use the fallback provider

Adapter-specific model behavior:
- Claude passes the selected `profile.Model` through `--model`
- Codex passes `--model` only when the model name looks like a GPT-family Codex model; Claude shorthand values such as `opus` and `sonnet` are intentionally suppressed
- Codex adapter always passes `--fresh` to prevent session reuse and reduce hang probability

Role overrides on chains:
- Each `ChainGoal` carries `RoleOverrides map[string]RoleProfile` alongside its other per-goal fields.
- MCP `gorchera_start_chain` accepts a `role_overrides` object per goal entry.
- `startChainGoal()` copies `goal.RoleOverrides` into `CreateJobInput.RoleOverrides` when creating the job for that step.
- Resolution priority inside the job: `RoleOverrides[role]` > `RoleProfiles[role]` > job provider > mock fallback.

Stored but not fully enforced yet:
- `fallback_model`
- `effort`
- `tool_policy`
- `max_budget_usd`

## Structured Errors

Provider-side structured errors live in `internal/provider/errors.go`.

Current error kinds:
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

Recommended actions:
- retry: `rate_limited`, `network_error`
- block: `auth_failure`, `billing_required`, `session_expired`
- fail: everything else, including `quota_exceeded` and `transport_error`

Worker-side structured errors:
- Failed or blocked worker outcomes can populate `Step.StructuredReason`
- Current categories include `timeout`, `schema_violation`, `file_access`, `test_failure`, and `build_failure`
- Worker failure events serialize both the human-readable reason and the structured reason JSON

## Workspace Validation And Scope Enforcement

`ValidateWorkspaceDir()` is called from:
- `Service.Start`
- `Service.StartAsync`
- `Service.StartChain`
- MCP `gorchera_start_job`
- MCP `gorchera_start_chain`

Validation rules:
- path must be absolute
- path must exist
- path must resolve to a directory
- directory symlinks are accepted
- permission-denied symlink resolution falls back to `Lstat`/`Clean` when possible

System command scope:
- `resolveSystemWorkdir()` makes relative `system_action.workdir` values workspace-relative
- `classifyScope()` marks targets as `workspace_local`, `workspace_outside`, or `unknown`
- Approval policy blocks workspace-external writes/commands, network access, deploy, git push, credential access, and mass delete

## Worker And System Execution

Single worker execution:
- `runWorkerStep()` creates one active step, calls the role-selected worker adapter, validates JSON/schema, materializes artifacts, and updates step/job status
- On successful single-worker completion, `collectWorkspaceDiffSummary()` runs `git -C <workspace> diff --stat` and stores the result in `Step.DiffSummary`
- If `git diff --stat` fails or the workspace is not a git repo, `DiffSummary` remains empty

Parallel worker execution:
- `parallel.go` enforces `maxParallelWorkers = 2`
- Targets and write scopes must be disjoint
- Parallel tasks can be expressed either as `run_workers` or embedded `parallel:` artifact specs
- Parallel worker steps do not currently populate `DiffSummary`

System execution:
- `run_system` currently supports `build`, `test`, `lint`, `search`, and `command`
- `mapSystemTask()` maps those types to runtime and approval categories
- Runtime allowlists live in `internal/runtime/policy.go`

## Artifact Flow

Artifacts are stored under `.gorchera/artifacts/<jobID>/`.

Planning artifacts:
- `product_spec.md`
- `execution_plan.json`
- `sprint_contract.json`
- `verification_contract.json`

Worker artifacts:
- `MaterializeWorkerArtifacts()` writes real file content from `WorkerOutput.FileContents` when provided
- Otherwise it writes a summary JSON payload

System artifacts:
- `MaterializeSystemResult()` stores the full runtime result JSON

## Control Surfaces

CLI:
- Job lifecycle and inspection commands exist for jobs and harness processes
- No chain-specific CLI commands yet

HTTP API:
- `/healthz`
- `/jobs`, `/jobs/{id}`
- `/jobs/{id}/events`, `/jobs/{id}/events/stream`
- `/jobs/{id}/artifacts`
- `/jobs/{id}/verification`
- `/jobs/{id}/planning`
- `/jobs/{id}/evaluator`
- `/jobs/{id}/profile`
- `/jobs/{id}/resume`, `/approve`, `/reject`, `/retry`, `/cancel`
- `/harness/*` and `/jobs/{id}/harness/*`

MCP (17 tools):
- job tools: `gorchera_start_job`, `gorchera_list_jobs`, `gorchera_status`, `gorchera_events`, `gorchera_artifacts`, `gorchera_approve`, `gorchera_reject`, `gorchera_retry`, `gorchera_cancel`, `gorchera_resume`
- chain tools: `gorchera_start_chain`, `gorchera_chain_status`, `gorchera_pause_chain`, `gorchera_resume_chain`, `gorchera_cancel_chain`, `gorchera_skip_chain_goal`
- steer tool: `gorchera_steer`
- `gorchera_start_job` key parameters: `goal`, `provider`, `workspace_dir`, `max_steps`, `strictness_level`, `context_mode` (supports `auto`)
- `gorchera_start_chain` key parameters: `workspace_dir`, `goals[]` with per-goal `goal`, `provider`, `strictness_level`, `context_mode`, `max_steps`, `role_overrides`
- `wait=true` is supported on `gorchera_status` and `gorchera_chain_status` with 2-second polling
- Omitted `wait_timeout` defaults to 30 seconds; `wait_timeout=0` preserves the 5-minute maximum
- Positive `wait_timeout` values are interpreted as seconds

## Evaluator Rubric Scoring

Rubric axes allow the planner to define multi-dimensional evaluation criteria in the `VerificationContract`.

Schema (domain/types.go):
- `RubricAxis`: `{name string, weight float64, min_threshold float64}`
- `RubricScore`: `{axis string, score float64, passed bool}`
- `VerificationContract.RubricAxes []RubricAxis` -- axes defined by the planner
- `EvaluatorReport.RubricScores []RubricScore` -- per-axis scores returned by the provider

Enforcement in `mergeEvaluatorReport()` (evaluator.go):
- Applied only when both `verification.RubricAxes` and `providerReport.RubricScores` are non-empty.
- Additive enforcement: existing pass/fail logic (step coverage, strictness-level rules) runs first; rubric can only demote a passing report, never promote a failing one.
- Each reported score is checked against its `min_threshold`; axes below threshold are collected in `failedAxes`.
- If any axis fails: `report.Passed = false`, `report.Status = "failed"`, reason lists each failed axis with its score and threshold.

Evaluator prompt:
- When rubric axes are present in the verification contract, the evaluator prompt includes a `RUBRIC SCORING` section.
- The evaluator must score each axis on a 0.0-1.0 scale with one-sentence reasoning per axis.

## Adaptive Decomposition (strictness=auto)

When `strictness_level` is set to `"auto"`, the planner chooses the evaluation level dynamically.

Resolution path in `ensurePlanning()` (planning.go):
1. Job is created with `StrictnessLevel = "auto"` (normalizeStrictnessLevel passes it through).
2. After the planner phase runs, the planner output fields `RecommendedStrictness` and `RecommendedMaxSteps` are read.
3. If `RecommendedStrictness` is one of `strict | normal | lenient`, it is applied to `job.StrictnessLevel`.
4. If the recommendation is empty or unrecognised, `job.StrictnessLevel` falls back to `"normal"`.
5. If `RecommendedMaxSteps > 0`, `job.MaxSteps` is updated.
6. The resolved level is used for the rest of the job lifecycle: sprint contract, evaluator gate, step-type thresholds.

Planner guidance (protocol.go):
- The planner prompt instructs the planner to recommend strictness based on goal complexity and model capabilities.
- Stronger models (opus) can handle stricter evaluation with fewer steps.
- Weaker models (sonnet, haiku) benefit from normal strictness and more steps.

Fallback:
- If the planner phase is skipped (unsupported phase), `"auto"` reaching `buildSprintContract()` is treated as `"normal"`.

## Planner Prompt Enhancement

`buildPlannerPrompt()` in protocol.go includes several sections beyond the raw job JSON.

Role profiles section:
- A formatted list of all role profiles (planner, leader, executor, reviewer, tester, evaluator) is appended.
- Each entry shows `role: provider/model`, e.g. `executor: claude/sonnet`.
- This informs the planner's `recommended_strictness` and `recommended_max_steps` so it can calibrate for actual model capability.

Chain context section:
- When `job.ChainContext` is non-nil, a "Previous chain step results" section is injected with the prior step's summary and evaluator report reference.
- Allows the planner to scope the current goal relative to what the previous chain step accomplished.

Codebase analysis instruction:
- The planner is instructed to read relevant source files before writing the spec to ground the plan in current reality rather than assumptions.

Measurable acceptance criteria:
- The planner prompt requires acceptance criteria to be verifiable (e.g. `go test ./... exits 0`), not vague (e.g. "code is clean").
