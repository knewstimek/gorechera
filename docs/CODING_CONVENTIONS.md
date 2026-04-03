# Gorchera Coding Conventions

## Build And Test

```bash
go build ./...
go test ./...
```

## Go Style

- Use `gofmt` output without local deviation.
- Return errors early; avoid unnecessary nesting.
- Keep domain JSON fields in `snake_case`.
- Prefer small, explicit helpers over hidden cross-package magic.
- Comments should explain a non-obvious rule, not restate the code.

## Domain Type Rules

- All cross-package domain types live in `internal/domain/types.go`.
- Add new job, step, chain, profile, or contract fields there first.
- Keep package-internal transport or helper structs local to their package.
- If a new status is added, also add the corresponding validation helper in `types.go`.

## State Persistence Pattern

For any job or chain mutation:

```go
entity.Field = value
s.addEvent(job, "event_kind", "message") // job only
s.touch(job)                             // or s.touchChain(chain)
if err := s.state.SaveJob(ctx, job); err != nil {
    return nil, err
}
```

Guidelines:
- Persist before starting asynchronous follow-up work.
- Do not update chain state only in memory and then launch the next goal.
- When a mutation changes terminal semantics, update both the in-memory struct and the persisted record before returning.
- Recoverable-state cleanup must be lease-aware. Do not block fresh active jobs just because they are in `waiting_*`.
- Interruption handling should convert stranded recoverable jobs into `blocked` with an explicit reason rather than silently dropping runtime ownership.

## Workspace Rules

- `WorkspaceDir` is the actual execution path. `RequestedWorkspaceDir` is the operator-supplied source workspace path.
- `workspace_mode=isolated` stays opt-in and must prepare a detached git worktree instead of mutating the requested workspace directly.
- Promotion from an isolated workspace is a supervisor/operator action; orchestrator code must not auto-merge detached worktree changes back into the primary workspace.
- Chain goals still assume a shared workspace unless chain-specific isolation semantics are designed end-to-end.

## Provider Adapter Rules

Interfaces:
- `Adapter`: `RunLeader`, `RunWorker`
- `PlannerRunner`: `RunPlanner`
- `EvaluatorRunner`: `RunEvaluator`

Registration:
- Register adapters in `provider.NewRegistry()`.

Selection:
- Provider resolution is role-specific.
- Use `SessionManager.resolveProfile()` / `resolveAdapter()` instead of re-implementing fallback logic.
- `fallback_provider` is resolved in `adapterForProfile()`.
- `fallback_model` retry logic belongs in `SessionManager`, not inside individual adapters or orchestrator state transitions.
- `fallback_model` may trigger at most one retry on the same already-selected adapter, and only for provider command failures that happen before any structured response is produced.
- Blank or model-equal `fallback_model` values must behave as disabled.

Model handling:
- Claude consumes the selected model directly.
- Codex should only emit `--model` for Codex/GPT-family values.
- Do not silently treat Claude shorthand model names as valid Codex model flags.

Error handling:
- New provider transport/classification work belongs in `internal/provider/errors.go` and `internal/provider/command.go`.
- When adding a new provider error kind, also decide its `RecommendedAction`.

## Prompt And Schema Rules

- Update prompt builders in `internal/provider/protocol.go`.
- Update schema validation in `internal/schema/validate.go`.
- Every new leader action or worker status needs both:
  - schema validation
  - orchestrator handling in `runLoop()` or worker execution paths

Leader context:
- `ContextMode` must stay normalized to `full`, `summary`, or `minimal`.
- Supervisor directives must remain a separate prompt section, not duplicated inside serialized job payloads.
- Keep prompt guidance abstract and failure-mode oriented. Encode categories like contract violations, lifecycle/retry/recovery safety, duplicate execution, contradictory evidence, and external contract drift rather than project-specific bug examples.
- Role prompts should stay distinct:
  - executor: implementation
  - evaluator: adversarial counterexample and invariant checking + completion gate (depth varies with pipeline_mode)
  - tester: executable verification

## Verification Contract Rules

- Planning generates the persisted verification contract.
- Test tasks should be decorated with verification-contract context through `decorateTaskForVerification()`.
- Do not bypass evaluator gating by writing `done` directly.
- If you change completion semantics, update:
  - `planning.go`
  - `verification.go`
  - `evaluator.go`
  - docs

## Artifact Rules

- Keep artifact writes atomic through `ArtifactStore`.
- Worker artifacts should prefer `FileContents` when available.
- System artifacts should store the full runtime result JSON.
- `Step.DiffSummary` is reserved for workspace diff visibility; do not overload it with arbitrary notes.

## Runtime And Approval Rules

- Add new system task types in `mapSystemTask()` and in runtime/policy allowlists together.
- Approval policy is category- and scope-based. Do not special-case risky behavior in a provider adapter.
- Workspace-relative command directories must continue to flow through `resolveSystemWorkdir()`.

## Chain Extension Guide

When extending the chain system, make changes in this order.

1. Domain model:
   - Add statuses or fields in `internal/domain/types.go`.
   - Update `ValidChainStatus` / `ValidChainGoalStatus` as needed.

2. Persistence:
   - Confirm the new fields round-trip through `StateStore` JSON save/load.
   - Add store tests if the extension changes persisted semantics.

3. Orchestrator lifecycle:
   - Update `StartChain`, `startChainGoal`, `advanceChain`, `handleChainCompletion`, `handleChainTerminalState`, and any operator control methods that the new behavior affects.
   - Preserve the invariant that only orchestrator-owned code starts the next chain goal.
   - Preserve evaluator-gated completion. A chain goal is not `done` until the underlying job is evaluator-approved `done`.

4. Control-plane surface:
   - Add MCP tools in `internal/mcp/server.go` only if the behavior is intentionally exposed.
   - Do not document or expose chain operations through CLI or HTTP unless they are actually wired.
   - If wait semantics are needed, follow the existing MCP polling pattern instead of inventing a second status mechanism.

5. Cancellation/pausing semantics:
   - Pausing must stop advancement, not force-kill the active goal.
   - Cancelling or skipping an active goal must go through `interruptChainGoalJob()` so the job state is persisted consistently.
   - New terminal chain statuses must short-circuit `advanceChain()`.

6. Tests:
   - Add service tests for lifecycle behavior.
   - Add MCP tests if a new MCP tool or response path is added.
   - Add domain/status validation tests when new statuses are introduced.

7. Documentation:
   - Update `docs/ARCHITECTURE.md` for lifecycle semantics and control surfaces.
   - Update `docs/IMPLEMENTATION_STATUS.md` for newly available behavior.
   - Update `docs/PRINCIPLES.md` if the change affects non-bypassable invariants.

## Extension Guide: Adding a New Rubric Axis

Rubric axes let an operator require minimum quality scores on named dimensions (e.g., "correctness", "coverage").

1. **Domain model** (`internal/domain/types.go`):
   - `RubricAxis` struct has `Name string`, `Weight float64`, and `MinThreshold float64`.
   - `VerificationContract.RubricAxes []RubricAxis` holds the declared axes.
   - `RubricScore` struct has `Axis string`, `Score float64`, `Passed bool`.
   - `EvaluatorReport.RubricScores []RubricScore` holds per-axis results from the provider.
   - No new fields need to be added for a new axis -- axes are data, not code.

2. **Contract definition** (`internal/orchestrator/verification.go`):
   - Add the axis name and `min_threshold` to the `rubric_axes` array in the verification contract that is materialized during planning.
   - The threshold is a float64 in [0,1] or [0,100] depending on convention; be consistent with existing axes.

3. **Evaluator enforcement** (`internal/orchestrator/evaluator.go` -- `mergeEvaluatorReport()`):
   - The enforcement loop at lines 197-225 iterates `verification.RubricAxes`, builds a threshold map by name, and checks each `providerReport.RubricScores` entry.
   - Any axis score below its threshold causes the report to be demoted to `status="failed"`.
   - This is purely data-driven: no code change is needed when adding a new axis to a contract.

4. **Provider prompt** (`internal/provider/protocol.go`):
   - The evaluator prompt already injects a `rubricSection` dynamically in `buildEvaluatorPrompt()` when `RubricAxes` is non-empty.
   - The section lists each axis name, its `min_threshold`, and `weight` so the provider knows what to score.
   - No code change is needed for a new axis; the builder iterates the contract's `RubricAxes` slice automatically.

5. **Tests**: add a `mergeEvaluatorReport` table-driven test covering the new axis's pass and fail thresholds.

## Extension Guide: Adding a New Context Mode

Context modes control how much job state is serialized into the leader prompt. Current modes: `full`, `summary`, `minimal`, `auto`.

1. **Normalization** (`internal/orchestrator/planning.go` -- `normalizeContextMode()`):
   - Add a new `case "yourmode":` returning the canonical string.
   - The function is the single canonicalization point; no other place needs to know about raw user input.

2. **Payload builder** (`internal/provider/protocol.go` -- `buildLeaderJobPayload()`):
   - Add a `case "yourmode":` that calls a new `buildYourModePayload(job)` function.
   - Model the builder after `buildSummaryPayload()` (keeps goal, summary, step list with truncation) or `buildMinimalPayload()` (keeps only counts and last step).
   - The builder must set `ContextMode` in the serialized struct to the canonical mode string.

3. **Auto-resolution** (`internal/provider/protocol.go` -- `autoContextMode()`):
   - If the new mode should be selected automatically based on step count, add a threshold range in `autoContextMode()`.
   - Current thresholds: `<10` steps -> `full`; `10-20` -> `summary`; `>20` -> `minimal`.

4. **Documentation comment** in `CreateJobInput.ContextMode` (`internal/orchestrator/service.go`):
   - Update the inline comment (currently `// full | summary | minimal; empty defaults to "full"`) to include the new mode.

5. **Tests**: add a `buildLeaderJobPayload` / `buildYourModePayload` unit test confirming the new mode omits or includes the expected fields.

## Extension Guide: Adding a New Strictness Level

Strictness levels control which step types are required and how the evaluator interprets completion. Current levels: `strict`, `normal`, `lenient`, `auto`.

1. **Normalization** (`internal/orchestrator/planning.go` -- `normalizeStrictnessLevel()`):
   - Add a `case "yourlevel":` returning the canonical string.
   - `auto` is intentionally passed through to `ensurePlanning()`; new levels that are resolved before planning should be handled the same way.

2. **Sprint contract thresholds** (`internal/orchestrator/planning.go` -- `buildSprintContract()`):
   - Add a `case "yourlevel":` inside the `switch level` block.
   - Populate `required []string` (required step types), `thresholdSuccessCnt`, and `thresholdMinSteps` to match the level's semantics.

3. **Verification satisfaction** (`internal/orchestrator/evaluator.go` -- `verificationSatisfiedForLevel()`):
   - Add a `case "yourlevel":` that calls the appropriate `verificationSatisfied*` helper or implements inline logic.
   - For lenient-like levels: delegate to `missingRequiredSteps` with the contract's `RequiredStepTypes`.
   - For strict-like levels: delegate to `verificationSatisfied` which checks for a test worker with artifacts.

4. **Merge override logic** (`internal/orchestrator/evaluator.go` -- `mergeEvaluatorReport()`):
   - If the new level should override the provider's verdict (like `normal` does for rule-passed jobs), add the corresponding guard with an explanatory comment.

5. **Provider filter** (`internal/orchestrator/evaluator.go` -- `filterProviderMissingStepTypes()`):
   - Decide which provider-reported missing step types should be surfaced at the new level; add the corresponding filter logic.

6. **Tests**: add table-driven test rows in the `mergeEvaluatorReport` and `buildSprintContract` test suites for the new level.

7. **Documentation**: update `docs/ARCHITECTURE.md` strictness section and `docs/IMPLEMENTATION_STATUS.md`.

## Test Style

- Prefer focused table-driven tests for validation and routing.
- Use end-to-end mock-provider tests for orchestrator loop behavior.
- When changing provider routing, cover leader, planner, evaluator, executor, and tester paths explicitly.
- When changing chain behavior, cover both happy-path advancement and terminal interruption cases.

## Documentation Rules

When code changes:
- Update architecture docs for lifecycle, control surface, or package-boundary changes.
- Update implementation-status docs for newly implemented or still-missing behavior.
- Update principles docs when a new invariant or non-bypassable operator rule is introduced.
- Do not document spec-only behavior as implemented unless the current code path exists.
