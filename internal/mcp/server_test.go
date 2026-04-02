package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorchera/internal/domain"
	"gorchera/internal/orchestrator"
	"gorchera/internal/provider"
	"gorchera/internal/provider/mock"
	"gorchera/internal/store"
)

func TestToolStartJobRejectsInvalidWorkspaceBeforeExecution(t *testing.T) {
	t.Parallel()

	server, service, root := newTestServer(t, mock.New())
	missingWorkspace := filepath.Join(root, "missing-workspace")

	resp := server.handleToolCall(mustToolCallRequest(t, "gorchera_start_job", map[string]any{
		"goal":          "Reject invalid MCP workspace",
		"provider":      string(domain.ProviderMock),
		"workspace_dir": missingWorkspace,
	}))
	if resp == nil {
		t.Fatal("expected response")
	}
	if resp.Error != nil {
		t.Fatalf("expected MCP text error result, got rpc error %#v", resp.Error)
	}

	result, ok := resp.Result.(toolResult)
	if !ok {
		t.Fatalf("expected toolResult, got %T", resp.Result)
	}
	text := toolResultText(t, result)
	if !strings.Contains(text, "workspace directory does not exist: "+missingWorkspace) {
		t.Fatalf("expected invalid workspace path in MCP result, got %q", text)
	}

	if _, statErr := os.Stat(filepath.Join(root, "state", "jobs")); !os.IsNotExist(statErr) {
		t.Fatalf("expected no persisted jobs, stat err=%v", statErr)
	}

	select {
	case event := <-service.EventChan():
		t.Fatalf("expected no events for rejected MCP start, got %#v", event)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestToolStartJobAcceptsExistingWorkspace(t *testing.T) {
	t.Parallel()

	server, service, root := newTestServer(t, quickAsyncProvider{})
	workspace := t.TempDir()

	result, err := server.toolStartJob(context.Background(), map[string]any{
		"goal":          "Accept valid MCP workspace",
		"provider":      "quick-async",
		"workspace_dir": workspace,
	})
	if err != nil {
		t.Fatalf("toolStartJob returned error: %v", err)
	}

	var job domain.Job
	if err := json.Unmarshal([]byte(toolResultText(t, result)), &job); err != nil {
		t.Fatalf("failed to decode job result: %v", err)
	}
	if job.WorkspaceDir != workspace {
		t.Fatalf("expected workspace dir %q, got %q", workspace, job.WorkspaceDir)
	}

	jobPath := filepath.Join(root, "state", "jobs", job.ID+".json")
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, statErr := os.Stat(jobPath); statErr == nil {
			break
		} else if !os.IsNotExist(statErr) {
			t.Fatalf("expected persisted job file, got %v", statErr)
		}

		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for persisted job %q", jobPath)
		}
		time.Sleep(10 * time.Millisecond)
	}

	waitForJobStatus(t, service, job.ID, domain.JobStatusBlocked)
}

func TestToolStartChainReturnsChainIDAndStatus(t *testing.T) {
	t.Parallel()

	control := &mcpChainProvider{
		name:    domain.ProviderName("mcp-chain"),
		release: make(chan struct{}),
	}
	server, service, _ := newTestServer(t, control)
	workspace := t.TempDir()

	resp := server.handleToolCall(mustToolCallRequest(t, "gorchera_start_chain", map[string]any{
		"workspace_dir": workspace,
		"goals": []map[string]any{
			{
				"goal":             "hold first",
				"provider":         string(control.name),
				"strictness_level": "lenient",
				"context_mode":     "full",
				"max_steps":        4,
			},
			{
				"goal":             "finish second",
				"provider":         string(control.name),
				"strictness_level": "lenient",
				"context_mode":     "full",
				"max_steps":        4,
			},
		},
	}))
	if resp == nil || resp.Error != nil {
		t.Fatalf("expected successful chain start response, got %#v", resp)
	}

	var started struct {
		ChainID string `json:"chain_id"`
	}
	if err := json.Unmarshal([]byte(toolResultText(t, resp.Result.(toolResult))), &started); err != nil {
		t.Fatalf("failed to decode chain start result: %v", err)
	}
	if started.ChainID == "" {
		t.Fatal("expected non-empty chain id")
	}

	statusResult, err := server.toolChainStatus(context.Background(), map[string]any{"chain_id": started.ChainID})
	if err != nil {
		t.Fatalf("toolChainStatus returned error: %v", err)
	}
	var chain domain.JobChain
	if err := json.Unmarshal([]byte(toolResultText(t, statusResult)), &chain); err != nil {
		t.Fatalf("failed to decode chain status: %v", err)
	}
	if chain.ID != started.ChainID {
		t.Fatalf("expected chain id %q, got %q", started.ChainID, chain.ID)
	}
	if chain.Goals[0].Status != "running" || chain.Goals[0].JobID == "" {
		t.Fatalf("expected first goal running with job id, got %#v", chain.Goals[0])
	}
	if chain.Goals[1].Status != "pending" || chain.Goals[1].JobID != "" {
		t.Fatalf("expected second goal pending, got %#v", chain.Goals[1])
	}

	close(control.release)
	waitForChainStatus(t, service, started.ChainID, "done")
}

func TestStatusToolsExposeWaitSchema(t *testing.T) {
	tools := toolList()

	assertWaitSchema := func(name string) {
		t.Helper()

		for _, tool := range tools {
			if tool.Name != name {
				continue
			}
			prop, ok := tool.InputSchema.Properties["wait"]
			if !ok {
				t.Fatalf("%s schema missing wait property", name)
			}
			if prop.Type != "boolean" {
				t.Fatalf("%s wait property type = %q, want boolean", name, prop.Type)
			}
			timeoutProp, ok := tool.InputSchema.Properties["wait_timeout"]
			if !ok {
				t.Fatalf("%s schema missing wait_timeout property", name)
			}
			if timeoutProp.Type != "integer" {
				t.Fatalf("%s wait_timeout property type = %q, want integer", name, timeoutProp.Type)
			}
			if timeoutProp.Default != 30 {
				t.Fatalf("%s wait_timeout default = %#v, want 30", name, timeoutProp.Default)
			}
			for _, required := range tool.InputSchema.Required {
				if required == "wait" || required == "wait_timeout" {
					t.Fatalf("%s %s property must be optional", name, required)
				}
			}
			return
		}
		t.Fatalf("tool %s not found", name)
	}

	assertWaitSchema("gorchera_status")
	assertWaitSchema("gorchera_chain_status")
}

func TestStatusWaitDurationSemantics(t *testing.T) {
	t.Run("wait disabled", func(t *testing.T) {
		if got := statusWaitDuration(map[string]any{"wait_timeout": 99}, false); got != 0 {
			t.Fatalf("wait=false duration = %v, want 0", got)
		}
	})

	t.Run("omitted wait timeout defaults to 30 seconds", func(t *testing.T) {
		setStatusWaitTimings(t, 20*time.Millisecond, 5*time.Minute)
		if got := statusWaitDuration(map[string]any{}, true); got != 30*time.Second {
			t.Fatalf("omitted wait_timeout duration = %v, want %v", got, 30*time.Second)
		}
	})

	t.Run("zero wait timeout preserves status wait timeout", func(t *testing.T) {
		setStatusWaitTimings(t, 20*time.Millisecond, 150*time.Millisecond)
		if got := statusWaitDuration(map[string]any{"wait_timeout": 0}, true); got != 150*time.Millisecond {
			t.Fatalf("wait_timeout=0 duration = %v, want %v", got, 150*time.Millisecond)
		}
	})

	t.Run("positive wait timeout uses seconds", func(t *testing.T) {
		if got := statusWaitDuration(map[string]any{"wait_timeout": 7}, true); got != 7*time.Second {
			t.Fatalf("wait_timeout=7 duration = %v, want %v", got, 7*time.Second)
		}
	})
}

func TestToolStatusWaitFalseReturnsImmediately(t *testing.T) {
	control := newMCPWaitProvider("mcp-wait-false")
	server, service, _ := newTestServer(t, control)
	workspace := t.TempDir()

	job := startMCPJob(t, server, workspace, string(control.name))
	t.Cleanup(func() {
		cancelJobForCleanup(t, service, job.ID)
	})

	start := time.Now()
	result, err := server.toolStatus(context.Background(), map[string]any{
		"job_id": job.ID,
		"wait":   false,
	})
	if err != nil {
		t.Fatalf("toolStatus returned error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("toolStatus wait=false took too long: %v", elapsed)
	}

	var current domain.Job
	if err := json.Unmarshal([]byte(toolResultText(t, result)), &current); err != nil {
		t.Fatalf("failed to decode job status: %v", err)
	}
	if isTerminalJobStatus(current.Status) {
		t.Fatalf("expected immediate non-terminal snapshot, got %s", current.Status)
	}
}

func TestToolStatusWaitReturnsDoneAfterTerminalState(t *testing.T) {
	setStatusWaitTimings(t, 20*time.Millisecond, time.Second)

	control := newMCPWaitProvider("mcp-wait-done")
	server, _, _ := newTestServer(t, control)
	workspace := t.TempDir()

	job := startMCPJob(t, server, workspace, string(control.name))

	go func() {
		time.Sleep(20 * time.Millisecond)
		control.releaseNow()
	}()

	result, err := server.toolStatus(context.Background(), map[string]any{
		"job_id": job.ID,
		"wait":   true,
	})
	if err != nil {
		t.Fatalf("toolStatus returned error: %v", err)
	}

	var current domain.Job
	if err := json.Unmarshal([]byte(toolResultText(t, result)), &current); err != nil {
		t.Fatalf("failed to decode job status: %v", err)
	}
	if current.Status != domain.JobStatusDone {
		t.Fatalf("expected done status, got %s", current.Status)
	}
}

func TestToolStatusWaitReturnsBlockedForOperatorCancellation(t *testing.T) {
	setStatusWaitTimings(t, 20*time.Millisecond, time.Second)

	control := newMCPWaitProvider("mcp-wait-cancel")
	server, service, _ := newTestServer(t, control)
	workspace := t.TempDir()

	job := startMCPJob(t, server, workspace, string(control.name))

	go func() {
		time.Sleep(20 * time.Millisecond)
		_, _ = service.Cancel(context.Background(), job.ID, "operator stop")
	}()

	result, err := server.toolStatus(context.Background(), map[string]any{
		"job_id": job.ID,
		"wait":   true,
	})
	if err != nil {
		t.Fatalf("toolStatus returned error: %v", err)
	}

	var current domain.Job
	if err := json.Unmarshal([]byte(toolResultText(t, result)), &current); err != nil {
		t.Fatalf("failed to decode job status: %v", err)
	}
	if current.Status != domain.JobStatusBlocked {
		t.Fatalf("expected blocked status for operator cancellation, got %s", current.Status)
	}
	if !strings.Contains(strings.ToLower(current.BlockedReason), "cancelled by operator") {
		t.Fatalf("expected operator cancellation reason, got %q", current.BlockedReason)
	}
}

func TestToolStatusWaitTimeoutReturnsLatestSnapshot(t *testing.T) {
	setStatusWaitTimings(t, 20*time.Millisecond, 120*time.Millisecond)

	control := newMCPWaitProvider("mcp-wait-timeout")
	server, service, _ := newTestServer(t, control)
	workspace := t.TempDir()

	job := startMCPJob(t, server, workspace, string(control.name))
	t.Cleanup(func() {
		cancelJobForCleanup(t, service, job.ID)
	})

	start := time.Now()
	result, err := server.toolStatus(context.Background(), map[string]any{
		"job_id": job.ID,
		"wait":   true,
	})
	if err != nil {
		t.Fatalf("toolStatus returned error: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("toolStatus wait timeout returned too quickly: %v", elapsed)
	}

	var current domain.Job
	if err := json.Unmarshal([]byte(toolResultText(t, result)), &current); err != nil {
		t.Fatalf("failed to decode job status: %v", err)
	}
	if isTerminalJobStatus(current.Status) {
		t.Fatalf("expected non-terminal snapshot after timeout, got %s", current.Status)
	}
}

func TestToolStatusWaitTimeoutZeroUsesStatusWaitTimeout(t *testing.T) {
	setStatusWaitTimings(t, 20*time.Millisecond, 120*time.Millisecond)

	control := newMCPWaitProvider("mcp-wait-timeout-zero")
	server, service, _ := newTestServer(t, control)
	workspace := t.TempDir()

	job := startMCPJob(t, server, workspace, string(control.name))
	t.Cleanup(func() {
		cancelJobForCleanup(t, service, job.ID)
	})

	start := time.Now()
	result, err := server.toolStatus(context.Background(), map[string]any{
		"job_id":       job.ID,
		"wait":         true,
		"wait_timeout": 0,
	})
	if err != nil {
		t.Fatalf("toolStatus returned error: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("toolStatus wait_timeout=0 returned too quickly: %v", elapsed)
	}

	var current domain.Job
	if err := json.Unmarshal([]byte(toolResultText(t, result)), &current); err != nil {
		t.Fatalf("failed to decode job status: %v", err)
	}
	if isTerminalJobStatus(current.Status) {
		t.Fatalf("expected non-terminal snapshot after wait_timeout=0 timeout, got %s", current.Status)
	}
}

func TestToolStatusWaitTimeoutPositiveUsesProvidedSeconds(t *testing.T) {
	setStatusWaitTimings(t, 10*time.Millisecond, 5*time.Minute)

	control := newMCPWaitProvider("mcp-wait-timeout-positive")
	server, service, _ := newTestServer(t, control)
	workspace := t.TempDir()

	job := startMCPJob(t, server, workspace, string(control.name))
	t.Cleanup(func() {
		cancelJobForCleanup(t, service, job.ID)
	})

	start := time.Now()
	result, err := server.toolStatus(context.Background(), map[string]any{
		"job_id":       job.ID,
		"wait":         true,
		"wait_timeout": 1,
	})
	if err != nil {
		t.Fatalf("toolStatus returned error: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("toolStatus wait_timeout=1 returned too quickly: %v", elapsed)
	}

	var current domain.Job
	if err := json.Unmarshal([]byte(toolResultText(t, result)), &current); err != nil {
		t.Fatalf("failed to decode job status: %v", err)
	}
	if isTerminalJobStatus(current.Status) {
		t.Fatalf("expected non-terminal snapshot after positive wait timeout, got %s", current.Status)
	}
}

func TestToolChainStatusWaitReturnsDoneAfterTerminalState(t *testing.T) {
	setStatusWaitTimings(t, 20*time.Millisecond, time.Second)

	control := &mcpChainProvider{
		name:    domain.ProviderName("mcp-chain-wait"),
		release: make(chan struct{}),
	}
	server, _, _ := newTestServer(t, control)
	workspace := t.TempDir()

	chainID := startMCPChain(t, server, workspace, string(control.name))

	go func() {
		time.Sleep(20 * time.Millisecond)
		close(control.release)
	}()

	result, err := server.toolChainStatus(context.Background(), map[string]any{
		"chain_id": chainID,
		"wait":     true,
	})
	if err != nil {
		t.Fatalf("toolChainStatus returned error: %v", err)
	}

	var chain domain.JobChain
	if err := json.Unmarshal([]byte(toolResultText(t, result)), &chain); err != nil {
		t.Fatalf("failed to decode chain status: %v", err)
	}
	if chain.Status != domain.ChainStatusDone {
		t.Fatalf("expected done chain status, got %s", chain.Status)
	}
}

func TestToolChainStatusWaitTimeoutReturnsLatestSnapshot(t *testing.T) {
	setStatusWaitTimings(t, 20*time.Millisecond, 120*time.Millisecond)

	control := &mcpChainProvider{
		name:    domain.ProviderName("mcp-chain-timeout"),
		release: make(chan struct{}),
	}
	server, service, _ := newTestServer(t, control)
	workspace := t.TempDir()

	chainID := startMCPChain(t, server, workspace, string(control.name))
	t.Cleanup(func() {
		cancelChainForCleanup(t, service, chainID)
	})

	start := time.Now()
	result, err := server.toolChainStatus(context.Background(), map[string]any{
		"chain_id": chainID,
		"wait":     true,
	})
	if err != nil {
		t.Fatalf("toolChainStatus returned error: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("toolChainStatus wait timeout returned too quickly: %v", elapsed)
	}

	var chain domain.JobChain
	if err := json.Unmarshal([]byte(toolResultText(t, result)), &chain); err != nil {
		t.Fatalf("failed to decode chain status: %v", err)
	}
	if isTerminalChainStatus(chain.Status) {
		t.Fatalf("expected non-terminal chain snapshot after timeout, got %s", chain.Status)
	}
}

func TestToolChainStatusWaitTimeoutZeroUsesStatusWaitTimeout(t *testing.T) {
	setStatusWaitTimings(t, 20*time.Millisecond, 120*time.Millisecond)

	control := &mcpChainProvider{
		name:    domain.ProviderName("mcp-chain-timeout-zero"),
		release: make(chan struct{}),
	}
	server, service, _ := newTestServer(t, control)
	workspace := t.TempDir()

	chainID := startMCPChain(t, server, workspace, string(control.name))
	t.Cleanup(func() {
		cancelChainForCleanup(t, service, chainID)
	})

	start := time.Now()
	result, err := server.toolChainStatus(context.Background(), map[string]any{
		"chain_id":     chainID,
		"wait":         true,
		"wait_timeout": 0,
	})
	if err != nil {
		t.Fatalf("toolChainStatus returned error: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("toolChainStatus wait_timeout=0 returned too quickly: %v", elapsed)
	}

	var chain domain.JobChain
	if err := json.Unmarshal([]byte(toolResultText(t, result)), &chain); err != nil {
		t.Fatalf("failed to decode chain status: %v", err)
	}
	if isTerminalChainStatus(chain.Status) {
		t.Fatalf("expected non-terminal chain snapshot after wait_timeout=0 timeout, got %s", chain.Status)
	}
}

func TestToolChainStatusWaitTimeoutPositiveUsesProvidedSeconds(t *testing.T) {
	setStatusWaitTimings(t, 10*time.Millisecond, 5*time.Minute)

	control := &mcpChainProvider{
		name:    domain.ProviderName("mcp-chain-timeout-positive"),
		release: make(chan struct{}),
	}
	server, service, _ := newTestServer(t, control)
	workspace := t.TempDir()

	chainID := startMCPChain(t, server, workspace, string(control.name))
	t.Cleanup(func() {
		cancelChainForCleanup(t, service, chainID)
	})

	start := time.Now()
	result, err := server.toolChainStatus(context.Background(), map[string]any{
		"chain_id":     chainID,
		"wait":         true,
		"wait_timeout": 1,
	})
	if err != nil {
		t.Fatalf("toolChainStatus returned error: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("toolChainStatus wait_timeout=1 returned too quickly: %v", elapsed)
	}

	var chain domain.JobChain
	if err := json.Unmarshal([]byte(toolResultText(t, result)), &chain); err != nil {
		t.Fatalf("failed to decode chain status: %v", err)
	}
	if isTerminalChainStatus(chain.Status) {
		t.Fatalf("expected non-terminal chain snapshot after positive wait timeout, got %s", chain.Status)
	}
}

func TestToolStartJobRejectsRelativeWorkspaceBeforeExecution(t *testing.T) {
	t.Parallel()

	server, service, root := newTestServer(t, mock.New())

	resp := server.handleToolCall(mustToolCallRequest(t, "gorchera_start_job", map[string]any{
		"goal":          "Reject relative MCP workspace",
		"provider":      string(domain.ProviderMock),
		"workspace_dir": "relative\\workspace",
	}))
	if resp == nil {
		t.Fatal("expected response")
	}
	if resp.Error != nil {
		t.Fatalf("expected MCP text error result, got rpc error %#v", resp.Error)
	}

	result, ok := resp.Result.(toolResult)
	if !ok {
		t.Fatalf("expected toolResult, got %T", resp.Result)
	}
	text := toolResultText(t, result)
	if !strings.Contains(text, "workspace directory must be an absolute path") {
		t.Fatalf("expected relative workspace error, got %q", text)
	}

	if _, statErr := os.Stat(filepath.Join(root, "state", "jobs")); !os.IsNotExist(statErr) {
		t.Fatalf("expected no persisted jobs, stat err=%v", statErr)
	}

	select {
	case event := <-service.EventChan():
		t.Fatalf("expected no events for rejected MCP start, got %#v", event)
	case <-time.After(50 * time.Millisecond):
	}
}

func newTestServer(t *testing.T, adapters ...provider.Adapter) (*Server, *orchestrator.Service, string) {
	t.Helper()

	root := t.TempDir()
	registry := provider.NewRegistry()
	for _, adapter := range adapters {
		registry.Register(adapter)
	}

	service := orchestrator.NewService(
		provider.NewSessionManager(registry),
		store.NewStateStore(filepath.Join(root, "state")),
		store.NewArtifactStore(filepath.Join(root, "artifacts")),
		root,
	)

	return NewServer(service), service, root
}

func setStatusWaitTimings(t *testing.T, poll, timeout time.Duration) {
	t.Helper()

	oldPoll := statusWaitPollInterval
	oldTimeout := statusWaitTimeout
	statusWaitPollInterval = poll
	statusWaitTimeout = timeout
	t.Cleanup(func() {
		statusWaitPollInterval = oldPoll
		statusWaitTimeout = oldTimeout
	})
}

func startMCPJob(t *testing.T, server *Server, workspace, providerName string) domain.Job {
	t.Helper()

	result, err := server.toolStartJob(context.Background(), map[string]any{
		"goal":             "wait-aware MCP status test",
		"provider":         providerName,
		"workspace_dir":    workspace,
		"strictness_level": "lenient",
	})
	if err != nil {
		t.Fatalf("toolStartJob returned error: %v", err)
	}

	var job domain.Job
	if err := json.Unmarshal([]byte(toolResultText(t, result)), &job); err != nil {
		t.Fatalf("failed to decode job result: %v", err)
	}
	return job
}

func startMCPChain(t *testing.T, server *Server, workspace, providerName string) string {
	t.Helper()

	result, err := server.toolStartChain(context.Background(), map[string]any{
		"workspace_dir": workspace,
		"goals": []any{
			map[string]any{
				"goal":             "hold first",
				"provider":         providerName,
				"strictness_level": "lenient",
				"context_mode":     "full",
				"max_steps":        4,
			},
			map[string]any{
				"goal":             "finish second",
				"provider":         providerName,
				"strictness_level": "lenient",
				"context_mode":     "full",
				"max_steps":        4,
			},
		},
	})
	if err != nil {
		t.Fatalf("toolStartChain returned error: %v", err)
	}

	var started struct {
		ChainID string `json:"chain_id"`
	}
	if err := json.Unmarshal([]byte(toolResultText(t, result)), &started); err != nil {
		t.Fatalf("failed to decode chain start result: %v", err)
	}
	if started.ChainID == "" {
		t.Fatal("expected non-empty chain id")
	}
	return started.ChainID
}

type quickAsyncProvider struct{}

func (quickAsyncProvider) Name() domain.ProviderName {
	return domain.ProviderName("quick-async")
}

func (quickAsyncProvider) RunLeader(_ context.Context, _ domain.Job) (string, error) {
	return `{"action":"complete","target":"none","task_type":"none","reason":"complete immediately"}`, nil
}

func (quickAsyncProvider) RunWorker(_ context.Context, _ domain.Job, _ domain.LeaderOutput) (string, error) {
	return `{"status":"success","summary":"quick async worker completed","artifacts":["worker-output.json"]}`, nil
}

type mcpWaitProvider struct {
	name    domain.ProviderName
	release chan struct{}
}

func newMCPWaitProvider(name string) *mcpWaitProvider {
	return &mcpWaitProvider{
		name:    domain.ProviderName(name),
		release: make(chan struct{}),
	}
}

func (p *mcpWaitProvider) Name() domain.ProviderName {
	return p.name
}

func (p *mcpWaitProvider) RunLeader(ctx context.Context, _ domain.Job) (string, error) {
	select {
	case <-p.release:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return `{"action":"complete","target":"none","task_type":"none","reason":"wait provider complete"}`, nil
}

func (p *mcpWaitProvider) RunWorker(_ context.Context, _ domain.Job, _ domain.LeaderOutput) (string, error) {
	return `{"status":"success","summary":"unused","artifacts":[]}`, nil
}

func (p *mcpWaitProvider) RunEvaluator(_ context.Context, _ domain.Job) (string, error) {
	return `{"status":"passed","passed":true,"score":100,"reason":"accepted","missing_step_types":[],"evidence":["wait"],"contract_ref":"","verification_report":{"status":"passed","passed":true,"reason":"accepted","evidence":["wait"],"missing_checks":[],"artifacts":[],"contract_ref":""}}`, nil
}

func (p *mcpWaitProvider) releaseNow() {
	select {
	case <-p.release:
	default:
		close(p.release)
	}
}

func mustToolCallRequest(t *testing.T, name string, args map[string]any) jsonRPCRequest {
	t.Helper()

	params, err := json.Marshal(map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		t.Fatalf("failed to marshal tool request: %v", err)
	}

	return jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      "req-1",
		Method:  "tools/call",
		Params:  params,
	}
}

func toolResultText(t *testing.T, result toolResult) string {
	t.Helper()

	if len(result.Content) != 1 {
		t.Fatalf("expected one content item, got %d", len(result.Content))
	}
	return result.Content[0].Text
}

func waitForJobStatus(t *testing.T, service *orchestrator.Service, jobID string, want domain.JobStatus) {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)
	for {
		job, err := service.Get(context.Background(), jobID)
		if err == nil && job.Status == want {
			return
		}

		if time.Now().After(deadline) {
			if err != nil {
				t.Fatalf("timed out waiting for job %s: %v", jobID, err)
			}
			t.Fatalf("timed out waiting for job %s status %s, got %s", jobID, want, job.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForChainStatus(t *testing.T, service *orchestrator.Service, chainID string, want string) {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)
	for {
		chain, err := service.GetChain(context.Background(), chainID)
		if err == nil && chain.Status == want {
			return
		}

		if time.Now().After(deadline) {
			if err != nil {
				t.Fatalf("timed out waiting for chain %s: %v", chainID, err)
			}
			t.Fatalf("timed out waiting for chain %s status %s, got %s", chainID, want, chain.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func cancelJobForCleanup(t *testing.T, service *orchestrator.Service, jobID string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for {
		job, err := service.Cancel(context.Background(), jobID, "cleanup")
		if err == nil && isTerminalJobStatus(job.Status) {
			time.Sleep(50 * time.Millisecond)
			return
		}
		if time.Now().After(deadline) {
			if err != nil {
				t.Fatalf("failed to cancel job %s during cleanup: %v", jobID, err)
			}
			t.Fatalf("job %s did not reach a terminal state during cleanup", jobID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func cancelChainForCleanup(t *testing.T, service *orchestrator.Service, chainID string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for {
		chain, err := service.CancelChain(context.Background(), chainID, "cleanup")
		if err == nil && isTerminalChainStatus(chain.Status) {
			time.Sleep(50 * time.Millisecond)
			return
		}
		if time.Now().After(deadline) {
			if err != nil {
				t.Fatalf("failed to cancel chain %s during cleanup: %v", chainID, err)
			}
			t.Fatalf("chain %s did not reach a terminal state during cleanup", chainID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type mcpChainProvider struct {
	name    domain.ProviderName
	release chan struct{}
}

func (p *mcpChainProvider) Name() domain.ProviderName {
	return p.name
}

func (p *mcpChainProvider) RunLeader(ctx context.Context, job domain.Job) (string, error) {
	if strings.Contains(job.Goal, "hold") {
		select {
		case <-p.release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return `{"action":"complete","target":"none","task_type":"none","reason":"chain goal complete"}`, nil
}

func (p *mcpChainProvider) RunWorker(_ context.Context, _ domain.Job, _ domain.LeaderOutput) (string, error) {
	return `{"status":"success","summary":"unused","artifacts":[],"blocked_reason":"","error_reason":"","next_recommended_action":""}`, nil
}

func (p *mcpChainProvider) RunEvaluator(_ context.Context, _ domain.Job) (string, error) {
	return `{"status":"passed","passed":true,"score":100,"reason":"accepted","missing_step_types":[],"evidence":["chain"],"contract_ref":"","verification_report":{"status":"passed","passed":true,"reason":"accepted","evidence":["chain"],"missing_checks":[],"artifacts":[],"contract_ref":""}}`, nil
}

// TestToolApproveLogsErrorOnFailure verifies that when the background Approve
// goroutine encounters an error, it logs it rather than silently discarding it.
// We simulate this by putting a job in a state where Approve will fail
// (not waiting_approval), so the goroutine logs the error.
func TestToolApproveLogsErrorOnFailure(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	registry := provider.NewRegistry()
	svc := orchestrator.NewService(
		provider.NewSessionManager(registry),
		store.NewStateStore(filepath.Join(root, "state")),
		store.NewArtifactStore(filepath.Join(root, "artifacts")),
		root,
	)
	server := NewServer(svc)

	// Save a job in waiting_leader status; Approve will fail because the job is
	// not in waiting_approval state.
	job := &domain.Job{
		ID:           "job-approve-log-test",
		Goal:         "log error test",
		WorkspaceDir: root,
		Status:       domain.JobStatusWaitingLeader,
		Provider:     domain.ProviderMock,
		RoleProfiles: domain.DefaultRoleProfiles(domain.ProviderMock),
		MaxSteps:     1,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	stateStore := store.NewStateStore(filepath.Join(root, "state"))
	if err := stateStore.SaveJob(context.Background(), job); err != nil {
		t.Fatalf("failed to save job: %v", err)
	}

	// Redirect the default logger so we can capture output.
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	// toolApprove calls Get (succeeds) then fires a goroutine calling Approve.
	_, _ = server.toolApprove(context.Background(), map[string]any{"job_id": job.ID})

	// Give the background goroutine time to run and log.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if strings.Contains(buf.String(), "[gorchera] Approve failed for job "+job.ID) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected Approve error to be logged; log output: %q", buf.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
