# Product Spec

Goal: Add workspace directory validation before job execution in orchestrator Start/StartAsync and MCP start-job handling, then verify with build and tests.

## Scope
- Pre-run workspace directory validation in internal/orchestrator/service.go Start
- Pre-run workspace directory validation in internal/orchestrator/service.go StartAsync
- MCP gorechera_start_job validation in internal/mcp/server.go before StartAsync
- Regression tests for invalid workspace_dir handling
- Relevant docs update for the new validation behavior
- Repository verification with go build ./... and go test ./...

## Non-Goals
- Changing workspace fallback semantics based on workspaceRoot
- Changing evaluator, approval, or runLoop behavior beyond early validation
- Adding broader filesystem validation beyond this workspace_dir existence check
- Changing unrelated MCP tools or job lifecycle APIs
- Skipping required build and test verification

## Proposed Steps
- Inspect the existing Start, StartAsync, and toolStartJob flows to identify the exact post-job-construction validation point and current MCP error surfacing behavior.
- Add a small shared validation helper in internal/orchestrator/service.go or equivalent local logic that checks the resolved job.WorkspaceDir when non-empty with os.Stat and returns a clear error such as "workspace directory does not exist: {path}" before SaveJob or runLoop.
- Apply that validation in both Start and StartAsync after the job struct is created and before any persistence or goroutine launch occurs.
- Update internal/mcp/server.go toolStartJob to validate workspace_dir before calling StartAsync and return a user-friendly MCP-facing error for invalid input.
- Add or extend tests in internal/orchestrator/service_test.go to cover Start and StartAsync rejecting a nonexistent workspace directory and preserving normal behavior for valid paths.
- Add MCP regression coverage, likely via a new internal/mcp/server_test.go, to verify gorechera_start_job rejects an invalid workspace_dir with a friendly error message.
- Update docs under docs/ to note that job creation now fails fast when workspace_dir is invalid.
- Run go build ./... and go test ./... and use the results as the final verification gate.

## Acceptance
