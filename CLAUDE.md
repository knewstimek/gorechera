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

- **3-agent pipeline**: director -> executor -> [engine build/test] -> evaluator
- **pipeline_mode**: light (QUICK eval) / balanced (THOROUGH eval) / full (EXHAUSTIVE eval)
- **director** = planner + leader merged; engine = rule-based go build/test
- Context compaction: executor/evaluator get compact payloads
- Cross-provider: role_overrides on start_job (e.g. director=codex, executor=claude)

## Recommended Profile

provider=codex, executor=claude sonnet. Result: GPT plans, Claude executes. ~$0.04/job light mode.

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

## Audit: Orchestrator-Specific Checks

/audit 실행 시 글로벌 스킬 항목 외에 추가로 확인할 것:
- **Evaluator gate bypass** -- evaluateCompletion() 없이 done 처리되는 경로가 없는지
- **Approval policy bypass** -- PendingApproval 상태에서 승인 없이 위험 작업 실행되는 경로가 없는지
