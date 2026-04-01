# Gorechera Architecture

## Package Structure

```
cmd/gorechera/main.go           -- CLI entrypoint, flag parsing, command routing
internal/
  domain/types.go               -- ALL domain types (Job, JobChain, ChainGoal, Step, TokenUsage, Event, LeaderOutput, WorkerOutput, etc.)
  orchestrator/
    service.go                  -- Core: runLoop, Start/StartChain/Resume/Cancel/Retry/Approve/Reject, chain advancement, harness mgmt
    planning.go                 -- Planner phase, buildPlanningArtifact, buildSprintContract
    evaluator.go                -- Evaluator phase, mergeEvaluatorReport, scoreCompletion, strictness-aware completion gates
    verification.go             -- VerificationContract build/load/validate
    parallel.go                 -- Parallel worker fan-out (max 2, disjoint scope enforcement)
  provider/
    provider.go                 -- Adapter/PlannerRunner/EvaluatorRunner interfaces, Registry, SessionManager
    protocol.go                 -- Prompt builders (buildLeaderPrompt etc.), JSON schema definitions
    command.go                  -- probeExecutable, runExecutable (CLI subprocess execution)
    errors.go                   -- ProviderError, ErrorKind (5 kinds)
    claude.go                   -- ClaudeAdapter (CLI wrapper scaffolding)
    codex.go                    -- CodexAdapter (CLI wrapper scaffolding)
    mock/mock.go                -- MockAdapter (fully working for end-to-end testing)
  api/
    server.go                   -- HTTP handler, routing (ServeMux prefix matching)
    harness.go                  -- Harness process HTTP handlers
  schema/validate.go            -- LeaderOutput/WorkerOutput/VerificationContract schema validation
  policy/policy.go              -- Approval policy (safe vs risky action classification)
  runtime/
    runner.go                   -- Runner.Run() synchronous command execution + output capture
    lifecycle.go                -- ProcessManager (async process start/stop/status/wait)
    policy.go                   -- Runtime command allowlist (per category)
    types.go                    -- Category, Request, ProcessHandle, Result
  store/
    state_store.go              -- File-based Job + JobChain save/load/list (JSON, atomic write)
    artifact_store.go           -- Artifact materialization (worker/system/text/JSON)
```

## State Machine

```
Job:  starting -> running -> waiting_leader -> (action) ->
        waiting_worker -> (result) -> running -> ... -> done / failed / blocked

      NOTE: JobStatusQueued is defined in types.go but never used in current code.
            Start() creates jobs directly as JobStatusStarting.

      blocked states:
        - max_steps_exceeded
        - leader returned blocked
        - worker returned blocked
        - system action blocked by approval policy
        - evaluator blocked completion

Step: pending -> active -> succeeded / blocked / failed / skipped

ChainGoal: pending -> running -> done / failed

JobChain: running -> done / failed
```

## Core Loop (service.go:runLoop)

```
1. ensurePlanning()
   - Run planner phase (provider-backed)
   - Generate: product_spec.md, execution_plan.json, sprint_contract.json, verification_contract.json
   - Rough token/cost usage is estimated from serialized phase input + raw provider output and accumulated on the job (and current step when one exists)
   - If planner unsupported (ErrorKindUnsupportedPhase): fall back to hardcoded planning artifact (BUG-2)

2. for job.CurrentStep < job.MaxSteps:
   a. Set status = waiting_leader, save
   b. Call sessions.RunLeader(job) -> raw JSON
   c. Estimate rough token usage at 1 token / 4 chars for input and output, then accumulate it on Job.TokenUsage and the current Step.TokenUsage when applicable
   d. Unmarshal + validate LeaderOutput
   e. NOTE: JSON parse failure or validation failure -> immediate failJob (NOT re-request as spec says)
   f. Switch on leader.Action:
      - run_worker:  -> runWorkerStep -> buildWorkerPlans -> single worker execution
      - run_workers: -> runWorkerStep -> buildWorkerPlans -> runParallelWorkerPlans (goroutine per plan)
      - run_system:  -> runSystemStep -> buildSystemRequest -> approval check -> runtime.Run
      - summarize:   -> store summary in job, continue loop
      - complete:    -> evaluateCompletion -> if passed: done, else: blocked
      - fail:        -> failJob
      - blocked:     -> set blocked status, return
      - default: failJob with "unrecognized leader action"

3. If max_steps exceeded: blocked with "max_steps_exceeded"
```

## Sequential Job Chaining

`StartChain()` persists a `JobChain`, starts the first goal asynchronously, and records that goal's `JobID` on the chain before the goroutine is launched.

When a chained job reaches evaluator-approved `done`, `handleChainCompletion()` loads the persisted chain and `advanceChain()` either:
- marks the finished goal `done` and starts the next pending goal
- or marks the full chain `done` when the final goal finishes

When a chained job reaches a terminal `blocked` or `failed` state, `handleChainTerminalState()` marks the active `ChainGoal` and the whole `JobChain` as `failed`. No later goals are started.

Later chain goals remain `pending` with empty `JobID` fields until `advanceChain()` starts them.

NOTE: Both `run_worker` and `run_workers` go through `runWorkerStep()`, which calls `buildWorkerPlans()` internally. `buildWorkerPlans` dispatches to single or parallel based on the action.

Completion strictness:
- `strict`: requires `implement`, `review`, and `test` coverage.
- `normal`: requires `implement`; `review` is optional; verification can be satisfied by a succeeded `test`, `build`, or `command` step.
- `lenient`: can accept provider pass without structural step coverage.

## Provider Adapter Pattern

```
                    Adapter interface
                   /       |        \
            MockAdapter  ClaudeAdapter  CodexAdapter
                |            |              |
            (complete)   (scaffolding)  (scaffolding)
```

- Adapter: Name(), RunLeader(), RunWorker()
- PlannerRunner: RunPlanner()
- EvaluatorRunner: RunEvaluator()
- PhaseAdapter: all three combined
- Registry: map[ProviderName]Adapter, auto-registers Codex + Claude at construction (provider.go:36-38)
- SessionManager: resolves role -> profile -> adapter, handles fallback provider

## Prompt & Schema

protocol.go defines:
- buildLeaderPrompt(job) -- injects full job state JSON + role profile + instructions
- buildWorkerPrompt(job, task) -- injects job state + task + verification contract
- buildPlannerPrompt(job) -- injects job state + profile
- buildEvaluatorPrompt(job) -- injects job state + verification contract
- leaderSchema(), workerSchema(), plannerSchema(), evaluatorSchema() -- JSON Schema strings

Claude adapter passes schema via `--json-schema` CLI flag.
Codex adapter writes schema to temp file, passes via `--output-schema` flag.

The orchestrator stores rough token usage by estimating serialized prompt/input text and raw provider output at 1 token per 4 characters. Estimated cost is a heuristic only, not provider billing.

NOTE: prompts inject the ENTIRE job JSON (`json.MarshalIndent(job, "", "  ")`). This means all steps, events, artifacts are included. For large jobs this will be very large.

## run_system Task Type Mapping

`mapSystemTask()` (service.go:737-749) only supports these taskType values:

| taskType | runtime.Category | policy.ActionType |
|----------|-----------------|-------------------|
| "build"  | CategoryBuild   | ActionBuild       |
| "test"   | CategoryTest    | ActionTest        |
| "lint"   | CategoryLint    | ActionLint        |
| "search" | CategorySearch  | ActionSearch      |

Any other taskType (including "command") returns error. The `command` category exists in runtime/types.go and runtime/policy.go but is NOT mapped in mapSystemTask.

## Approval Flow

```
leader returns run_system
  -> buildSystemRequest(job, leader) -> (runtime.Request, policy.Request)
  -> approval.Evaluate(policyReq)
  -> if DecisionBlock AND not already approved:
       step = blocked, job.PendingApproval = {...}
       operator calls POST /approve -> runSystemStepWithApproval(approvalGranted=true)
       operator calls POST /reject -> job stays blocked
  -> if DecisionAllow (or approvalGranted=true):
       runtime.Run(req) -> result -> step succeeded/failed
```

## Parallel Execution

```
leader returns run_workers with exactly 2 tasks
  -> runWorkerStep -> buildWorkerPlans -> buildParallelWorkerPlansFromTasks
  -> validate: disjoint targets, disjoint write scopes, max 2 workers
  -> create steps for all plans (registered on job before execution)
  -> goroutine per plan: executeParallelWorkerPlan
  -> sync.WaitGroup.Wait()
  -> collect results, merge into job state
  -> overall status: blocked if any blocked, failed if any failed, running if all succeeded
```

## Artifact Flow

- Worker artifacts: MaterializeWorkerArtifacts -> uses FileContents when available, falls back to summary JSON
- System artifacts: MaterializeSystemResult -> full runtime.Result JSON (stdout, stderr, exit code, timing)
- Planning artifacts: MaterializeTextArtifact (markdown) + MaterializeJSONArtifact (structured data)
- All stored under `.gorechera/{jobID}/`
- Worker artifact filenames: `step-{NN}-{sanitized_name}`

## HTTP Routing

server.go uses `http.ServeMux` with prefix matching:
- `/healthz` -> handleHealth (exact)
- `/jobs` -> handleJobs (exact, GET list / POST create)
- `/jobs/` -> handleJob (prefix, manually parses sub-path for {id} and sub-routes)
- `/harness` -> handleHarness (exact + prefix)
- `/harness/` -> handleHarness (prefix)

Inside `handleJob`, sub-paths are parsed manually:
```
/jobs/{id}                    -> GET job detail
/jobs/{id}/events             -> GET events
/jobs/{id}/events/stream      -> GET SSE stream
/jobs/{id}/artifacts          -> GET artifacts
/jobs/{id}/verification       -> GET verification view
/jobs/{id}/planning           -> GET planning view
/jobs/{id}/evaluator          -> GET evaluator view
/jobs/{id}/profile            -> GET profile view
/jobs/{id}/resume             -> POST resume
/jobs/{id}/approve            -> POST approve
/jobs/{id}/reject             -> POST reject
/jobs/{id}/retry              -> POST retry
/jobs/{id}/cancel             -> POST cancel
/jobs/{id}/harness            -> handleJobHarness (delegates)
/jobs/{id}/harness/...        -> handleJobHarness (delegates)
```

To add a new route: add a case in the sub-path switch inside handleJob (server.go).

## MCP Tooling

The MCP stdio server exposes both single-job and chain lifecycle tools:
- `gorechera_start_job`, `gorechera_status`, `gorechera_list_jobs`, `gorechera_events`, `gorechera_artifacts`
- `gorechera_start_chain`, `gorechera_chain_status`
- control-plane tools: `gorechera_approve`, `gorechera_reject`, `gorechera_retry`, `gorechera_cancel`, `gorechera_resume`, `gorechera_steer`

## CLI Command Pattern

main.go uses `os.Args[1]` as the command name with a switch statement.
Each command has its own flag set (e.g., `flag.NewFlagSet("run", flag.ExitOnError)`).
To add a new CLI command: add a case in the main switch + define its flags.

## Known Code Duplication

- `firstNonEmpty()` is defined in both `planning.go:200` and `protocol.go:254` (same logic, different packages). Both are package-private so no conflict, but be aware.
