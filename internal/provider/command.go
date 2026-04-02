package provider

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// providerEnv returns a minimal environment for provider subprocess calls.
// It includes OS essentials needed to locate the provider binary and run it,
// plus API keys required by each provider. All other env vars (e.g. secrets
// from the parent shell that are not provider keys) are excluded.
func providerEnv(extra ...string) []string {
	// OS essentials: PATH to find the binary, TEMP/HOME for runtime state.
	safe := []string{
		"PATH", "SYSTEMROOT", "HOME", "TEMP", "TMP",
		"LOCALAPPDATA", "APPDATA", "USERPROFILE",
		"COMSPEC", "PATHEXT",
		// Provider API keys -- explicitly included so CLI tools can authenticate.
		"ANTHROPIC_API_KEY",
		"OPENAI_API_KEY",
		"OPENAI_BASE_URL",
		// Codex/OpenAI org routing
		"OPENAI_ORG_ID",
		"ANTHROPIC_BASE_URL",
	}
	env := make([]string, 0, len(safe)+len(extra))
	for _, key := range safe {
		if val, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+val)
		}
	}
	env = append(env, extra...)
	return env
}

type CommandResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

func probeExecutable(ctx context.Context, executable string, timeout time.Duration, args ...string) (CommandResult, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if _, err := exec.LookPath(executable); err != nil {
		return CommandResult{}, err
	}

	cmd := exec.CommandContext(probeCtx, executable, args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := CommandResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}
	if err != nil {
		if probeCtx.Err() == context.DeadlineExceeded {
			return result, fmt.Errorf("probe timed out: %w", probeCtx.Err())
		}
		return result, err
	}
	return result, nil
}

// runExecutableWithStdin is like runExecutable but feeds stdinData to the process stdin.
func runExecutableWithStdin(ctx context.Context, executable string, timeout time.Duration, dir string, env []string, stdinData string, args ...string) (CommandResult, error) {
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if _, err := exec.LookPath(executable); err != nil {
		return CommandResult{}, err
	}

	cmd := exec.CommandContext(runCtx, executable, args...)
	if strings.TrimSpace(dir) != "" {
		cmd.Dir = dir
	}
	// Use providerEnv (allowlist-based) instead of cmd.Environ() to avoid
	// leaking arbitrary parent-process secrets to the provider subprocess.
	cmd.Env = providerEnv(env...)
	cmd.Stdin = strings.NewReader(stdinData)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := CommandResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		}
		if runCtx.Err() == context.DeadlineExceeded {
			return result, fmt.Errorf("provider command timed out: %w", runCtx.Err())
		}
		return result, err
	}
	return result, nil
}

func runExecutable(ctx context.Context, executable string, timeout time.Duration, dir string, env []string, args ...string) (CommandResult, error) {
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if _, err := exec.LookPath(executable); err != nil {
		return CommandResult{}, err
	}

	cmd := exec.CommandContext(runCtx, executable, args...)
	if strings.TrimSpace(dir) != "" {
		cmd.Dir = dir
	}
	// Use providerEnv (allowlist-based) instead of cmd.Environ() to avoid
	// leaking arbitrary parent-process secrets to the provider subprocess.
	cmd.Env = providerEnv(env...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := CommandResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		}
		if runCtx.Err() == context.DeadlineExceeded {
			return result, fmt.Errorf("provider command timed out: %w", runCtx.Err())
		}
		return result, err
	}
	return result, nil
}
