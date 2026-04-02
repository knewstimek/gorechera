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

Normalization:
- Empty or unrecognized values are normalized to `full`
- Chain goals carry their own `context_mode`, which is copied into the job created for that goal

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

MCP:
- job tools: start, list, status, events, artifacts, approve, reject, retry, cancel, resume
- chain tools: start, status, pause, resume, cancel, skip
- steer tool: `gorchera_steer`
- `wait=true` is supported on `gorchera_status` and `gorchera_chain_status` with 2-second polling
- Omitted `wait_timeout` defaults to 30 seconds
- `wait_timeout=0` preserves the original 5-minute timeout
- Positive `wait_timeout` values are interpreted as seconds
