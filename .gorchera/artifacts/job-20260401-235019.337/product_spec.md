# Product Spec

Goal: Add per-step artifact diff tracking so successful worker steps capture a git diff summary in orchestrator state.

## Scope
- Add `Step.DiffSummary string` to the domain type definition in `internal/domain/types.go`.
- Update `internal/orchestrator/service.go` so the orchestrator computes and stores a diff summary only after a worker step succeeds.
- Use `git diff --stat` against the workspace directory when git is available and the workspace is a git repository.
- Gracefully fall back to an empty `DiffSummary` when git is unavailable or the workspace is not a git repository.
- Update relevant `docs/` content to reflect the new step metadata and behavior.
- Verify the change with `go build ./...` and `go test ./...`.

## Non-Goals
- Implementing full patch capture, file-level snapshots, or persistent artifact storage beyond a summary string.
- Changing evaluator gate behavior, approval semantics, or worker orchestration flow outside the success-path metadata update.
- Adding Windows-specific logic to core orchestration behavior.
- Introducing worker-side diff generation; this remains orchestrator-owned.
- Refactoring unrelated step/domain structures beyond what is necessary for `DiffSummary`.
- Altering git repository state or requiring git initialization for non-repo workspaces.

## Proposed Steps
- Inspect the current `Step` domain model and the worker-step success flow to identify where step results are finalized and persisted.
- Add `DiffSummary string` to `internal/domain/types.go` and ensure any constructors, copies, or serialization paths continue to behave correctly.
- Implement a small orchestrator-side helper in `internal/orchestrator/service.go` that runs `git diff --stat` in the workspace, returning `""` on non-repo or git-not-found conditions without failing the step.
- Invoke that helper only after a worker step has completed successfully and assign the returned value to `Step.DiffSummary` before the updated step state is stored or emitted.
- Update the relevant documentation under `docs/` so the new per-step diff visibility is described consistently with current architecture/status docs.
- Run `go build ./...` and `go test ./...`, then review outputs to confirm the feature integrates cleanly without regressions.

## Acceptance
