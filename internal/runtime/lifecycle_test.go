package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProcessManagerStartStatusAndStop(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTempBackgroundProgram(t, dir, `
package main

import (
	"fmt"
	"time"
)

func main() {
	fmt.Println("background-started")
	time.Sleep(5 * time.Second)
}
`)

	exePath := filepath.Join(dir, "sleepy.exe")
	buildCmd := exec.Command("go", "build", "-o", exePath, "main.go")
	buildCmd.Dir = dir
	buildCmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build test binary: %v\n%s", err, string(out))
	}

	policy := NewDefaultPolicy()
	policy.Allow(CategoryCommand, filepath.Base(exePath))
	manager := NewProcessManager(policy)

	handle, err := manager.Start(context.Background(), StartRequest{
		Request: Request{
			Category: CategoryCommand,
			Command:  exePath,
			Dir:      dir,
			Timeout:  30 * time.Second,
		},
		Name:   "sleepy",
		LogDir: filepath.Join(dir, "logs"),
		Port:   8080,
	})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if handle.PID == 0 {
		t.Fatal("expected pid to be populated")
	}
	if handle.LogPath == "" {
		t.Fatal("expected log path to be populated")
	}
	if handle.Port != 8080 {
		t.Fatalf("expected port 8080, got %d", handle.Port)
	}

	list := manager.List(context.Background())
	if len(list) != 1 || list[0].PID != handle.PID {
		t.Fatalf("expected one listed process with pid %d, got %+v", handle.PID, list)
	}

	byName := manager.FindByName(context.Background(), "sleepy")
	if len(byName) != 1 || byName[0].PID != handle.PID {
		t.Fatalf("expected one named process with pid %d, got %+v", handle.PID, byName)
	}

	byCategory := manager.FindByCategory(context.Background(), CategoryCommand)
	if len(byCategory) != 1 || byCategory[0].PID != handle.PID {
		t.Fatalf("expected one categorized process with pid %d, got %+v", handle.PID, byCategory)
	}

	status, err := waitForProcessState(context.Background(), manager, handle.PID, ProcessStateRunning)
	if err != nil {
		t.Fatalf("waiting for running status failed: %v", err)
	}
	if !status.Running {
		t.Fatal("expected running status")
	}

	if err := waitForFileContains(handle.LogPath, "background-started"); err != nil {
		t.Fatalf("waiting for log output failed: %v", err)
	}

	if _, err := manager.Stop(context.Background(), handle.PID); err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}

	stopped, err := waitForProcessState(context.Background(), manager, handle.PID, ProcessStateStopped)
	if err != nil {
		t.Fatalf("waiting for stopped status failed: %v", err)
	}
	if stopped.Running {
		t.Fatal("expected stopped process to report not running")
	}
	if stopped.State != ProcessStateStopped {
		t.Fatalf("expected stopped state, got %s", stopped.State)
	}

	data, err := os.ReadFile(handle.LogPath)
	if err != nil {
		t.Fatalf("failed to read log file: %v", err)
	}
	if !strings.Contains(string(data), "background-started") {
		t.Fatalf("expected log output to contain startup line, got %q", string(data))
	}
}

func TestProcessManagerReportsMissingProcess(t *testing.T) {
	t.Parallel()

	manager := NewProcessManager(NewDefaultPolicy())
	if _, err := manager.Status(context.Background(), 999999); err != ErrProcessNotFound {
		t.Fatalf("expected ErrProcessNotFound, got %v", err)
	}
}

func TestProcessManagerWaitReturnsExitedHandle(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTempBackgroundProgram(t, dir, `
package main

import "time"

func main() {
	time.Sleep(150 * time.Millisecond)
}
`)

	exePath := filepath.Join(dir, "quick.exe")
	buildCmd := exec.Command("go", "build", "-o", exePath, "main.go")
	buildCmd.Dir = dir
	buildCmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build test binary: %v\n%s", err, string(out))
	}

	policy := NewDefaultPolicy()
	policy.Allow(CategoryCommand, filepath.Base(exePath))
	manager := NewProcessManager(policy)

	handle, err := manager.Start(context.Background(), StartRequest{
		Request: Request{
			Category: CategoryCommand,
			Command:  exePath,
			Dir:      dir,
			Timeout:  30 * time.Second,
		},
		Name:   "quick",
		LogDir: filepath.Join(dir, "logs"),
	})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}

	finished, err := manager.Wait(context.Background(), handle.PID)
	if err != nil {
		t.Fatalf("Wait returned error: %v", err)
	}
	if finished.State != ProcessStateExited {
		t.Fatalf("expected exited state, got %s", finished.State)
	}
	if finished.Running {
		t.Fatal("expected finished process to report not running")
	}
}

// TestProcessManagerEnvDoesNotLeakSecrets verifies that ProcessManager.Start
// does not pass ANTHROPIC_API_KEY (or other secrets) into the subprocess env.
func TestProcessManagerEnvDoesNotLeakSecrets(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-secret-value-lifecycle")

	dir := t.TempDir()
	writeTempBackgroundProgram(t, dir, `
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

	exePath := filepath.Join(dir, "envcheck.exe")
	buildCmd := exec.Command("go", "build", "-o", exePath, "main.go")
	buildCmd.Dir = dir
	buildCmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build test binary: %v\n%s", err, string(out))
	}

	policy := NewDefaultPolicy()
	policy.Allow(CategoryCommand, filepath.Base(exePath))
	manager := NewProcessManager(policy)

	handle, err := manager.Start(context.Background(), StartRequest{
		Request: Request{
			Category: CategoryCommand,
			Command:  exePath,
			Dir:      dir,
			Timeout:  30 * time.Second,
		},
		Name:   "envcheck",
		LogDir: filepath.Join(dir, "logs"),
	})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}

	if _, err := manager.Wait(context.Background(), handle.PID); err != nil {
		t.Fatalf("Wait returned error: %v", err)
	}

	data, err := os.ReadFile(handle.LogPath)
	if err != nil {
		t.Fatalf("failed to read log file: %v", err)
	}
	output := string(data)

	if strings.Contains(output, "LEAKED:") {
		t.Fatalf("subprocess env leaked ANTHROPIC_API_KEY: %s", output)
	}
	if !strings.Contains(output, "env-check-done") {
		t.Fatalf("subprocess did not complete as expected: %q", output)
	}
}

// TestWatchProcessStopRequestedNoRace exercises the concurrent path between
// Stop() (which writes rec.stopRequested under m.mu) and watchProcess()
// (which must read it under m.mu). Run with -race to detect regressions.
func TestWatchProcessStopRequestedNoRace(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTempBackgroundProgram(t, dir, `
package main

import "time"

func main() {
	time.Sleep(2 * time.Second)
}
`)

	exePath := filepath.Join(dir, "longsleep.exe")
	buildCmd := exec.Command("go", "build", "-o", exePath, "main.go")
	buildCmd.Dir = dir
	buildCmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build test binary: %v\n%s", err, string(out))
	}

	policy := NewDefaultPolicy()
	policy.Allow(CategoryCommand, filepath.Base(exePath))

	// Run several iterations: Stop() sets stopRequested while watchProcess()
	// may be about to read it -- the race detector catches any unsynchronized access.
	for range 3 {
		manager := NewProcessManager(policy)
		handle, err := manager.Start(context.Background(), StartRequest{
			Request: Request{
				Category: CategoryCommand,
				Command:  exePath,
				Dir:      dir,
				Timeout:  30 * time.Second,
			},
			Name:   "longsleep",
			LogDir: filepath.Join(dir, "logs"),
		})
		if err != nil {
			t.Fatalf("Start returned error: %v", err)
		}

		if _, err := manager.Stop(context.Background(), handle.PID); err != nil {
			t.Fatalf("Stop returned error: %v", err)
		}

		stopped, err := manager.Status(context.Background(), handle.PID)
		if err != nil {
			t.Fatalf("Status returned error: %v", err)
		}
		if stopped.State != ProcessStateStopped {
			t.Fatalf("expected stopped state, got %s", stopped.State)
		}
	}
}

func writeTempBackgroundProgram(t *testing.T, dir, source string) {
	t.Helper()
	path := filepath.Join(dir, "main.go")
	if err := os.WriteFile(path, []byte(strings.TrimSpace(source)+"\n"), 0o644); err != nil {
		t.Fatalf("write temp program: %v", err)
	}
}

func waitForProcessState(ctx context.Context, manager *ProcessManager, pid int, state ProcessState) (ProcessHandle, error) {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()

	for {
		handle, err := manager.Status(ctx, pid)
		if err != nil {
			return ProcessHandle{}, err
		}
		if handle.State == state {
			return handle, nil
		}
		select {
		case <-ctx.Done():
			return ProcessHandle{}, ctx.Err()
		case <-deadline.C:
			return ProcessHandle{}, context.DeadlineExceeded
		case <-ticker.C:
		}
	}
}

func waitForFileContains(path, needle string) error {
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()

	for {
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), needle) {
			return nil
		}
		select {
		case <-deadline.C:
			return context.DeadlineExceeded
		case <-ticker.C:
		}
	}
}
