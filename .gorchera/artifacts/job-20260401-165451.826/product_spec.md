# Product Spec

Goal: List the files in the current directory and summarize the project structure

## Scope
- Enumerate all files and directories in the workspace root
- Recursively list the project tree structure
- Summarize the purpose of each top-level directory and key files
- Identify the package layout and architectural organization

## Non-Goals
- Do not modify any files
- Do not build or test the project
- Do not analyze code logic or find bugs

## Proposed Steps
- Step 1: List root directory contents (AGENTS.md, CLAUDE.md, README.md, Old/, cmd/, docs/, examples/, go.mod, gorechera.exe, internal/)
- Step 2: Recursively list all files under cmd/, internal/, docs/, examples/ to build a complete file tree
- Step 3: Read key files (go.mod, README.md, docs/ARCHITECTURE.md) to understand module name, dependencies, and architectural overview
- Step 4: Produce a structured summary: root files, cmd/gorechera/main.go (CLI entry), internal/ packages (api, domain, orchestrator, policy, provider, runtime, schema, store), docs/ (6 markdown files), examples/ (sample config)

## Acceptance
