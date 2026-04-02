package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPolicyAllowsAndBlocksByCategory(t *testing.T) {
	t.Parallel()

	policy := NewDefaultPolicy()

	if err := policy.Allows(Request{Category: CategoryBuild, Command: "go"}); err != nil {
		t.Fatalf("expected go to be allowed for build: %v", err)
	}
	if err := policy.Allows(Request{Category: CategorySearch, Command: "rg"}); err != nil {
		t.Fatalf("expected rg to be allowed for search: %v", err)
	}
	if err := policy.Allows(Request{Category: CategoryBuild, Command: "powershell"}); err == nil {
		t.Fatal("expected powershell to be blocked")
	}
}

func TestRunnerCapturesStdoutStderrAndExitCode(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTempGoProgram(t, dir, `
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Println("stdout-line")
	fmt.Fprintln(os.Stderr, "stderr-line")
	os.Exit(7)
}
`)

	policy := NewDefaultPolicy()
	policy.Allow(CategoryTest, "probe")
	runner := NewRunner(policy)

	buildResult, err := runner.Run(context.Background(), Request{
		Category: CategoryBuild,
		Command:  "go",
		Args:     []string{"build", "-o", "probe.exe", "main.go"},
		Dir:      dir,
		Timeout:  30 * time.Second,
	})
	if err != nil {
		t.Fatalf("expected build to succeed, got error: %v", err)
	}
	if buildResult.ExitCode != 0 {
		t.Fatalf("expected build exit code 0, got %d", buildResult.ExitCode)
	}

	result, err := runner.Run(context.Background(), Request{
		Category: CategoryTest,
		Command:  filepath.Join(dir, "probe.exe"),
		Dir:      dir,
		Timeout:  30 * time.Second,
	})
	if err == nil {
		t.Fatal("expected non-zero exit error from probe binary")
	}
	if result.ExitCode != 7 {
		t.Fatalf("expected exit code 7, got %d", result.ExitCode)
	}
	if !strings.Contains(result.Stdout, "stdout-line") {
		t.Fatalf("stdout not captured: %q", result.Stdout)
	}
	if !strings.Contains(result.Stderr, "stderr-line") {
		t.Fatalf("stderr not captured: %q", result.Stderr)
	}
	if result.TimedOut {
		t.Fatal("did not expect timeout")
	}
}

func TestRunnerTimeout(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTempGoProgram(t, dir, `
package main

import "time"

func main() {
	time.Sleep(5 * time.Second)
}
`)

	runner := NewRunner(NewDefaultPolicy())
	result, err := runner.Run(context.Background(), Request{
		Category: CategoryBuild,
		Command:  "go",
		Args:     []string{"run", "main.go"},
		Dir:      dir,
		Timeout:  50 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !result.TimedOut {
		t.Fatal("expected timed out result")
	}
}

// TestMinimalEnvDoesNotLeakSecrets verifies that minimalEnv() strips secrets
// such as ANTHROPIC_API_KEY from the parent process environment.
func TestMinimalEnvDoesNotLeakSecrets(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-secret-value-runner")

	env := minimalEnv()
	for _, e := range env {
		if strings.Contains(strings.ToUpper(e), "ANTHROPIC_API_KEY") {
			t.Fatalf("minimalEnv leaked ANTHROPIC_API_KEY: %q", e)
		}
	}
}

// TestRunnerEnvDoesNotLeakSecrets verifies that Runner.Run does not pass
// ANTHROPIC_API_KEY (or other secrets) from the parent env to the subprocess.
func TestRunnerEnvDoesNotLeakSecrets(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-secret-value-runner-run")

	dir := t.TempDir()
	writeTempGoProgram(t, dir, `
package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	for _, e := range os.Environ() {
		if strings.Contains(strings.ToUpper(e), "ANTHROPIC_API_KEY") {
			fmt.Println("LEAKED:", e)
		}
	}
	fmt.Println("env-check-done")
}
`)

	policy := NewDefaultPolicy()
	policy.Allow(CategoryTest, "envcheck")
	runner := NewRunner(policy)

	buildResult, err := runner.Run(context.Background(), Request{
		Category: CategoryBuild,
		Command:  "go",
		Args:     []string{"build", "-o", "envcheck.exe", "main.go"},
		Dir:      dir,
		Timeout:  30 * time.Second,
	})
	if err != nil {
		t.Fatalf("build failed: %v\nstderr: %s", err, buildResult.Stderr)
	}

	result, _ := runner.Run(context.Background(), Request{
		Category: CategoryTest,
		Command:  filepath.Join(dir, "envcheck.exe"),
		Dir:      dir,
		Timeout:  30 * time.Second,
	})

	if strings.Contains(result.Stdout, "LEAKED:") {
		t.Fatalf("subprocess env leaked ANTHROPIC_API_KEY: %s", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "env-check-done") {
		t.Fatalf("subprocess did not complete as expected: stdout=%q stderr=%q", result.Stdout, result.Stderr)
	}
}

func writeTempGoProgram(t *testing.T, dir, source string) {
	t.Helper()
	path := filepath.Join(dir, "main.go")
	if err := os.WriteFile(path, []byte(strings.TrimSpace(source)+"\n"), 0o644); err != nil {
		t.Fatalf("write temp program: %v", err)
	}
}
