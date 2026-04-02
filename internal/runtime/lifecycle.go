package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var ErrProcessNotFound = errors.New("runtime process not found")
var ErrProcessNotRunning = errors.New("runtime process not running")

type ProcessManager struct {
	Policy         *Policy
	Environment    []string
	DefaultTimeout time.Duration
	LookPath       func(string) (string, error)
	CommandFactory func(context.Context, string, ...string) *exec.Cmd

	mu        sync.Mutex
	processes map[int]*processRecord
}

type processRecord struct {
	handle        ProcessHandle
	process       *os.Process
	logFile       *os.File
	done          chan struct{}
	stopRequested bool
}

func NewProcessManager(policy *Policy) *ProcessManager {
	return &ProcessManager{
		Policy:         policy,
		DefaultTimeout: 5 * time.Minute,
		Environment:    nil,
		LookPath:       exec.LookPath,
		CommandFactory: exec.CommandContext,
		processes:      make(map[int]*processRecord),
	}
}

func (m *ProcessManager) Start(ctx context.Context, req StartRequest) (ProcessHandle, error) {
	if req.Timeout <= 0 {
		req.Timeout = m.DefaultTimeout
	}
	if req.LogDir == "" {
		req.LogDir = os.TempDir()
	}
	if m.Policy == nil {
		return ProcessHandle{}, fmt.Errorf("policy is nil")
	}
	if err := m.Policy.Allows(req.Request); err != nil {
		return ProcessHandle{}, fmt.Errorf("%w: %v", ErrNotAllowed, err)
	}
	if err := os.MkdirAll(req.LogDir, 0o755); err != nil {
		return ProcessHandle{}, err
	}

	logPath := filepath.Join(req.LogDir, processLogName(req.Name, req.Command))
	logFile, err := os.Create(logPath)
	if err != nil {
		return ProcessHandle{}, err
	}

	factory := m.CommandFactory
	if factory == nil {
		factory = exec.CommandContext
	}
	cmd := factory(ctx, req.Command, req.Args...)
	cmd.Dir = req.Dir
	cmd.Env = append(minimalEnv(), m.Environment...)
	cmd.Env = append(cmd.Env, req.Env...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	startedAt := time.Now().UTC()
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return ProcessHandle{}, err
	}

	handle := ProcessHandle{
		PID:       cmd.Process.Pid,
		Name:      req.Name,
		Category:  req.Category,
		Command:   req.Command,
		Args:      append([]string(nil), req.Args...),
		Port:      req.Port,
		LogPath:   logPath,
		State:     ProcessStateRunning,
		StartedAt: startedAt,
		Running:   true,
	}
	rec := &processRecord{
		handle:  handle,
		process: cmd.Process,
		logFile: logFile,
		done:    make(chan struct{}),
	}

	m.mu.Lock()
	m.processes[handle.PID] = rec
	m.mu.Unlock()

	go m.watchProcess(handle.PID, cmd, rec)
	return handle, nil
}

func (m *ProcessManager) Stop(ctx context.Context, pid int) (ProcessHandle, error) {
	rec, err := m.record(pid)
	if err != nil {
		return ProcessHandle{}, err
	}

	m.mu.Lock()
	rec.stopRequested = true
	process := rec.process
	done := rec.done
	m.mu.Unlock()

	if process != nil {
		_ = process.Kill()
	}

	select {
	case <-done:
		return m.Status(ctx, pid)
	case <-ctx.Done():
		return m.Status(ctx, pid)
	}
}

func (m *ProcessManager) Status(_ context.Context, pid int) (ProcessHandle, error) {
	rec, err := m.record(pid)
	if err != nil {
		return ProcessHandle{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return rec.handle, nil
}

func (m *ProcessManager) List(_ context.Context) []ProcessHandle {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]ProcessHandle, 0, len(m.processes))
	for _, rec := range m.processes {
		out = append(out, rec.handle)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].PID < out[j].PID
		}
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out
}

func (m *ProcessManager) FindByName(_ context.Context, name string) []ProcessHandle {
	needle := normalizeName(name)
	if needle == "" {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	var out []ProcessHandle
	for _, rec := range m.processes {
		if normalizeName(rec.handle.Name) == needle {
			out = append(out, rec.handle)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].PID < out[j].PID
		}
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out
}

func (m *ProcessManager) FindByCategory(_ context.Context, category Category) []ProcessHandle {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []ProcessHandle
	for _, rec := range m.processes {
		if rec.handle.Category == category {
			out = append(out, rec.handle)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].PID < out[j].PID
		}
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out
}

func (m *ProcessManager) Wait(ctx context.Context, pid int) (ProcessHandle, error) {
	rec, err := m.record(pid)
	if err != nil {
		return ProcessHandle{}, err
	}

	m.mu.Lock()
	done := rec.done
	m.mu.Unlock()

	select {
	case <-done:
		return m.Status(ctx, pid)
	case <-ctx.Done():
		return m.Status(ctx, pid)
	}
}

func (m *ProcessManager) record(pid int) (*processRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.processes[pid]
	if !ok {
		return nil, ErrProcessNotFound
	}
	return rec, nil
}

func (m *ProcessManager) watchProcess(pid int, cmd *exec.Cmd, rec *processRecord) {
	waitErr := cmd.Wait()
	finishedAt := time.Now().UTC()
	exitCode := exitCodeFromError(waitErr)
	// Read stopRequested under the mutex to avoid a data race with Stop(),
	// which sets this field while holding m.mu.
	m.mu.Lock()
	stopRequested := rec.stopRequested
	m.mu.Unlock()
	state := ProcessStateExited
	if stopRequested {
		state = ProcessStateStopped
	} else if waitErr != nil || exitCode != 0 {
		state = ProcessStateFailed
	}

	m.mu.Lock()
	rec.handle.FinishedAt = finishedAt
	rec.handle.ExitCode = exitCode
	rec.handle.State = state
	rec.handle.Running = false
	if waitErr != nil && state == ProcessStateFailed {
		rec.handle.Error = waitErr.Error()
	}
	if rec.logFile != nil {
		_ = rec.logFile.Close()
		rec.logFile = nil
	}
	close(rec.done)
	m.mu.Unlock()
}

func processLogName(name, command string) string {
	base := strings.TrimSpace(name)
	if base == "" {
		base = filepath.Base(command)
	}
	base = sanitizeComponent(base)
	return base + "-" + time.Now().UTC().Format("20060102-150405.000") + ".log"
}

func sanitizeComponent(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "process"
	}
	value = strings.ReplaceAll(value, string(os.PathSeparator), "-")
	value = strings.ReplaceAll(value, " ", "-")
	value = strings.ReplaceAll(value, ":", "-")
	return value
}

func normalizeName(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ToLower(value)
	value = strings.ReplaceAll(value, string(os.PathSeparator), "-")
	value = strings.ReplaceAll(value, "/", "-")
	value = strings.ReplaceAll(value, "\\", "-")
	value = strings.ReplaceAll(value, " ", "-")
	value = strings.ReplaceAll(value, ":", "-")
	return value
}
