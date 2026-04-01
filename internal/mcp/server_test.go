package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorechera/internal/domain"
	"gorechera/internal/orchestrator"
	"gorechera/internal/provider"
	"gorechera/internal/provider/mock"
	"gorechera/internal/store"
)

func TestToolStartJobRejectsInvalidWorkspaceBeforeExecution(t *testing.T) {
	t.Parallel()

	server, service, root := newTestServer(t, mock.New())
	missingWorkspace := filepath.Join(root, "missing-workspace")

	resp := server.handleToolCall(mustToolCallRequest(t, "gorechera_start_job", map[string]any{
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

func TestToolStartJobRejectsRelativeWorkspaceBeforeExecution(t *testing.T) {
	t.Parallel()

	server, service, root := newTestServer(t, mock.New())

	resp := server.handleToolCall(mustToolCallRequest(t, "gorechera_start_job", map[string]any{
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

	deadline := time.Now().Add(5 * time.Second)
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
