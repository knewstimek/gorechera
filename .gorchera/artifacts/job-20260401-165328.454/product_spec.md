# Product Spec

Goal: List the files in the current directory and summarize the project structure

## Scope
- Enumerate all files/directories in the workspace root
- Identify Go package structure under cmd/ and internal/
- Summarize each package's responsibility in 1-2 sentences
- Note key entry points (main.go, service.go, provider.go)
- List documentation files and their topics
- Report artifact/state storage layout under .gorechera/

## Non-Goals
- No code modifications
- No build or test execution
- No dependency analysis beyond go.mod

## Proposed Steps
- Step 1 (executor): List all files recursively (excluding .git), group by top-level directory, and produce a tree-style summary
- Step 2 (executor): Read package doc comments or first lines of key .go files to summarize each package's role: cmd/gorechera, internal/api, internal/domain, internal/orchestrator, internal/policy, internal/provider, internal/runtime, internal/schema, internal/store
- Step 3 (executor): Compile final summary artifact with: (a) directory tree, (b) package responsibility table, (c) entry points, (d) doc index

## Acceptance
