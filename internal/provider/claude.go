package provider

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"gorechera/internal/domain"
)

type ClaudeAdapter struct {
	executable string
	probeArgs  []string
	probeTime  time.Duration
	runTime    time.Duration
	// runCommand accepts an optional stdinData string before variadic args.
	// Signature: (ctx, executable, timeout, dir, env, stdinData, args...)
	runCommand func(context.Context, string, time.Duration, string, []string, string, ...string) (CommandResult, error)
}

func NewClaudeAdapter() *ClaudeAdapter {
	return &ClaudeAdapter{
		executable: envOrDefault("GORECHERA_CLAUDE_BIN", "claude"),
		probeArgs:  []string{"--version"},
		probeTime:  5 * time.Second,
		runTime:    30 * time.Minute,
		runCommand: runExecutableWithStdin,
	}
}

func (a *ClaudeAdapter) Name() domain.ProviderName {
	return domain.ProviderClaude
}

func (a *ClaudeAdapter) RunLeader(ctx context.Context, job domain.Job) (string, error) {
	if err := a.ensureReady(ctx); err != nil {
		return "", err
	}
	return a.runStructured(ctx, job.WorkspaceDir, buildLeaderPrompt(job), leaderSchema(), job.RoleProfiles.ProfileFor(domain.RoleLeader, job.Provider))
}

func (a *ClaudeAdapter) RunPlanner(ctx context.Context, job domain.Job) (string, error) {
	if err := a.ensureReady(ctx); err != nil {
		return "", err
	}
	return a.runStructured(ctx, job.WorkspaceDir, buildPlannerPrompt(job), plannerSchema(), job.RoleProfiles.ProfileFor(domain.RolePlanner, job.Provider))
}

func (a *ClaudeAdapter) RunEvaluator(ctx context.Context, job domain.Job) (string, error) {
	if err := a.ensureReady(ctx); err != nil {
		return "", err
	}
	return a.runStructured(ctx, job.WorkspaceDir, buildEvaluatorPrompt(job), evaluatorSchema(), job.RoleProfiles.ProfileFor(domain.RoleEvaluator, job.Provider))
}

func (a *ClaudeAdapter) RunWorker(ctx context.Context, job domain.Job, task domain.LeaderOutput) (string, error) {
	if err := a.ensureReady(ctx); err != nil {
		return "", err
	}
	return a.runStructured(ctx, job.WorkspaceDir, buildWorkerPrompt(job, task), workerSchema(), job.RoleProfiles.ProfileFor(domain.RoleForTaskType(task.TaskType), job.Provider))
}

func (a *ClaudeAdapter) ensureReady(ctx context.Context) error {
	executable := a.executable
	if executable == "" {
		executable = "claude"
	}
	if _, err := probeExecutable(ctx, executable, a.probeTime, a.probeArgs...); err != nil {
		if isNotFound(err) {
			return missingExecutableError(a.Name(), executable, err)
		}
		return probeFailedError(a.Name(), executable, err)
	}
	return nil
}

func (a *ClaudeAdapter) runStructured(ctx context.Context, workspaceDir, prompt, schema string, profile domain.ExecutionProfile) (string, error) {
	// minify schema to a single line to avoid shell tokenization issues
	minSchema := minifyJSON(schema)
	args := []string{
		"-p",
		"--permission-mode", "dontAsk",
		"--output-format", "json",
		"--json-schema", minSchema,
	}

	if profile.Model != "" {
		args = append(args, "--model", profile.Model)
	}

	args = append(args, "--no-session-persistence")

	// prompt is passed via stdin; claude -p reads from stdin when no positional arg is given
	executable := firstNonEmpty(a.executable, "claude")
	result, err := a.runCommand(ctx, executable, a.runTime, workspaceDir, nil, prompt, args...)
	if err != nil {
		return "", classifyCommandError(a.Name(), executable, result, err)
	}
	output := result.Stdout
	if output == "" {
		output = result.Stderr
	}
	if output == "" {
		return "", invalidResponseError(a.Name(), executable, "empty claude response", nil)
	}

	// --output-format json returns a JSON envelope with "result" field
	// Try to extract the result content; if it fails, use raw output
	extracted := extractJSONResult(output)
	if extracted != "" {
		return extracted, nil
	}
	return output, nil
}

func extractJSONResult(output string) string {
	// Claude --output-format json returns an envelope with either:
	//   "result" field (text mode) or "structured_output" field (when --json-schema is used)
	var envelope struct {
		Result           string          `json:"result"`
		StructuredOutput json.RawMessage `json:"structured_output"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &envelope); err != nil {
		return ""
	}
	// Prefer structured_output when present and non-null
	if len(envelope.StructuredOutput) > 0 && string(envelope.StructuredOutput) != "null" {
		return string(envelope.StructuredOutput)
	}
	if envelope.Result != "" {
		return envelope.Result
	}
	return ""
}
