package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

var ErrNotAllowed = errors.New("runtime command not allowed")

type Runner struct {
	Policy           *Policy
	DefaultTimeout   time.Duration
	DefaultMaxOutput int64
	Environment      []string
	LookPath         func(string) (string, error)
	CommandFactory   func(context.Context, string, ...string) *exec.Cmd
}

func NewRunner(policy *Policy) *Runner {
	return &Runner{
		Policy:           policy,
		DefaultTimeout:   5 * time.Minute,
		DefaultMaxOutput: 1 << 20,
		Environment:      nil,
		LookPath:         exec.LookPath,
		CommandFactory:   exec.CommandContext,
	}
}

func (r *Runner) Run(parent context.Context, req Request) (Result, error) {
	if req.Timeout <= 0 {
		req.Timeout = r.DefaultTimeout
	}
	if req.MaxOutputBytes <= 0 {
		req.MaxOutputBytes = r.DefaultMaxOutput
	}
	if err := r.Policy.Allows(req); err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrNotAllowed, err)
	}

	ctx, cancel := context.WithTimeout(parent, req.Timeout)
	defer cancel()

	cmd := r.CommandFactory(ctx, req.Command, req.Args...)
	cmd.Dir = req.Dir
	cmd.Env = append(minimalEnv(), r.Environment...)
	cmd.Env = append(cmd.Env, req.Env...)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return Result{}, err
	}

	startedAt := time.Now().UTC()
	if err := cmd.Start(); err != nil {
		return Result{}, err
	}

	stdoutCapture := newLimitedCapture(req.MaxOutputBytes)
	stderrCapture := newLimitedCapture(req.MaxOutputBytes)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(stdoutCapture, stdoutPipe)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(stderrCapture, stderrPipe)
	}()

	waitErr := cmd.Wait()
	wg.Wait()

	finishedAt := time.Now().UTC()
	result := Result{
		Category:        req.Category,
		Command:         req.Command,
		Args:            append([]string(nil), req.Args...),
		Stdout:          stdoutCapture.String(),
		Stderr:          stderrCapture.String(),
		StartedAt:       startedAt,
		FinishedAt:      finishedAt,
		Duration:        finishedAt.Sub(startedAt),
		TruncatedStdout: stdoutCapture.Truncated(),
		TruncatedStderr: stderrCapture.Truncated(),
	}

	if exitCode := exitCodeFromError(waitErr); exitCode != -1 {
		result.ExitCode = exitCode
	}

	if ctx.Err() == context.DeadlineExceeded {
		result.TimedOut = true
		if result.ExitCode == 0 {
			result.ExitCode = 1
		}
		return result, ctx.Err()
	}
	if waitErr != nil {
		return result, waitErr
	}
	return result, nil
}

// minimalEnv returns a minimal environment for subprocesses containing only
// variables needed for basic OS and Go toolchain operation. This prevents
// leaking secrets (API keys, tokens, passwords) that callers may have set.
// Callers may append additional variables via r.Environment or req.Env.
func minimalEnv() []string {
	// Safe variables: OS essentials + Go toolchain configuration.
	// Secrets such as ANTHROPIC_API_KEY, OPENAI_API_KEY, etc. are excluded.
	safe := []string{
		"PATH", "SYSTEMROOT", "HOME", "TEMP", "TMP",
		// Windows user profile dirs (needed by Go build cache)
		"LOCALAPPDATA", "APPDATA", "USERPROFILE",
		// Windows shell helpers
		"COMSPEC", "PATHEXT",
		// Go toolchain vars (not secrets)
		"GOCACHE", "GOPATH", "GOROOT", "GOPROXY",
		"GONOSUMCHECK", "GONOSUMDB", "GONOPROXY", "GOFLAGS",
		"GOTMPDIR", "CGO_ENABLED",
	}
	env := make([]string, 0, len(safe))
	for _, key := range safe {
		if val, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+val)
		}
	}
	return env
}

func exitCodeFromError(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			return status.ExitStatus()
		}
		return 1
	}
	return -1
}

type limitedCapture struct {
	limit     int64
	used      int64
	truncated bool
	buf       bytes.Buffer
}

func newLimitedCapture(limit int64) *limitedCapture {
	if limit <= 0 {
		limit = 1 << 20
	}
	return &limitedCapture{limit: limit}
}

func (c *limitedCapture) Write(p []byte) (int, error) {
	if c.used >= c.limit {
		c.truncated = true
		return len(p), nil
	}
	remaining := c.limit - c.used
	if int64(len(p)) > remaining {
		_, _ = c.buf.Write(p[:remaining])
		c.used += remaining
		c.truncated = true
		return len(p), nil
	}
	n, err := c.buf.Write(p)
	c.used += int64(n)
	return n, err
}

func (c *limitedCapture) String() string {
	return c.buf.String()
}

func (c *limitedCapture) Truncated() bool {
	return c.truncated
}
