# Product Spec

Goal: Add structured worker failure reasons so the supervisor receives machine-parseable error feedback for failed or blocked worker steps.

## Scope
- Add `StructuredReason` to `internal/domain/types.go` and attach it to `Step` as an optional field.
- Implement `classifyWorkerFailure(err error, workerOutput string) *StructuredReason` in `internal/orchestrator/service.go` using the specified pattern rules.
- Populate `Step.StructuredReason` when worker execution ends in failure or blocked status.
- Update `worker_failed` and `worker_blocked` event messages to include structured reason JSON.
- Run `go build ./...` and `go test ./...` after the code changes.
- Update relevant docs if current behavior or data shape documentation is affected by the change.

## Non-Goals
- Changing supervisor logic beyond making the event payload easier to parse.
- Introducing a broader error taxonomy than the requested categories and simple pattern classification.
- Refactoring unrelated orchestrator state-machine behavior.
- Changing evaluator gate semantics, approval handling, harness ownership, or worker spawning rules.
- Adding Windows-specific behavior to core orchestration code.

## Proposed Steps
- Inspect `internal/domain/types.go` and `internal/orchestrator/service.go` to confirm current `Step` shape, worker result handling, and event emission points.
- Add a `StructuredReason` struct with `Category`, `Detail`, and `SuggestedAction` fields, then add `StructuredReason *StructuredReason \`json:\",omitempty\"\`` to `Step`.
- Implement `classifyWorkerFailure(err error, workerOutput string) *StructuredReason` in the orchestrator, using error unwrapping/string checks plus worker output text to detect build, test, file access, timeout, and schema violations.
- Wire classification into worker failure and blocked handling so the current step captures the structured reason whenever classification succeeds.
- Update `addEvent` calls for `worker_failed` and `worker_blocked` to embed the structured reason as JSON in the message while preserving enough human-readable context for logs.
- Review whether any docs describing step/event payloads need adjustment and update them if necessary.
- Run `go build ./...` and `go test ./...`, then fix any regressions caused by the new field or event formatting.

## Acceptance
