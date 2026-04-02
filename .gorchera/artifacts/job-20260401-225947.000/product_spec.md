# Product Spec

Goal: Add a loop-based `Power(base, exp int) int` function in `main.go` and tests in `main_test.go` for the specified cases.

## Scope
- Add `Power(base, exp int) int` to `main.go`.
- Implement exponentiation using iterative multiplication in a loop.
- Return `1` when `exp` is `0`.
- Add unit tests in `main_test.go` for `Power(2,3)==8`, `Power(5,0)==1`, and `Power(3,1)==3`.
- Verify the implementation with Go tests.

## Non-Goals
- Using `math.Pow` or floating-point arithmetic.
- Changing unrelated application behavior or refactoring unrelated code.
- Adding support for negative exponents unless already required by existing code.
- Introducing new files beyond the necessary updates to `main.go` and `main_test.go`.

## Proposed Steps
- Inspect `main.go` and `main_test.go` to confirm current package structure and existing test style.
- Add `Power(base, exp int) int` to `main.go` using a loop that multiplies an accumulator initialized to `1`, repeating `exp` times.
- Ensure the zero-exponent case naturally returns `1` via the accumulator logic.
- Add unit tests in `main_test.go` covering the three required cases.
- Run `go test` for the package and fix any compile or test issues if they arise.

## Acceptance
