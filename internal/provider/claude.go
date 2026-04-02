package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"

	"gorchera/internal/domain"
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
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return ""
	}

	// Claude JSON output can wrap the final payload in several envelope shapes
	// depending on CLI version and structured-output mode. Extract the actual
	// schema payload when present, otherwise fall back to the raw output.
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &envelope); err != nil {
		return ""
	}

	for _, key := range []string{"structured_output", "parsed_output", "result"} {
		if extracted := extractJSONEnvelopeValue(envelope[key]); extracted != "" {
			return extracted
		}
	}
	return ""
}

func extractJSONEnvelopeValue(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}

	// result may be a JSON string containing structured text, while newer
	// structured-output flows can place the parsed object directly in the field.
	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return ""
		}
		return strings.TrimSpace(text)
	}

	return string(raw)
}
