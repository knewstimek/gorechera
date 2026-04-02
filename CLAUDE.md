# CLAUDE.md

Gorchera -- Go stateful multi-agent orchestration engine (workflow engine, not a chatbot).

## Build & Verify

```bash
go build ./...
go test ./...
```

## Core Principles (`docs/PRINCIPLES.md`)

- Evaluator gate: never bypass -- complete must pass evaluateCompletion()
- No full conversation logs between agents -- artifact + summary only
- Executor must not spawn workers -- parallelism is orchestrator-driven
- Approval-required actions must not auto-pass
- No Windows-specific code in core -- cross-platform neutral
- ASCII only in code/output -- cp949 breaks on non-ASCII
- Comment "why", not "what"

## Architecture

- **4-role pipeline**: director -> executor -> [engine build/test] -> reviewer -> evaluator
- **pipeline_mode**: light (default, skip reviewer) / balanced / full
- **director** = planner + leader merged; engine = rule-based go build/test
- Context compaction: executor/reviewer/evaluator get compact payloads
- Cross-provider: role_overrides on start_job (e.g. director=codex, executor=claude)

## Recommended Profile

provider=codex, executor/reviewer=claude sonnet. Result: GPT plans, Claude executes. ~$0.04/job light mode.

## Docs

1. `docs/ARCHITECTURE.md` -- packages, state machine, core loop
2. `docs/IMPLEMENTATION_STATUS.md` -- current state, resolved issues
3. `docs/PRINCIPLES.md` -- inviolable design principles
4. `docs/CODING_CONVENTIONS.md` -- coding rules, extension guides
5. `docs/SUPERVISOR_GUIDE.md` -- goal writing, ambition levels, invariants, operational tips
6. `docs/ORCHESTRATOR_SPEC_UPDATED.md` -- detailed design spec

## Code Entry Points

- `cmd/gorchera/main.go` -- CLI
- `internal/orchestrator/service.go` -- core loop
- `internal/provider/provider.go` -- adapter interface
- `internal/provider/protocol.go` -- prompts/schemas

## Rules

- `go build ./...` && `go test ./...` after every code change
- Update `docs/` when changing behavior
- New features: see `docs/CODING_CONVENTIONS.md` extension guide
- Comment intent on non-obvious logic ("why this way")
