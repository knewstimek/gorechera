# Product Spec

Goal: Create a Go CLI tool (tempconv) that converts temperatures between Celsius and Fahrenheit using -c/-f flags, with unit tests.

## Scope
- CLI flag parsing: -c (Celsius to Fahrenheit) and -f (Fahrenheit to Celsius)
- Temperature conversion with standard formulas
- Formatted output of converted value
- Unit tests in main_test.go with table-driven tests

## Non-Goals
- GUI or web interface
- Kelvin or other temperature scales
- Interactive/REPL mode
- Configuration files

## Proposed Steps
- Step 1: Run `go mod init tempconv` in the workspace directory to initialize the Go module
- Step 2: Create main.go with: (a) conversion functions CtoF and FtoC, (b) flag parsing for -c and -f flags accepting float64, (c) main function that validates exactly one flag is provided, converts, and prints result
- Step 3: Create main_test.go with table-driven tests for CtoF and FtoC functions covering: 0C=32F, 100C=212F, -40C=-40F, 32F=0C, 212F=100C, negative values
- Step 4: Run `go build` to verify compilation
- Step 5: Run `go test -v` to verify all tests pass

## Acceptance
