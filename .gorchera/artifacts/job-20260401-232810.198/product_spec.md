# Product Spec

Goal: Add sequential job-chain orchestration so a supervisor can submit multiple goals and have each next job start automatically after the previous job finishes successfully.

## Scope
- Add `JobChain` and `ChainGoal` domain models in `internal/domain/types.go`.
- Persist chains in the file-backed state store with save/load/list behavior parallel to jobs.
- Add orchestrator chain lifecycle methods: `StartChain`, `advanceChain`, `GetChain`, and `ListChains`.
- Start the first chained job immediately and record per-goal `JobID` and status transitions.
- Advance to the next goal only when the active chained job reaches `done`.
- Mark the chain terminal state correctly when the last goal completes or when an active chained job cannot continue successfully.
- Expose `gorechera_start_chain` and `gorechera_chain_status` in the MCP server.
- Add or extend automated tests covering chain persistence, service behavior, and MCP tool behavior.
- Update docs describing the new chain model and MCP tools.

## Non-Goals
- Parallel execution of chain goals.
- General DAG dependencies, branching, retries, or conditional chain logic.
- Changing evaluator-gate semantics for individual jobs.
- Adding new HTTP routes or CLI commands unless required by existing tests or plumbing.
- Cross-process distributed locking beyond the current file-store/single-service model.
- Changing worker, reviewer, or tester role contracts outside what chaining needs.

## Proposed Steps
- Inspect current job creation, terminal-status handling, and MCP tool patterns to place chain hooks without bypassing existing evaluator-gated completion.
- Add `JobChain` and `ChainGoal` to `internal/domain/types.go`, and add minimal chain linkage metadata to job state only if needed to identify the owning chain cleanly from completion/failure paths.
- Extend `internal/store/state_store.go` with `SaveChain`, `LoadChain`, and `ListChains`, storing chain JSON under a dedicated `chains` directory and preserving the existing atomic-write pattern.
- Implement `StartChain` in `internal/orchestrator/service.go` to validate input, create a chain record with pending goals, start goal 0 as a normal job, mark that goal running with its `JobID`, and persist the updated chain.
- Implement `GetChain` and `ListChains` as thin service wrappers over the state store.
- Implement `advanceChain` so it loads the current chain state, marks the completed goal done, increments `CurrentIndex`, starts the next job when one remains, or marks the chain done when the final goal finishes.
- Wire chain progression into job terminal handling after evaluator-approved `done`, and also update chain state when a chained job ends blocked/failed so the chain does not remain incorrectly `running`.
- Add MCP tool definitions, dispatch cases, argument parsing, and result serialization for `gorechera_start_chain` and `gorechera_chain_status` in `internal/mcp/server.go`.
- Add tests for state-store chain persistence, service-level sequential advancement and stop-on-failure behavior, and MCP start/status tool responses.
- Update `docs/ARCHITECTURE.md` and `docs/IMPLEMENTATION_STATUS.md` to document chain persistence, orchestrator flow changes, and MCP tool availability.
- Run `go build ./...` and `go test ./...` and use failures to tighten any missing plumbing or schema mismatches.

## Acceptance
