package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gorchera/internal/domain"
)

type CodexAdapter struct {
	executable string
	probeArgs  []string
	probeTime  time.Duration
	runTime    time.Duration
	// runCommand feeds prompt via stdin; codex exec reads stdin when prompt is "-".
	runCommand func(context.Context, string, time.Duration, string, []string, string, ...string) (CommandResult, error)
}

func NewCodexAdapter() *CodexAdapter {
	return &CodexAdapter{
		executable: envOrDefault("GORECHERA_CODEX_BIN", "codex"),
		probeArgs:  []string{"--version"},
		probeTime:  5 * time.Second,
		runTime:    30 * time.Minute,
		runCommand: runExecutableWithStdin,
	}
}

func (a *CodexAdapter) Name() domain.ProviderName {
	return domain.ProviderCodex
}

func (a *CodexAdapter) RunLeader(ctx context.Context, job domain.Job) (string, error) {
	if err := a.ensureReady(ctx); err != nil {
		return "", err
	}
	return a.runStructured(ctx, job.WorkspaceDir, buildLeaderPrompt(job), leaderSchema(), job.RoleProfiles.ProfileFor(domain.RoleLeader, job.Provider).Model)
}

func (a *CodexAdapter) RunPlanner(ctx context.Context, job domain.Job) (string, error) {
	if err := a.ensureReady(ctx); err != nil {
		return "", err
	}
	return a.runStructured(ctx, job.WorkspaceDir, buildPlannerPrompt(job), plannerSchema(), job.RoleProfiles.ProfileFor(domain.RolePlanner, job.Provider).Model)
}

func (a *CodexAdapter) RunEvaluator(ctx context.Context, job domain.Job) (string, error) {
	if err := a.ensureReady(ctx); err != nil {
		return "", err
	}
	return a.runStructured(ctx, job.WorkspaceDir, buildEvaluatorPrompt(job), evaluatorSchema(), job.RoleProfiles.ProfileFor(domain.RoleEvaluator, job.Provider).Model)
}

func (a *CodexAdapter) RunWorker(ctx context.Context, job domain.Job, task domain.LeaderOutput) (string, error) {
	if err := a.ensureReady(ctx); err != nil {
		return "", err
	}
	return a.runStructured(ctx, job.WorkspaceDir, buildWorkerPrompt(job, task), workerSchema(), job.RoleProfiles.ProfileFor(domain.RoleForTaskType(task.TaskType), job.Provider).Model)
}

func (a *CodexAdapter) ensureReady(ctx context.Context) error {
	executable := a.executable
	if executable == "" {
		executable = "codex"
	}
	if _, err := probeExecutable(ctx, executable, a.probeTime, a.probeArgs...); err != nil {
		if isNotFound(err) {
			return missingExecutableError(a.Name(), executable, err)
		}
		return probeFailedError(a.Name(), executable, err)
	}
	return nil
}

func (a *CodexAdapter) runStructured(ctx context.Context, workspaceDir, prompt, schema, model string) (string, error) {
	schemaPath, err := writeSchemaFile(workspaceDir, "schema.json", schema)
	if err != nil {
		return "", invalidResponseError(a.Name(), a.executable, "failed to write schema file", err)
	}
	defer os.RemoveAll(filepath.Dir(schemaPath))

	outputDir, err := os.MkdirTemp(firstNonEmpty(workspaceDir, os.TempDir()), "gorchera-codex-*")
	if err != nil {
		return "", invalidResponseError(a.Name(), a.executable, "failed to create output directory", err)
	}
	defer os.RemoveAll(outputDir)
	outputPath := outputDir + string(os.PathSeparator) + "result.json"

	// workspace-write sandbox allows the agent to create and modify files in the
	// workspace directory, which is required for implement/test/command tasks.
	// --skip-git-repo-check is needed because the workspace may not be a git repo.
	// Prompt is fed via stdin ("-") to avoid Windows command-line length limits
	// and to prevent JSON payload characters from being misinterpreted by the shell.
	args := []string{
		"exec",
		"--fresh",
		"--skip-git-repo-check",
		"-s", "workspace-write",
		"--output-schema", schemaPath,
		"-o", outputPath,
		"-C", firstNonEmpty(workspaceDir, "."),
		"-", // read prompt from stdin
	}
	if model = strings.TrimSpace(model); model != "" && isCodexModel(model) {
		args = append(args, "--model", model)
	}
	if executable := a.executable; executable != "" {
		result, err := a.runCommand(ctx, executable, a.runTime, workspaceDir, nil, prompt, args...)
		if err != nil {
			return "", classifyCommandError(a.Name(), executable, result, err)
		}
		data, readErr := os.ReadFile(outputPath)
		if readErr != nil {
			return "", invalidResponseError(a.Name(), executable, "failed to read codex output", readErr)
		}
		return string(data), nil
	}
	return "", invalidResponseError(a.Name(), a.executable, "missing codex executable", nil)
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func isCodexModel(model string) bool {
	normalized := strings.ToLower(strings.TrimSpace(model))
	if normalized == "" {
		return true
	}
	switch normalized {
	case "opus", "sonnet", "haiku":
		return false
	default:
		return strings.HasPrefix(normalized, "gpt")
	}
}
