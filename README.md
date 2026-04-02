# Gorchera

Go stateful multi-agent orchestration engine / harness engineering with self-improvement capabilities.

**Gorchera** (Go + Orchestra) coordinates AI agents (GPT, Claude) to plan, implement, review, test, and evaluate software tasks autonomously. A supervisor agent (e.g., Claude Opus via MCP) monitors and steers the workflow. Inspired by harness engineering principles from Anthropic's multi-agent orchestration research.

## Features

- **4-role pipeline**: director -> executor -> [engine build/test] -> reviewer -> evaluator
- **3 strictness levels**: strict (implement+review+test), normal (implement only), lenient (results only)
- **3 context modes**: full, summary, minimal -- controls director prompt payload size
- **Job chaining**: sequential multi-goal execution with automatic advancement
- **Chain-level controls**: pause, resume, cancel, skip individual chain goals
- **Supervisor steering**: mid-flight directive injection via `gorchera_steer`
- **Provider adapters**: GPT/Codex, Claude, Mock (testing) -- with per-role model/provider selection
- **Role overrides**: per-job provider/model overrides from MCP (e.g., evaluator=opus, executor=sonnet)
- **Synchronous wait**: block on job/chain status with configurable timeout
- **Self-improvement**: Gorchera can modify its own codebase via orchestrated jobs
- **Error classification**: 12 error types with 3-strike retry policy
- **Token tracking**: rough per-job and per-step token/cost estimation
- **Security**: SUPERVISOR injection prevention, workspace validation, steer authorization
- **MCP server**: 17+ tools for supervisor agent integration (stdio JSON-RPC 2.0)

## Quick Start

```bash
go build ./...
go run ./cmd/gorchera mcp          # Start MCP server for Claude Code integration
go run ./cmd/gorchera run -goal "Add a hello function" -provider codex
go run ./cmd/gorchera status -all
```

## Pipeline Modes

Gorchera supports three pipeline modes to balance quality vs cost:

- **light** (default): director -> executor -> engine build/test -> evaluator. Fastest and cheapest. Skip reviewer. Good for simple changes.
- **balanced**: Adds reviewer before evaluator. Good for moderate changes where code review catches bugs.
- **full**: Adds fix loops and parallel workers. For complex, risky changes.

Light mode with cross-provider (director=GPT, executor=Claude Sonnet) costs ~$0.04 per job.

## MCP Tools

| Tool | Description |
|------|-------------|
| `gorchera_start_job` | Start a single job (with optional role_overrides) |
| `gorchera_start_chain` | Start sequential job chain |
| `gorchera_status` | Get job status (wait=true for sync blocking) |
| `gorchera_chain_status` | Get chain status (wait=true for sync blocking) |
| `gorchera_steer` | Inject supervisor directive |
| `gorchera_events` | Get job events |
| `gorchera_artifacts` | Get job artifacts |
| `gorchera_approve` | Approve blocked action |
| `gorchera_reject` | Reject blocked action |
| `gorchera_retry` | Retry failed job |
| `gorchera_cancel` | Cancel running job |
| `gorchera_resume` | Resume blocked job |
| `gorchera_list_jobs` | List all jobs |
| `gorchera_pause_chain` | Pause chain after current goal |
| `gorchera_resume_chain` | Resume paused chain |
| `gorchera_cancel_chain` | Cancel entire chain |
| `gorchera_skip_chain_goal` | Skip current chain goal |

## Architecture

See [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md) for package structure, state machine, and core loop.

## Supervisor Guidelines

When using Gorchera with an AI supervisor (e.g., Claude Opus via MCP), the supervisor must follow these rules:

- **Never write code directly.** All code changes must go through `gorchera_start_job` or `gorchera_start_chain`. The supervisor writes goals, not code.
- **Never diagnose or investigate directly.** Spawning sub-agents to explore the codebase is still "doing it directly." Create a Gorchera audit/review job instead.
- **Steer, don't intervene.** If a job goes off track, use `gorchera_steer` to redirect. If it fails, use `gorchera_retry` or start a new fix job.
- **Reading code is allowed only for goal formulation.** The supervisor may read interfaces and types to write better goals, but not to debug or fix issues.
- **Monitor via status polling.** Use `gorchera_status` / `gorchera_chain_status` to track progress. Use `wait=true` for synchronous blocking.

This separation ensures that all work is auditable, artifact-tracked, and evaluator-gated -- the core value proposition of using an orchestration engine over direct AI coding.

For the full guide on goal writing, ambition levels, invariants, provider presets, and operational tips, see [docs/SUPERVISOR_GUIDE.md](./docs/SUPERVISOR_GUIDE.md).

## Documentation

1. [ARCHITECTURE.md](./docs/ARCHITECTURE.md) -- package structure, state machine, core loop
2. [IMPLEMENTATION_STATUS.md](./docs/IMPLEMENTATION_STATUS.md) -- current state, resolved issues
3. [PRINCIPLES.md](./docs/PRINCIPLES.md) -- inviolable design principles
4. [CODING_CONVENTIONS.md](./docs/CODING_CONVENTIONS.md) -- coding rules, extension guides
5. [SUPERVISOR_GUIDE.md](./docs/SUPERVISOR_GUIDE.md) -- goal writing, ambition levels, invariants, operational tips
6. [ORCHESTRATOR_SPEC_UPDATED.md](./docs/ORCHESTRATOR_SPEC_UPDATED.md) -- detailed design spec
7. [BLOG_COMPARISON.md](./docs/BLOG_COMPARISON.md) -- comparison with Anthropic's harness engineering blog

## License

MIT
