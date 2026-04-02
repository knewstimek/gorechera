# Product Spec

Goal: Add `Subtract(a, b int) int` to `main.go` and add a corresponding test in `main_test.go`.

## Scope
- Modify `main.go` to add `Subtract(a, b int) int`.
- Modify `main_test.go` to add a unit test for `Subtract`.
- Run Go tests relevant to the changed package.

## Non-Goals
- Refactoring unrelated code.
- Changing existing function behavior beyond adding `Subtract`.
- Adding new packages, modules, or dependencies.
- Expanding test coverage beyond the new `Subtract` function unless required for compilation.

## Proposed Steps
- Inspect `main.go` and `main_test.go` to understand current package and test structure.
- Add `Subtract(a, b int) int` to `main.go` with implementation `return a - b`.
- Add a unit test in `main_test.go` that calls `Subtract` and checks the expected result.
- Run `go test` for the package and confirm all tests pass.
- Review the diff to ensure only the intended files and changes are included.

## Acceptance
