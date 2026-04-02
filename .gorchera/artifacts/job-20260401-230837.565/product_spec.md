# Product Spec

Goal: Fix the 5 audited medium-severity defects in orchestrator state handling, worker-failure classification, context payload generation, UTF-8-safe summary truncation, and parallel worker failure flow, then verify with Go build and tests.

## Scope
- `internal/orchestrator/service.go` completion-retry summarize behavior and worker-failure classification
- `internal/provider/protocol.go` minimal and summary context payload correctness
- `internal/orchestrator/parallel.go` parallel worker failure state transition behavior
- Regression coverage in existing orchestrator/provider test files
- Documentation updates for the resolved audit findings

## Non-Goals
- No new orchestrator actions, statuses, or features
- No changes to approval semantics, evaluator gate rules, or provider interfaces beyond the requested fixes
- No broad refactor of run loop, payload builders, or parallel execution architecture
- No unrelated bug fixes outside the 5 listed audit findings

## Proposed Steps
- Inspect the current implementations around `runLoop`, `classifyWorkerFailure`, `buildMinimalPayload`, `buildSummaryPayload`, and `runParallelWorkerPlans` to confirm exact existing behavior and nearby test coverage.
- Patch `internal/orchestrator/service.go` so the `summarize` action clears `job.BlockedReason` when `completionRetryPending` is active, and so the early `complete` return with no new steps performs `s.touch(job)` and `s.state.SaveJob(...)` before returning.
- Expand `classifyWorkerFailure` in `internal/orchestrator/service.go` to classify field-level validator output as schema violations when messages contain `is required`, `invalid`, or `validation failed`, while preserving existing timeout, file-access, test, and build classification precedence.
- Patch `internal/provider/protocol.go` so minimal mode counts `active`, `blocked`, and `failed` separately, counting only empty-status or `active` steps as active, and so summary-mode truncation uses rune-based slicing before appending ellipsis.
- Patch `internal/orchestrator/parallel.go` so a parallel worker result with status `failed` marks that step failed, records failure context for the leader, and leaves the job in a leader-resumable state instead of escalating the whole job to `failed`; keep blocked behavior unchanged.
- Add focused regression tests covering: summarize-after-completion-retry persistence, broader schema-failure detection, minimal payload blocked counting, UTF-8-safe summary truncation, and parallel failed-worker control returning to the leader.
- Run `gofmt` on modified Go files, update `docs/IMPLEMENTATION_STATUS.md` and any behavior-facing architecture note needed by the changed state flow, then run `go build ./...` and `go test ./...`.

## Acceptance
