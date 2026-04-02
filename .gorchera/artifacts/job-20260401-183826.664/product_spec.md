# Product Spec

Goal: Add a Multiply(a, b int) int function to main.go and add a corresponding test in main_test.go.

## Scope
- Update main.go to define Multiply(a, b int) int.
- Update main_test.go to add a unit test covering Multiply.
- Verify the implementation by running Go tests in the workspace.

## Non-Goals
- Refactoring unrelated code.
- Changing existing public behavior beyond adding Multiply.
- Adding extra math helpers or broader test coverage not needed for Multiply.
- Modifying files outside main.go and main_test.go unless required by the build.

## Proposed Steps
- Inspect main.go and main_test.go to confirm package name, existing functions, and current test style.
- Add Multiply(a, b int) int to main.go with logic that returns a * b.
- Add a unit test in main_test.go that asserts Multiply returns the expected product for a representative case.
- Run go test ./... from the workspace and confirm the new test passes.
- Do a final check that only the intended files were changed and acceptance criteria are met.

## Acceptance
