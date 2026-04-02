# Product Spec

Goal: Extract the duplicated `firstNonEmpty(vals ...string) string` helper into `internal/orchestrator/util.go`, remove the duplicate definitions from `service.go` and `evaluator.go`, keep all existing call sites compiling, and verify with build and tests.

## Scope
- `internal/orchestrator/service.go` duplicate helper removal
- `internal/orchestrator/evaluator.go` duplicate helper removal
- new shared helper file `internal/orchestrator/util.go`
- package-level compile integrity for orchestrator call sites
- repository verification via `go build ./...` and `go test ./...`

## Non-Goals
- Changing the behavior or signature of `firstNonEmpty`
- Refactoring unrelated orchestrator utilities or package structure
- Modifying provider, CLI, or workflow semantics outside this helper extraction
- Adding new features beyond the helper deduplication
- Changing evaluator gate, harness, approval, or worker-spawn behavior

## Proposed Steps
- Inspect `internal/orchestrator/service.go` and `internal/orchestrator/evaluator.go` to confirm the duplicated `firstNonEmpty` implementations are identical or compatible.
- Create `internal/orchestrator/util.go` in package `orchestrator` and move a single shared `firstNonEmpty(vals ...string) string` implementation there.
- Remove the duplicate `firstNonEmpty` definitions from `internal/orchestrator/service.go` and `internal/orchestrator/evaluator.go` without altering existing call sites.
- Run formatting if needed so the new file and edited files match repository conventions.
- Run `go build ./...` to confirm the refactor compiles across the repo.
- Run `go test ./...` to verify no regressions.
- Update documentation only if this shared utility extraction changes any documented file layout or conventions in a meaningful way.

## Acceptance
