# Product Spec

Goal: Create a Go CLI tool (tempconv) that converts temperatures between Celsius and Fahrenheit with unit flags and unit tests.

## Scope
- CLI flag parsing: -c (input is Celsius) and -f (input is Fahrenheit)
- Temperature conversion: C->F and F->C
- Formatted output of converted value
- Error handling for missing/invalid input
- Unit tests for conversion logic

## Non-Goals
- GUI or web interface
- Support for Kelvin or other units
- Configuration files or persistent state
- Package publishing

## Proposed Steps
- Step 1: Run `go mod init tempconv` in workspace to initialize the Go module
- Step 2: Create main.go with flag parsing (-c, -f), conversion functions (CtoF, FtoC), and formatted output (e.g. `32.00 F = 0.00 C`)
- Step 3: Create main_test.go with table-driven tests covering: normal values (100C=212F, 32F=0C), zero, negative numbers, and edge cases
- Step 4: Run `go build` to verify compilation
- Step 5: Run `go test -v` to verify all tests pass
- Step 6: Manual smoke test: run the binary with sample inputs (e.g. `tempconv -c 100`, `tempconv -f 32`)

## Acceptance
