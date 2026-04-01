# Gorechera Coding Conventions

## Build & Test

```bash
go build ./...
go test ./...
```

Go 1.26, no external dependencies (pure stdlib).

## Go Style

- Tab indentation (gofmt standard)
- Error: return immediately, no nesting
- Variable names: Go idiomatic camelCase
- JSON fields: snake_case
- No unnecessary comments -- express intent through code

## Type Rules

- ALL domain types live in `internal/domain/types.go`
- New domain types -> add to types.go, never declare in other packages
- Exception: package-internal types (e.g., `orchestrator.VerificationContract` in verification.go)
- Helper utility functions like `firstNonEmpty()` exist as package-private in both `planning.go` and `protocol.go` -- reuse from the same package, don't create a third copy

## State Management Pattern

The intended pattern for every job state change:

```go
job.Status = domain.JobStatusXxx
job.SomeField = value
s.addEvent(job, "event_kind", "human-readable message")
s.touch(job)    // sets UpdatedAt
if err := s.state.SaveJob(ctx, job); err != nil {
    return nil, err
}
```

NOTE: Not all existing code follows this exactly. Some paths call SaveJob after adding events inside a switch. The pattern above is the target -- follow it for new code and don't break existing patterns.

## Provider Adapter Rules

Three interfaces to implement:
- `provider.Adapter`: Name(), RunLeader(ctx, job), RunWorker(ctx, job, task)
- `provider.PlannerRunner`: RunPlanner(ctx, job)
- `provider.EvaluatorRunner`: RunEvaluator(ctx, job)

Registration: add to `NewRegistry()` in provider.go (lines 36-38).
Prompts: use `build*Prompt()` functions in protocol.go.
Schemas: use `*Schema()` functions in protocol.go.

All Run* methods return (string, error) where string is raw JSON.
Orchestrator handles unmarshal + validation.
Rough token/cost accounting stays in orchestrator code. Keep adapters billing-agnostic and do not add provider-specific pricing logic.

## Schema Validation

- schema/validate.go contains all validation functions
- ValidateLeaderOutput: checks action, target, task_type, required fields per action
- ValidateWorkerOutput: checks status, summary, blocked_reason/error_reason when required
- ValidateVerificationContract: checks version, goal, scope, required_checks, required_commands

When adding a new leader action or worker status:
1. Add to the allowlist maps at the top of validate.go (leaderActions, workerStatuses, etc.)
2. Add validation rules in the corresponding Validate* function
3. Add handling in service.go's runLoop switch

## Artifact Materialization

- `artifacts.MaterializeWorkerArtifacts(jobID, stepIndex, workerOutput)` -> paths
- `artifacts.MaterializeSystemResult(jobID, stepIndex, runtimeResult)` -> paths
- `artifacts.MaterializeTextArtifact(jobID, name, content)` -> path
- `artifacts.MaterializeJSONArtifact(jobID, name, value)` -> path

All files stored under `.gorechera/{jobID}/`.
Filenames sanitized via sanitizeArtifactName() -- replaces special chars with `-`.
Worker artifact files are named `step-{NN}-{sanitized_name}`.

## Event Naming

Events use `noun_verb` or `noun_adjective` pattern:
- job_created, job_resumed, job_cancelled, job_approved, job_rejected
- job_retry_requested, job_planned, job_completed, job_blocked, job_failed
- leader_requested, leader_summary
- worker_requested, worker_succeeded, worker_blocked, worker_failed
- system_requested, system_succeeded, system_blocked, system_failed
- parallel_workers_requested, parallel_workers_succeeded/blocked/failed
- evaluation_passed, evaluation_blocked, evaluation_failed

## Adding a New CLI Command

main.go pattern:
```go
case "mycommand":
    fs := flag.NewFlagSet("mycommand", flag.ExitOnError)
    someFlag := fs.String("flag", "default", "description")
    fs.Parse(os.Args[2:])
    // implementation
```

## Adding a New HTTP Route

server.go uses ServeMux prefix matching. Job sub-routes are parsed manually inside `handleJob()`.

To add `/jobs/{id}/newroute`:
1. Add a case in the path switch inside `handleJob` (server.go)
2. Create handler method on `*Server`

To add a top-level route:
1. Add `mux.HandleFunc("/newroute", s.handleNewRoute)` in `Handler()` method

## Adding a New System Task Type

To support a new taskType for `run_system`:
1. Add mapping in `mapSystemTask()` (service.go:737-749)
2. Add command allowlist in `NewDefaultPolicy()` (runtime/policy.go:14-24)
3. Current supported types: build, test, lint, search only

## Test Style

- Test files: `*_test.go` in the same package
- Patterns: table-driven or single-function
- Mock provider enables end-to-end loop testing without real AI providers
- Test helpers: use Go test helper patterns (t.Helper())
- `service_test.go`: tests for orchestrator loop, approval, retry, cancel, parallel, harness
- `server_test.go`: tests for HTTP API handlers

## Documentation Rules

When changing code, update:
- `docs/IMPLEMENTATION_STATUS.md` -- if fixing a bug, remove from Known Bugs
- `docs/ARCHITECTURE.md` -- if changing package structure, state machine, or API routes
- `ORCHESTRATOR_SPEC_UPDATED.md` -- if changing spec-level behavior

Must update docs when changing:
- CLI commands / HTTP routes
- State machine transitions
- Approval semantics
- Harness lifecycle
- Provider adapter interface
