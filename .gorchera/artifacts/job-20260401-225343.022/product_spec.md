# Product Spec

Goal: Cap repeated leader summarize actions and strengthen the leader prompt so summarize cannot be used as a completion substitute.

## Scope
- Modify `internal/orchestrator/service.go` `runLoop` logic to track and cap consecutive summarize actions.
- Preserve the existing `evaluateCompletion()` gate by forcing capped summarize behavior through the completion path rather than bypassing evaluation.
- Log a `leader_summarize_capped` event when the summarize cap is triggered.
- Update `internal/provider/protocol.go` `buildLeaderPrompt` text with the new summarize/complete rule.
- Run repository-level verification with `go build ./...` and `go test ./...`.

## Non-Goals
- Changing evaluator semantics beyond forcing capped summarize into the existing completion evaluation path.
- Altering unrelated leader action handling, worker dispatch flow, or review/test orchestration behavior.
- Adding new provider protocol structures or changing schema formats outside the prompt text update.
- Refactoring unrelated orchestration loop code.
- Fixing other known bugs or planned Phase 1 Claude adapter work.

## Proposed Steps
- Inspect `internal/orchestrator/service.go` `runLoop` to identify where leader actions are decoded, where summarize is handled, and where completion evaluation is invoked.
- Introduce a `consecutiveSummarizes` counter in `runLoop`, increment it only on allowed summarize actions, and reset it to `0` on every non-summarize action path that represents worker/system progress or any other leader action.
- When the leader selects `summarize` and `consecutiveSummarizes >= 2`, emit the `leader_summarize_capped` event and branch into the same completion evaluation flow used for `complete` instead of executing another summarize.
- Confirm the forced-complete path still uses `evaluateCompletion()` and does not bypass the evaluator gate or mark the job done directly.
- Update `internal/provider/protocol.go` `buildLeaderPrompt` to include the explicit instruction forbidding summarize as a substitute for complete and forbidding more than one consecutive summarize.
- Review nearby docs/comments if needed for consistency, but keep the change minimal and aligned with existing conventions.
- Run `go build ./...` and `go test ./...`, and address any failures caused by the change.
- Capture the outcome in a concise change summary with the capped summarize behavior, prompt rule update, and verification results.

## Acceptance
