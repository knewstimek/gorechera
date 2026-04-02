# Product Spec

Goal: Add rough token and cost tracking to orchestrator job and step state, with accumulation after each provider call and build/test verification.

## Scope
- Add TokenUsage struct to internal/domain/types.go with InputTokens, OutputTokens, TotalTokens, and EstimatedCostUSD.
- Add TokenUsage fields to Job and Step so usage is stored in orchestrator state.
- Implement estimateTokens(input, output string) TokenUsage in internal/orchestrator/service.go.
- Invoke token estimation after RunLeader, RunWorker, RunPlanner, and RunEvaluator responses and accumulate usage on both job and current step.
- Update relevant docs to mention token/cost tracking behavior.
- Run repository verification with go build ./... and go test ./...

## Non-Goals
- Integrating provider-native token accounting or real billing APIs.
- Changing provider interfaces to return exact usage metadata.
- Adding new CLI/reporting surfaces for token usage unless already required by existing state serialization.
- Reworking orchestrator flow beyond the minimal changes needed for accounting.
- Addressing unrelated bugs or refactors outside the touched token-tracking path.

## Proposed Steps
- Inspect internal/domain/types.go and internal/orchestrator/service.go to confirm current Job, Step, and provider-call flow.
- Add TokenUsage to the domain types and embed it into Job and Step with names consistent with existing serialization/state patterns.
- Implement a small estimation helper in the orchestrator service that converts input/output character lengths into rough token counts and derives total tokens and estimated USD cost.
- Wire the helper into each provider call site (RunLeader, RunWorker, RunPlanner, RunEvaluator), capturing the prompt/input text and returned output text, then accumulate the result onto the current step and overall job.
- Update the most relevant docs under docs/ to describe the new token/cost tracking behavior and estimation caveat.
- Run go build ./... and go test ./... and fix any compile or test regressions caused by the new fields or accounting logic.

## Acceptance
