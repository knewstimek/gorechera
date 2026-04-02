# Product Spec

Goal: Create a Go CLI tool (tempconv) that converts temperatures between Celsius and Fahrenheit using -c/-f flags, with unit tests.

## Scope
- Temperature conversion: Celsius to Fahrenheit and Fahrenheit to Celsius
- CLI flags: -c (input is Celsius) and -f (input is Fahrenheit)
- Formatted output of converted temperature value
- Unit tests for conversion functions and CLI behavior

## Non-Goals
- GUI or web interface
- Support for Kelvin or other temperature scales
- Configuration files or persistent settings
- Interactive/REPL mode

## Proposed Steps
- Step 1: Run `go mod init tempconv` in the workspace directory to initialize the Go module
- Step 2: Create convert.go with CtoF(c float64) float64 and FtoC(f float64) float64 functions
- Step 3: Create main.go with flag parsing (-c and -f flags), input validation, and formatted output (e.g., `32.00 F` or `0.00 C`)
- Step 4: Create convert_test.go with table-driven tests covering: 0C->32F, 100C->212F, -40C->-40F, 32F->0C, 212F->100C, -40F->-40C
- Step 5: Run `go build` to verify compilation
- Step 6: Run `go test -v` to verify all tests pass
- Step 7: Run the built binary with sample inputs to verify CLI behavior end-to-end

## Acceptance
