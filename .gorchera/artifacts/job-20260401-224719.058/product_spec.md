# Product Spec

Goal: Clarify prompt role hierarchy text in `internal/provider/protocol.go` for leader, worker, planner, and evaluator prompts, then verify with Go build and tests.

## Scope
- Modify `buildLeaderPrompt` opening instruction text in `internal/provider/protocol.go`.
- Modify `buildWorkerPrompt` opening instruction text in `internal/provider/protocol.go`.
- Modify `buildPlannerPrompt` opening instruction text in `internal/provider/protocol.go`.
- Modify `buildEvaluatorPrompt` opening instruction text in `internal/provider/protocol.go`.
- Keep prompt wording aligned with existing orchestrator role boundaries and principles.
- Run repository verification with `go build ./...` and `go test ./...`.
- Update relevant `docs/` content only if current documentation explicitly describes these prompt role semantics and would otherwise become inconsistent.

## Non-Goals
- No changes to orchestration state machine behavior or task routing.
- No changes to provider interfaces, schemas, or adapter transport logic beyond prompt text.
- No implementation of Claude adapter work or other roadmap items.
- No bypass of evaluator gate, approval flow, or harness semantics.
- No broad prompt rewrites outside the targeted role-hierarchy introductions.

## Proposed Steps
- Inspect `internal/provider/protocol.go` to locate `buildLeaderPrompt`, `buildWorkerPrompt`, `buildPlannerPrompt`, and `buildEvaluatorPrompt` and confirm current opening text patterns.
- Revise `buildLeaderPrompt` to use the explicit supervisor/leader/worker hierarchy statement provided in the job goal, preserving surrounding prompt structure.
- Revise `buildWorkerPrompt` to state executor-worker responsibility clearly, including accurate reporting of files changed, commands run, and errors encountered.
- Add parallel hierarchy-aware opening instructions to `buildPlannerPrompt` and `buildEvaluatorPrompt` that match their existing responsibilities and the orchestrator-supervisor model.
- Review nearby prompt-building code for wording consistency and ensure no conflicting legacy text remains.
- Check whether any documentation in `docs/` explicitly describes these role prompts; update only the directly affected references if needed to keep docs aligned with the code change.
- Run `go build ./...` from the workspace root and resolve any compile issues.
- Run `go test ./...` from the workspace root and confirm all tests pass.

## Acceptance
