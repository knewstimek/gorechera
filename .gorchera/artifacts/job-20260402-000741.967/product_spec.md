# Product Spec

Goal: Rename all in-file project references from gorechera/Gorechera to gorchera/Gorchera across the repository without renaming any filesystem paths, then verify the Go project still builds and tests.

## Scope
- Update module name in go.mod from gorechera to gorchera.
- Replace import path prefix gorechera/ with gorchera/ in all .go files.
- Replace case-sensitive string occurrences gorechera->gorchera and Gorechera->Gorchera in all .go, .md, and .json files.
- Update .gorechera content references to .gorchera in code, including state/artifact store paths.
- Update project documentation and repo metadata files including CLAUDE.md, AGENTS.md, README.md, docs/*.md, and .mcp.json.
- Keep existing filesystem paths such as cmd/gorechera/main.go in place while updating only file contents.
- Build and test validation with go build ./... and go test ./... .

## Non-Goals
- Do not rename directories or files on disk, including cmd/gorechera or any real .gorechera directory.
- Do not change package structure, runtime behavior, APIs, or orchestrator semantics beyond the textual rename.
- Do not introduce unrelated refactors, formatting-only churn, or dependency changes.
- Do not modify binary/generated artifacts unless they are tracked text files within the stated extensions and scope.
- Do not skip documentation/config updates that are explicitly listed in scope.

## Proposed Steps
- Scan the repository for in-scope occurrences of gorechera/Gorechera in go.mod, .go, .md, .json, docs, CLAUDE.md, AGENTS.md, README.md, .mcp.json, and targeted .gorechera path references.
- Update go.mod module declaration from gorechera to gorchera.
- Apply content-only replacements across all .go files: change import prefix gorechera/ to gorchera/ and replace remaining gorechera/Gorechera string literals where applicable.
- Apply content-only replacements across all .md and .json files, including CLAUDE.md, AGENTS.md, README.md, docs/*.md, and .mcp.json.
- Specifically verify code references to .gorechera state/artifact storage paths are changed to .gorchera without renaming any actual directories.
- Run a residual search to confirm no in-scope gorechera/Gorechera content remains, excluding intentional filesystem path names that are not to be renamed.
- Run go build ./... and go test ./... and resolve any rename-induced breakage until both pass.
- Summarize changed areas and verification evidence for reviewer/tester handoff.

## Acceptance
