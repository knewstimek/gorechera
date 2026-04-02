# Product Spec

Goal: Add `Multiply(a, b int) int` to `main.go` and add a corresponding test in `main_test.go`.

## Scope
- Update `main.go` to define `Multiply(a, b int) int`.
- Update `main_test.go` to cover `Multiply` with at least one unit test.
- Verify the change with Go tests in the workspace.

## Non-Goals
- Refactoring unrelated application code.
- Changing existing behavior outside the new `Multiply` function and its test.
- Adding extra math helpers beyond `Multiply`.
- Introducing new dependencies or build tooling changes.

## Proposed Steps
- Inspect `main.go` and `main_test.go` to understand the current package, functions, and test conventions.
- Add `Multiply(a, b int) int` to `main.go` with logic that returns `a * b`.
- Add a unit test in `main_test.go` that calls `Multiply` and asserts the expected product.
- Run `go test` for the relevant package to verify the new test passes and no existing tests regress.

## Acceptance
