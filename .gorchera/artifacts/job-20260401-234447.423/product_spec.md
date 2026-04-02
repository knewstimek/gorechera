# Product Spec

Goal: Add per-role model selection support to the Codex adapter and verify it with build/test.

## Scope
- Codex adapter argument construction in `internal/provider/codex.go`
- Role-profile model propagation for Codex leader/planner/evaluator/worker execution
- A helper `isCodexModel(model string) bool` to gate Codex `--model` usage
- Provider tests covering `--model` inclusion and exclusion behavior
- Documentation updates reflecting Codex per-role model support
- Repository verification via `go build ./...` and `go test ./...`

## Non-Goals
- Changing Claude adapter behavior beyond preserving compatibility
- Adding new provider interface methods or redesigning session routing unless required for the minimal fix
- Changing default role profile values in `internal/domain/types.go`
- Expanding provider fallback, billing, auth, or retry behavior
- Implementing broader model alias normalization outside the Codex gating needed for this task

## Proposed Steps
- Inspect `internal/provider/codex.go`, `internal/provider/claude.go`, and current provider tests to mirror the existing Claude profile flow with the smallest Codex-only change surface.
- Modify Codex `RunLeader`, `RunPlanner`, `RunEvaluator`, and `RunWorker` so each resolves the correct role profile from `job.RoleProfiles` and passes it into `runStructured` instead of calling `runStructured` without model context.
- Extend Codex `runStructured` to accept the resolved profile or model string, add `isCodexModel(model string) bool`, and append `--model <name>` only when the model is non-empty and allowed for Codex; suppress the flag for Claude shorthand models such as `opus`, `sonnet`, and `haiku`.
- Keep the implementation cross-platform and localized to provider code; avoid mixing harness semantics or changing unrelated orchestration behavior.
- Add or update unit tests in `internal/provider/provider_test.go` to assert that Codex receives `--model` for GPT-style models, omits it for empty model strings, and omits it for Claude shorthand role models routed through the Codex adapter.
- Update the relevant docs, at minimum the implementation status entry for the Codex adapter, so the documented behavior matches the new per-role model-selection support.
- Run `go build ./...` and `go test ./...` and capture the pass/fail result as final verification evidence.

## Acceptance
