# Product Spec

Goal: Plan a read-only audit of the recent self-improvement changes across the targeted orchestrator, provider, and MCP paths, ending in a worker summary of findings.

## Scope
- Audit [internal/orchestrator/service.go](D:\News\Business\AIOrchestrator\internal\orchestrator\service.go) with emphasis on `classifyWorkerFailure`, `consecutiveSummarizes`, workspace validation, and `completionRetryPending`.
- Audit [internal/provider/protocol.go](D:\News\Business\AIOrchestrator\internal\provider\protocol.go) with emphasis on `buildLeaderJobPayload`, summary/minimal payload modes, and the `[SUPERVISOR]` rule in leader prompting.
- Audit [internal/mcp/server.go](D:\News\Business\AIOrchestrator\internal\mcp\server.go) with emphasis on `validateWorkspaceDir`, `toolSteer`, and `gorechera_steer` behavior.
- Inspect directly related helper logic needed to validate the focused behaviors, especially evaluator completion-gate handling and step-status semantics.
- Produce a worker summary covering logical errors or edge cases, security concerns, dead code or unused functions, and inconsistencies between code paths.

## Non-Goals
- Do not modify any files.
- Do not implement fixes, refactors, or documentation updates.
- Do not broaden the audit into unrelated packages beyond immediate helper paths needed to verify the named logic.
- Do not run build/test solely for this review unless needed to confirm a claimed issue, since the task is analysis-only.
- Do not create new artifacts beyond the worker summary content.

## Proposed Steps
- Read the targeted regions in `internal/orchestrator/service.go` and trace how leader completion, summarize capping, blocked evaluation retries, worker-failure classification, and workspace validation affect persisted job state.
- Read the targeted regions in `internal/provider/protocol.go` and compare full, summary, and minimal leader payload construction, paying attention to whether prompt rules still match what the leader can actually observe in reduced-context modes.
- Read the targeted regions in `internal/mcp/server.go` and compare MCP-side workspace validation and steering behavior against orchestrator-side behavior, including request handling and response semantics.
- Cross-check immediate helper code that governs the same flows, especially evaluator completion handling and domain step statuses, to confirm whether suspected edge cases are real or only apparent.
- Write a final worker summary with concrete findings grouped into: logical/edge-case bugs, security concerns, dead code or unused paths, and cross-path inconsistencies; include exact file references and explicitly state any category with no findings.

## Acceptance
