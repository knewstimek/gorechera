package mcpsmoke

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"gorchera/internal/domain"
	"gorchera/internal/store"
)

const (
	defaultWaitTimeout = 20 * time.Second
	maxToolReadSize    = 1024 * 1024
)

type Config struct {
	ServerBin     string
	ServerArgs    []string
	Workdir       string
	Scenario      string
	RecoveryJobs  int
	RecoverJobIDs []string
	KeepWorkdir   bool
	WaitTimeout   time.Duration
	RecoveryState domain.JobStatus
}

type Summary struct {
	Scenario            string            `json:"scenario"`
	Workdir             string            `json:"workdir"`
	ServerName          string            `json:"server_name"`
	ServerVersion       string            `json:"server_version"`
	ToolCount           int               `json:"tool_count"`
	StartedJobID        string            `json:"started_job_id,omitempty"`
	StartedJobStatus    string            `json:"started_job_status,omitempty"`
	WorkspaceMode       string            `json:"workspace_mode,omitempty"`
	RequestedWorkspace  string            `json:"requested_workspace,omitempty"`
	ActualWorkspace     string            `json:"actual_workspace,omitempty"`
	RecoveryRequested   int               `json:"recovery_requested,omitempty"`
	RecoveredStatuses   map[string]string `json:"recovered_statuses,omitempty"`
	InterruptedStatuses map[string]string `json:"interrupted_statuses,omitempty"`
	Stderr              string            `json:"stderr,omitempty"`
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type toolCallParams struct {
	Name      string `json:"name"`
	Arguments any    `json:"arguments,omitempty"`
}

type toolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

type initializeResult struct {
	ServerInfo struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
}

type toolsListResult struct {
	Tools []struct {
		Name string `json:"name"`
	} `json:"tools"`
}

type client struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stderr  strings.Builder
	scanner *bufio.Scanner
	recvMu  sync.Mutex
	msgs    chan rpcMessage
	readErr chan error
}

func Run(cfg Config) (*Summary, error) {
	if strings.TrimSpace(cfg.ServerBin) == "" {
		return nil, fmt.Errorf("server binary is required")
	}
	if strings.TrimSpace(cfg.Workdir) == "" {
		return nil, fmt.Errorf("workdir is required")
	}
	if cfg.WaitTimeout <= 0 {
		cfg.WaitTimeout = defaultWaitTimeout
	}
	if cfg.RecoveryJobs <= 0 {
		cfg.RecoveryJobs = 3
	}
	if cfg.RecoveryState == "" {
		cfg.RecoveryState = domain.JobStatusStarting
	}

	if err := os.MkdirAll(cfg.Workdir, 0o755); err != nil {
		return nil, err
	}
	if !cfg.KeepWorkdir {
		defer os.RemoveAll(cfg.Workdir)
	}

	scenario := strings.ToLower(strings.TrimSpace(cfg.Scenario))
	if scenario == "" {
		scenario = "basic"
	}

	switch scenario {
	case "basic":
		return runBasic(cfg)
	case "isolated":
		return runIsolated(cfg)
	case "recovery":
		return runRecovery(cfg)
	case "interrupt":
		return runInterrupt(cfg)
	default:
		return nil, fmt.Errorf("unsupported scenario %q", cfg.Scenario)
	}
}

func runBasic(cfg Config) (*Summary, error) {
	c, init, tools, err := startClient(cfg)
	if err != nil {
		return nil, err
	}
	defer c.close()

	workspace := filepath.Join(cfg.Workdir, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return nil, err
	}

	startMsg, err := c.call(3, "tools/call", toolCallParams{
		Name: "gorchera_start_job",
		Arguments: map[string]any{
			"goal":             "mcp smoke basic",
			"provider":         "mock",
			"workspace_dir":    workspace,
			"max_steps":        8,
			"strictness_level": "normal",
			"context_mode":     "full",
		},
	}, cfg.WaitTimeout)
	if err != nil {
		return nil, err
	}
	started, err := decodeJob(startMsg)
	if err != nil {
		return nil, err
	}

	statusMsg, err := c.call(4, "tools/call", toolCallParams{
		Name: "gorchera_status",
		Arguments: map[string]any{
			"job_id":       started.ID,
			"wait":         true,
			"wait_timeout": int(cfg.WaitTimeout / time.Second),
			"compact":      false,
		},
	}, cfg.WaitTimeout+5*time.Second)
	if err != nil {
		return nil, err
	}
	finalJob, err := decodeJob(statusMsg)
	if err != nil {
		return nil, err
	}
	if finalJob.Status != domain.JobStatusDone {
		return nil, fmt.Errorf("basic scenario ended in %s", finalJob.Status)
	}

	return &Summary{
		Scenario:         "basic",
		Workdir:          cfg.Workdir,
		ServerName:       init.ServerInfo.Name,
		ServerVersion:    init.ServerInfo.Version,
		ToolCount:        len(tools.Tools),
		StartedJobID:     finalJob.ID,
		StartedJobStatus: string(finalJob.Status),
		Stderr:           strings.TrimSpace(c.stderr.String()),
	}, nil
}

func runIsolated(cfg Config) (*Summary, error) {
	c, init, tools, err := startClient(cfg)
	if err != nil {
		return nil, err
	}
	defer c.close()

	workspace, err := newGitWorkspace(cfg.Workdir, "isolated-workspace")
	if err != nil {
		return nil, err
	}

	startMsg, err := c.call(3, "tools/call", toolCallParams{
		Name: "gorchera_start_job",
		Arguments: map[string]any{
			"goal":             "mcp smoke isolated workspace",
			"provider":         "mock",
			"workspace_dir":    workspace,
			"workspace_mode":   string(domain.WorkspaceModeIsolated),
			"max_steps":        8,
			"strictness_level": "normal",
			"context_mode":     "full",
		},
	}, cfg.WaitTimeout)
	if err != nil {
		return nil, err
	}
	started, err := decodeJob(startMsg)
	if err != nil {
		return nil, err
	}
	if started.WorkspaceMode != string(domain.WorkspaceModeIsolated) {
		return nil, fmt.Errorf("isolated scenario returned workspace mode %q", started.WorkspaceMode)
	}
	if filepath.Clean(started.RequestedWorkspaceDir) != filepath.Clean(workspace) {
		return nil, fmt.Errorf("isolated scenario requested workspace = %q, want %q", started.RequestedWorkspaceDir, workspace)
	}
	if filepath.Clean(started.WorkspaceDir) == filepath.Clean(workspace) {
		return nil, fmt.Errorf("isolated scenario did not create a detached workspace")
	}

	statusMsg, err := c.call(4, "tools/call", toolCallParams{
		Name: "gorchera_status",
		Arguments: map[string]any{
			"job_id":       started.ID,
			"wait":         true,
			"wait_timeout": int(cfg.WaitTimeout / time.Second),
			"compact":      false,
		},
	}, cfg.WaitTimeout+5*time.Second)
	if err != nil {
		return nil, err
	}
	finalJob, err := decodeJob(statusMsg)
	if err != nil {
		return nil, err
	}
	if finalJob.Status != domain.JobStatusDone {
		return nil, fmt.Errorf("isolated scenario ended in %s", finalJob.Status)
	}

	return &Summary{
		Scenario:           "isolated",
		Workdir:            cfg.Workdir,
		ServerName:         init.ServerInfo.Name,
		ServerVersion:      init.ServerInfo.Version,
		ToolCount:          len(tools.Tools),
		StartedJobID:       finalJob.ID,
		StartedJobStatus:   string(finalJob.Status),
		WorkspaceMode:      finalJob.WorkspaceMode,
		RequestedWorkspace: finalJob.RequestedWorkspaceDir,
		ActualWorkspace:    finalJob.WorkspaceDir,
		Stderr:             strings.TrimSpace(c.stderr.String()),
	}, nil
}

func runRecovery(cfg Config) (*Summary, error) {
	seeded, err := seedRecoverableJobs(cfg)
	if err != nil {
		return nil, err
	}

	c, init, tools, err := startClient(cfg)
	if err != nil {
		return nil, err
	}
	defer c.close()

	selected := make(map[string]struct{}, len(cfg.RecoverJobIDs))
	for _, jobID := range cfg.RecoverJobIDs {
		jobID = strings.TrimSpace(jobID)
		if jobID != "" {
			selected[jobID] = struct{}{}
		}
	}

	stateStore := store.NewStateStore(filepath.Join(cfg.Workdir, ".gorchera", "state"))
	statuses := make(map[string]string, len(seeded))
	callID := 10
	for _, seededJob := range seeded {
		if len(selected) > 0 {
			if _, ok := selected[seededJob.ID]; !ok {
				current, err := stateStore.LoadJob(context.Background(), seededJob.ID)
				if err != nil {
					return nil, err
				}
				statuses[current.ID] = string(current.Status)
				if current.Status != cfg.RecoveryState {
					return nil, fmt.Errorf("unselected job %s unexpectedly changed to %s", current.ID, current.Status)
				}
				continue
			}
		}
		statusMsg, err := c.call(callID, "tools/call", toolCallParams{
			Name: "gorchera_status",
			Arguments: map[string]any{
				"job_id":       seededJob.ID,
				"wait":         true,
				"wait_timeout": int(cfg.WaitTimeout / time.Second),
				"compact":      false,
			},
		}, cfg.WaitTimeout+5*time.Second)
		if err != nil {
			return nil, err
		}
		callID++
		finalJob, err := decodeJob(statusMsg)
		if err != nil {
			return nil, err
		}
		statuses[finalJob.ID] = string(finalJob.Status)
		if finalJob.Status != domain.JobStatusDone {
			return nil, fmt.Errorf("recovered job %s ended in %s", finalJob.ID, finalJob.Status)
		}
	}

	return &Summary{
		Scenario:          "recovery",
		Workdir:           cfg.Workdir,
		ServerName:        init.ServerInfo.Name,
		ServerVersion:     init.ServerInfo.Version,
		ToolCount:         len(tools.Tools),
		RecoveryRequested: len(seeded),
		RecoveredStatuses: statuses,
		Stderr:            strings.TrimSpace(c.stderr.String()),
	}, nil
}

func runInterrupt(cfg Config) (*Summary, error) {
	seeded, err := seedStaleRecoverableJobs(cfg)
	if err != nil {
		return nil, err
	}

	c, init, tools, err := startClient(cfg)
	if err != nil {
		return nil, err
	}
	defer c.close()

	statuses := make(map[string]string, len(seeded))
	callID := 20
	for _, seededJob := range seeded {
		statusMsg, err := c.call(callID, "tools/call", toolCallParams{
			Name: "gorchera_status",
			Arguments: map[string]any{
				"job_id":  seededJob.ID,
				"wait":    false,
				"compact": false,
			},
		}, cfg.WaitTimeout)
		if err != nil {
			return nil, err
		}
		callID++
		current, err := decodeJob(statusMsg)
		if err != nil {
			return nil, err
		}
		statuses[current.ID] = string(current.Status)
		if current.Status != domain.JobStatusBlocked {
			return nil, fmt.Errorf("interrupted job %s remained %s", current.ID, current.Status)
		}
		if !strings.Contains(strings.ToLower(current.BlockedReason), "interrupted") {
			return nil, fmt.Errorf("interrupted job %s missing interruption reason: %q", current.ID, current.BlockedReason)
		}
	}

	return &Summary{
		Scenario:            "interrupt",
		Workdir:             cfg.Workdir,
		ServerName:          init.ServerInfo.Name,
		ServerVersion:       init.ServerInfo.Version,
		ToolCount:           len(tools.Tools),
		InterruptedStatuses: statuses,
		Stderr:              strings.TrimSpace(c.stderr.String()),
	}, nil
}

func startClient(cfg Config) (*client, *initializeResult, *toolsListResult, error) {
	c, err := newClient(cfg.ServerBin, cfg.Workdir, cfg.ServerArgs...)
	if err != nil {
		return nil, nil, nil, err
	}

	initMsg, err := c.call(1, "initialize", map[string]any{}, cfg.WaitTimeout)
	if err != nil {
		_ = c.close()
		return nil, nil, nil, err
	}
	var init initializeResult
	if err := json.Unmarshal(initMsg.Result, &init); err != nil {
		_ = c.close()
		return nil, nil, nil, err
	}

	toolsMsg, err := c.call(2, "tools/list", nil, cfg.WaitTimeout)
	if err != nil {
		_ = c.close()
		return nil, nil, nil, err
	}
	var tools toolsListResult
	if err := json.Unmarshal(toolsMsg.Result, &tools); err != nil {
		_ = c.close()
		return nil, nil, nil, err
	}
	return c, &init, &tools, nil
}

func newClient(binPath, workdir string, serverArgs ...string) (*client, error) {
	args := append([]string{"mcp"}, serverArgs...)
	cmd := exec.Command(binPath, args...)
	cmd.Dir = workdir

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	c := &client{
		cmd:     cmd,
		stdin:   stdin,
		scanner: bufio.NewScanner(stdout),
		msgs:    make(chan rpcMessage, 32),
		readErr: make(chan error, 1),
	}
	c.scanner.Buffer(make([]byte, 0, 64*1024), maxToolReadSize)
	cmd.Stderr = &c.stderr

	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go c.readLoop()
	return c, nil
}

func (c *client) close() error {
	_ = c.stdin.Close()
	if err := c.cmd.Wait(); err != nil {
		return fmt.Errorf("%w; stderr=%s", err, strings.TrimSpace(c.stderr.String()))
	}
	return nil
}

func (c *client) call(id int, method string, params any, timeout time.Duration) (rpcMessage, error) {
	c.recvMu.Lock()
	defer c.recvMu.Unlock()

	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
	}
	if params != nil {
		req["params"] = params
	}
	data, err := json.Marshal(req)
	if err != nil {
		return rpcMessage{}, err
	}
	if _, err := c.stdin.Write(append(data, '\n')); err != nil {
		return rpcMessage{}, err
	}

	select {
	case msg := <-c.msgs:
		for {
			if msg.ID != nil && *msg.ID == id {
				if msg.Error != nil {
					return rpcMessage{}, fmt.Errorf("rpc error %d: %s; stderr=%s", msg.Error.Code, msg.Error.Message, strings.TrimSpace(c.stderr.String()))
				}
				return msg, nil
			}
			select {
			case msg = <-c.msgs:
			case err := <-c.readErr:
				return rpcMessage{}, fmt.Errorf("%w; stderr=%s", err, strings.TrimSpace(c.stderr.String()))
			case <-time.After(timeout):
				return rpcMessage{}, fmt.Errorf("timeout waiting for response id=%d; stderr=%s", id, strings.TrimSpace(c.stderr.String()))
			}
		}
	case err := <-c.readErr:
		return rpcMessage{}, fmt.Errorf("%w; stderr=%s", err, strings.TrimSpace(c.stderr.String()))
	case <-time.After(timeout):
		return rpcMessage{}, fmt.Errorf("timeout waiting for response id=%d; stderr=%s", id, strings.TrimSpace(c.stderr.String()))
	}
}

func (c *client) readLoop() {
	for c.scanner.Scan() {
		var msg rpcMessage
		if err := json.Unmarshal(c.scanner.Bytes(), &msg); err != nil {
			continue
		}
		c.msgs <- msg
	}
	err := c.scanner.Err()
	if err == nil {
		err = fmt.Errorf("mcp stdout closed")
	}
	select {
	case c.readErr <- err:
	default:
	}
}

func decodeJob(msg rpcMessage) (*domain.Job, error) {
	var result toolResult
	if err := json.Unmarshal(msg.Result, &result); err != nil {
		return nil, err
	}
	if len(result.Content) == 0 {
		return nil, errors.New("tool returned no content")
	}
	var job domain.Job
	if err := json.Unmarshal([]byte(result.Content[0].Text), &job); err != nil {
		return nil, err
	}
	return &job, nil
}

func seedRecoverableJobs(cfg Config) ([]domain.Job, error) {
	if !isRecoverableStatus(cfg.RecoveryState) {
		return nil, fmt.Errorf("recovery-state must be starting, running, waiting_leader, or waiting_worker")
	}
	state := store.NewStateStore(filepath.Join(cfg.Workdir, ".gorchera", "state"))
	now := time.Now().UTC()
	jobs := make([]domain.Job, 0, cfg.RecoveryJobs)
	for i := 0; i < cfg.RecoveryJobs; i++ {
		workspace := filepath.Join(cfg.Workdir, fmt.Sprintf("recovery-workspace-%d", i+1))
		if err := os.MkdirAll(workspace, 0o755); err != nil {
			return nil, err
		}
		job := domain.Job{
			ID:                   fmt.Sprintf("recovery-job-%02d", i+1),
			Goal:                 fmt.Sprintf("recovery smoke %d", i+1),
			WorkspaceDir:         workspace,
			StrictnessLevel:      "normal",
			ContextMode:          "full",
			RoleProfiles:         domain.DefaultRoleProfiles(domain.ProviderMock),
			Status:               cfg.RecoveryState,
			Provider:             domain.ProviderMock,
			MaxSteps:             8,
			LeaderContextSummary: fmt.Sprintf("Goal: recovery smoke %d", i+1),
			CreatedAt:            now.Add(time.Duration(i) * time.Millisecond),
			UpdatedAt:            now.Add(time.Duration(i) * time.Millisecond),
		}
		if err := state.SaveJob(context.Background(), &job); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

func seedStaleRecoverableJobs(cfg Config) ([]domain.Job, error) {
	if !isRecoverableStatus(cfg.RecoveryState) {
		return nil, fmt.Errorf("recovery-state must be starting, running, waiting_leader, or waiting_worker")
	}
	state := store.NewStateStore(filepath.Join(cfg.Workdir, ".gorchera", "state"))
	now := time.Now().UTC().Add(-2 * time.Minute)
	jobs := make([]domain.Job, 0, cfg.RecoveryJobs)
	for i := 0; i < cfg.RecoveryJobs; i++ {
		workspace := filepath.Join(cfg.Workdir, fmt.Sprintf("interrupt-workspace-%d", i+1))
		if err := os.MkdirAll(workspace, 0o755); err != nil {
			return nil, err
		}
		job := domain.Job{
			ID:                    fmt.Sprintf("interrupt-job-%02d", i+1),
			Goal:                  fmt.Sprintf("interrupt smoke %d", i+1),
			WorkspaceDir:          workspace,
			RequestedWorkspaceDir: workspace,
			WorkspaceMode:         string(domain.WorkspaceModeShared),
			StrictnessLevel:       "normal",
			ContextMode:           "full",
			RoleProfiles:          domain.DefaultRoleProfiles(domain.ProviderMock),
			Status:                cfg.RecoveryState,
			Provider:              domain.ProviderMock,
			MaxSteps:              8,
			LeaderContextSummary:  fmt.Sprintf("Goal: interrupt smoke %d", i+1),
			RunOwnerID:            fmt.Sprintf("stale-owner-%d", i+1),
			RunHeartbeatAt:        now.Add(time.Duration(i) * time.Millisecond),
			CreatedAt:             now.Add(time.Duration(i) * time.Millisecond),
			UpdatedAt:             now.Add(time.Duration(i) * time.Millisecond),
		}
		if err := state.SaveJob(context.Background(), &job); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

func newGitWorkspace(root, name string) (string, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", fmt.Errorf("git is required for isolated scenario: %w", err)
	}

	workspace := filepath.Join(root, name)
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("# smoke workspace\n"), 0o644); err != nil {
		return "", err
	}
	if err := gitRun(workspace, "init"); err != nil {
		return "", err
	}
	if err := gitRun(workspace, "add", "README.md"); err != nil {
		return "", err
	}
	if err := gitRun(workspace, "-c", "user.name=Smoke", "-c", "user.email=smoke@example.com", "commit", "-m", "init"); err != nil {
		return "", err
	}
	return workspace, nil
}

func gitRun(dir string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if runtime.GOOS == "windows" {
		cmd.Env = append(os.Environ(), "HOME="+dir)
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %v failed: %w: %s", args, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func isRecoverableStatus(status domain.JobStatus) bool {
	switch status {
	case domain.JobStatusStarting, domain.JobStatusPlanning, domain.JobStatusRunning, domain.JobStatusWaitingLeader, domain.JobStatusWaitingWorker:
		return true
	default:
		return false
	}
}
