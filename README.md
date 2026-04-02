# Gorchera

Go stateful multi-agent orchestration engine / harness engineering with self-improvement capabilities.

**Gorchera** (Go + Orchestra) coordinates AI agents (GPT, Claude) to plan, implement, review, test, and evaluate software tasks autonomously. A supervisor agent (e.g., Claude Opus via MCP) monitors and steers the workflow. Inspired by harness engineering principles from Anthropic's multi-agent orchestration research.

## Features

- **6-role pipeline**: planner -> leader -> executor/reviewer/tester -> evaluator
- **3 strictness levels**: strict (implement+review+test), normal (implement only), lenient (results only)
- **3 context modes**: full, summary, minimal -- controls leader prompt payload size
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

## Documentation

1. [ARCHITECTURE.md](./docs/ARCHITECTURE.md) -- package structure, state machine, core loop
2. [IMPLEMENTATION_STATUS.md](./docs/IMPLEMENTATION_STATUS.md) -- current state, resolved issues
3. [PRINCIPLES.md](./docs/PRINCIPLES.md) -- inviolable design principles
4. [CODING_CONVENTIONS.md](./docs/CODING_CONVENTIONS.md) -- coding rules, extension guides
5. [ORCHESTRATOR_SPEC_UPDATED.md](./docs/ORCHESTRATOR_SPEC_UPDATED.md) -- detailed design spec
6. [BLOG_COMPARISON.md](./docs/BLOG_COMPARISON.md) -- comparison with Anthropic's harness engineering blog

## License

MIT
