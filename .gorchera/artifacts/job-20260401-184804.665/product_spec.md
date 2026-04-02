# Product Spec

Goal: Add a `Divide(a, b int) (int, error)` function to `main.go` and add tests for it in `main_test.go`.

## Scope
- Modify `main.go` to add `Divide(a, b int) (int, error)`.
- Modify `main_test.go` to add test coverage for normal division.
- Modify `main_test.go` to add test coverage for divide-by-zero behavior.
- Verify the updated Go tests pass in the workspace.

## Non-Goals
- Refactoring unrelated code in the project.
- Changing existing function signatures unrelated to `Divide`.
- Adding features beyond integer division and zero-division error handling.
- Introducing new dependencies or restructuring the package.

## Proposed Steps
- Read `main.go` and `main_test.go` to understand the current package, imports, and test conventions.
- Implement `Divide(a, b int) (int, error)` in `main.go` with a zero-check that returns an error when `b` is zero.
- Add or update tests in `main_test.go` to validate a successful division result.
- Add or update tests in `main_test.go` to validate that dividing by zero returns a non-nil error.
- Run `go test` for the workspace or relevant package and fix any small issues caused by the change.

## Acceptance
