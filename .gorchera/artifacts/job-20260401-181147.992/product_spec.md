# Product Spec

Goal: Create a Go CLI tool (tempconv) that converts temperatures between Celsius and Fahrenheit using -c/-f flags, with unit tests.

## Scope
- CLI flag parsing: -c (input is Celsius, convert to Fahrenheit) and -f (input is Fahrenheit, convert to Celsius)
- Temperature conversion: C->F = C*9/5+32, F->C = (F-32)*5/9
- Formatted output with unit label (e.g. '100.00 F' or '37.78 C')
- Error handling for missing/invalid arguments
- Unit tests in main_test.go

## Non-Goals
- GUI or web interface
- Supporting Kelvin or other temperature scales
- Configuration files or persistent state
- Third-party dependencies beyond Go stdlib

## Proposed Steps
- Step 1: Run `go mod init tempconv` in workspace directory to initialize Go module
- Step 2: Create main.go with conversion functions (CtoF, FtoC), flag parsing using os.Args or flag package, and formatted output
- Step 3: Create main_test.go with table-driven tests covering: normal conversions, zero values, negative temperatures, boiling/freezing points, and invalid input handling
- Step 4: Run `go build` to verify compilation
- Step 5: Run `go test -v` to verify all tests pass
- Step 6: Manual smoke test: run the binary with sample inputs (e.g. `tempconv -c 100`, `tempconv -f 32`)

## Acceptance
