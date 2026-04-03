# Installation Guide

## 1. Prerequisites

### Go

Go 1.26 or later is required.

```
go version
```

Download from https://go.dev/dl/

### External Tools (provider CLIs)

Gorchera delegates AI work to external CLI tools. Install the ones you plan to use.

| Provider     | Binary  | Install reference                        |
|--------------|---------|------------------------------------------|
| Claude (Anthropic) | `claude` | https://docs.anthropic.com/en/docs/claude-code |
| Codex (OpenAI)     | `codex`  | https://github.com/openai/codex          |

Both are optional -- only the provider you specify per job needs to be present.
The binary names can be overridden via environment variables (see Section 3).

---

## 2. Build

Clone the repository, then build from the repo root:

```bash
go build -o gorchera ./cmd/gorchera
```

Verify:

```bash
./gorchera
# Usage: gorchera <run|status|events|...> [flags]
```

Run tests:

```bash
go test ./...
```

No C toolchain or CGO is required. The binary is fully self-contained.

---

## 3. Configuration

All configuration is done through environment variables. No config file is required.

### Provider API Keys (required per provider)

| Variable           | Provider       | Required when            |
|--------------------|----------------|--------------------------|
| `ANTHROPIC_API_KEY` | Claude CLI    | Using `--provider claude` |
| `OPENAI_API_KEY`    | Codex CLI     | Using `--provider codex`  |
| `OPENAI_ORG_ID`     | Codex CLI     | Optional; org routing     |
| `OPENAI_BASE_URL`   | Codex CLI     | Optional; custom endpoint |
| `ANTHROPIC_BASE_URL`| Claude CLI    | Optional; custom endpoint |

These are passed to provider subprocesses through an allowlist -- other shell
variables are NOT forwarded to provider subprocesses.

### Provider Binary Overrides (optional)

| Variable              | Default   | Purpose                          |
|-----------------------|-----------|----------------------------------|
| `GORECHERA_CLAUDE_BIN` | `claude` | Path to Claude CLI executable    |
| `GORECHERA_CODEX_BIN`  | `codex`  | Path to Codex CLI executable     |

Use these if the binary is not on PATH or you want to pin a specific version:

```bash
export GORECHERA_CLAUDE_BIN=/usr/local/bin/claude-1.2.0
```

### HTTP Server Auth (optional)

| Variable              | Default   | Purpose                                          |
|-----------------------|-----------|--------------------------------------------------|
| `GORCHERA_AUTH_TOKEN` | (none)    | Bearer token for HTTP API. Unset = no auth (dev) |

When set, all HTTP API requests must include:

```
Authorization: Bearer <token>
```

---

## 4. Running

Gorchera has three operating modes.

### MCP Server Mode (Claude Code integration -- recommended)

Runs as a stdio MCP server that Claude Code connects to:

```bash
gorchera mcp
```

Options:

```
-recover                  Resume all interrupted jobs from previous run
-recover-jobs job1,job2   Resume specific jobs only
```

### CLI Mode

Submit and manage jobs directly from the terminal.

Start a job:

```bash
gorchera run \
  -goal "Add input validation to the user registration endpoint" \
  -provider codex \
  -tech-stack go \
  -workspace-mode shared
```

Key flags for `run`:

| Flag              | Default   | Description                              |
|-------------------|-----------|------------------------------------------|
| `-goal`           | (required) | What the agent pipeline should achieve  |
| `-provider`       | `mock`    | `claude` or `codex`                      |
| `-tech-stack`     | `go`      | Label passed to agents for context       |
| `-workspace-mode` | `shared`  | `shared` (current dir) or `isolated` (git worktree) |
| `-max-steps`      | `8`       | Max executor steps per job               |
| `-strictness`     | `normal`  | Evaluator gate: `strict`, `normal`, `lenient` |
| `-profiles-file`  | (none)    | JSON file with per-role model overrides  |

Check job status:

```bash
gorchera status -job <job-id>
gorchera status -all
```

Other CLI subcommands:

```bash
gorchera events     -job <id>   # event log
gorchera artifacts  -job <id>   # output artifact paths
gorchera approve    -job <id>   # approve a blocked job
gorchera reject     -job <id> -reason "..."
gorchera retry      -job <id>
gorchera cancel     -job <id> -reason "..."
gorchera resume     -job <id>   # resume a paused job
```

### HTTP Server Mode

Runs a REST API server (includes SSE event streaming and a web dashboard):

```bash
gorchera serve -addr 127.0.0.1:8080
```

Options:

```
-addr string              Listen address (default: 127.0.0.1:8080)
-recover                  Resume all interrupted jobs on startup
-recover-jobs job1,job2   Resume specific jobs on startup
```

Stream events for a running job:

```bash
gorchera stream -job <id> -server http://127.0.0.1:8080
```

Dashboard (static files served from `web/`):

```
http://127.0.0.1:8080/dashboard/
```

Health check:

```
GET http://127.0.0.1:8080/healthz
```

---

## 5. Claude Code Integration

Register gorchera as an MCP server so Claude Code can submit and manage jobs
through natural language.

### .mcp.json (project-local, recommended)

Place this file in the project root (already present in this repo):

```json
{
  "mcpServers": {
    "gorchera": {
      "type": "stdio",
      "command": "gorchera",
      "args": ["mcp"]
    }
  }
}
```

If you built the binary but haven't installed it to PATH, use the full path:

```json
{
  "mcpServers": {
    "gorchera": {
      "type": "stdio",
      "command": "D:/path/to/gorchera",
      "args": ["mcp"]
    }
  }
}
```

Or use `go run` to skip the build step (useful during development):

```json
{
  "mcpServers": {
    "gorchera": {
      "type": "stdio",
      "command": "go",
      "args": ["run", "./cmd/gorchera", "mcp"]
    }
  }
}
```

### Recovery on startup

To auto-resume interrupted jobs when Claude Code (re)starts the MCP server:

```json
{
  "mcpServers": {
    "gorchera": {
      "type": "stdio",
      "command": "gorchera",
      "args": ["mcp", "-recover"]
    }
  }
}
```

### Per-role model overrides

Create a profiles JSON file to assign different models per role:

```json
{
  "director":  {"provider": "codex",  "model": "gpt-4o"},
  "executor":  {"provider": "claude", "model": "sonnet"},
  "evaluator": {"provider": "claude", "model": "sonnet"}
}
```

Pass it when starting a job:

```bash
gorchera run -goal "..." -provider codex -profiles-file profiles.json
```

See `docs/SUPERVISOR_GUIDE.md` for goal-writing guidance and pipeline tuning.
