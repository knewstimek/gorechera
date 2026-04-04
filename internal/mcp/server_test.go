package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
		"goal":           "Accept valid MCP workspace",
		"provider":       "quick-async",
		"workspace_dir":  workspace,
		"ambition_level": domain.AmbitionLevelHigh,
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
	if job.AmbitionLevel != domain.AmbitionLevelHigh {
		t.Fatalf("expected ambition level %q, got %q", domain.AmbitionLevelHigh, job.AmbitionLevel)
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

func TestToolStartJobPersistsRoleOverrides(t *testing.T) {
	t.Parallel()

	server, service, _ := newTestServer(t, quickAsyncProvider{})
	workspace := t.TempDir()

	result, err := server.toolStartJob(context.Background(), map[string]any{
		"goal":          "Persist role overrides",
		"provider":      "quick-async",
		"workspace_dir": workspace,
		"pipeline_mode": "full",
		"role_overrides": map[string]any{
			"director": map[string]any{
				"provider": "quick-async",
				"model":    "opus",
			},
			"executor": map[string]any{
				"provider": "quick-async",
				"model":    "sonnet",
			},
		},
	})
	if err != nil {
		t.Fatalf("toolStartJob returned error: %v", err)
	}

	var job domain.Job
	if err := json.Unmarshal([]byte(toolResultText(t, result)), &job); err != nil {
		t.Fatalf("failed to decode job result: %v", err)
	}
	if got := job.RoleOverrides["director"].Model; got != "opus" {
		t.Fatalf("director override model = %q, want %q", got, "opus")
	}
	if got := string(job.RoleOverrides["executor"].Provider); got != "quick-async" {
		t.Fatalf("executor override provider = %q, want %q", got, "quick-async")
	}

	waitForJobStatus(t, service, job.ID, domain.JobStatusBlocked)
}

func TestToolStartChainPersistsRoleOverridesPerGoal(t *testing.T) {
	t.Parallel()

	control := &mcpChainProvider{
		name:    domain.ProviderName("mcp-chain-overrides"),
		release: make(chan struct{}),
	}
	server, service, _ := newTestServer(t, control)
	workspace := t.TempDir()

	result, err := server.toolStartChain(context.Background(), map[string]any{
		"workspace_dir": workspace,
		"goals": []any{
			map[string]any{
				"goal":     "hold first",
				"provider": string(control.name),
				"role_overrides": map[string]any{
					" evaluator ": map[string]any{
						"provider": string(control.name),
						"model":    "opus",
					},
				},
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
		t.Fatalf("failed to decode chain result: %v", err)
	}
	chain, err := service.GetChain(context.Background(), started.ChainID)
	if err != nil {
		t.Fatalf("GetChain returned error: %v", err)
	}
	if got := chain.Goals[0].RoleOverrides["evaluator"].Model; got != "opus" {
		t.Fatalf("chain goal evaluator override model = %q, want %q", got, "opus")
	}

	job, err := service.Get(context.Background(), chain.Goals[0].JobID)
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if got := job.RoleOverrides["evaluator"].Provider; got != control.name {
		t.Fatalf("job evaluator override provider = %q, want %q", got, control.name)
	}

	close(control.release)
	waitForTerminalChainStatus(t, service, started.ChainID)
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
				"ambition_level":   domain.AmbitionLevelLow,
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
	if chain.Goals[0].AmbitionLevel != domain.AmbitionLevelLow {
		t.Fatalf("expected first chain goal ambition level %q, got %q", domain.AmbitionLevelLow, chain.Goals[0].AmbitionLevel)
	}
	if chain.Goals[1].Status != "pending" || chain.Goals[1].JobID != "" {
		t.Fatalf("expected second goal pending, got %#v", chain.Goals[1])
	}
	if chain.Goals[1].AmbitionLevel != domain.AmbitionLevelMedium {
		t.Fatalf("expected second chain goal default ambition level %q, got %q", domain.AmbitionLevelMedium, chain.Goals[1].AmbitionLevel)
	}

	jobResult, err := server.toolStatus(context.Background(), map[string]any{"job_id": chain.Goals[0].JobID, "compact": false})
	if err != nil {
		t.Fatalf("toolStatus returned error: %v", err)
	}
	var job domain.Job
	if err := json.Unmarshal([]byte(toolResultText(t, jobResult)), &job); err != nil {
		t.Fatalf("failed to decode job status: %v", err)
	}
	if job.AmbitionLevel != domain.AmbitionLevelLow {
		t.Fatalf("expected first chained job ambition level %q, got %q", domain.AmbitionLevelLow, job.AmbitionLevel)
	}

	close(control.release)
	waitForTerminalChainStatus(t, service, started.ChainID)
}

func TestStartAndResumeToolSchemasExposePipelineControls(t *testing.T) {
	t.Parallel()

	tools := toolList()
	var foundJob bool
	var foundChain bool
	var foundResume bool

	for _, tool := range tools {
		switch tool.Name {
		case "gorchera_start_job":
			foundJob = true
			prop, ok := tool.InputSchema.Properties["ambition_level"]
			if !ok {
				t.Fatal("gorchera_start_job schema missing ambition_level")
			}
			if prop.Type != "string" {
				t.Fatalf("gorchera_start_job ambition_level type = %q, want string", prop.Type)
			}
			if prop.Default != domain.AmbitionLevelMedium {
				t.Fatalf("gorchera_start_job ambition_level default = %#v, want %q", prop.Default, domain.AmbitionLevelMedium)
			}
			pipelineProp, ok := tool.InputSchema.Properties["pipeline_mode"]
			if !ok {
				t.Fatal("gorchera_start_job schema missing pipeline_mode")
			}
			if pipelineProp.Type != "string" {
				t.Fatalf("gorchera_start_job pipeline_mode type = %q, want string", pipelineProp.Type)
			}
			if pipelineProp.Default != "balanced" {
				t.Fatalf("gorchera_start_job pipeline_mode default = %#v, want %q", pipelineProp.Default, "balanced")
			}
			if len(pipelineProp.Enum) != 3 {
				t.Fatalf("gorchera_start_job pipeline_mode enum = %#v, want 3 entries", pipelineProp.Enum)
			}
			roleOverridesProp, ok := tool.InputSchema.Properties["role_overrides"]
			if !ok {
				t.Fatal("gorchera_start_job schema missing role_overrides")
			}
			if roleOverridesProp.Type != "object" {
				t.Fatalf("gorchera_start_job role_overrides type = %q, want object", roleOverridesProp.Type)
			}
			if _, ok := roleOverridesProp.Properties["director"]; !ok {
				t.Fatal("gorchera_start_job role_overrides missing director")
			}
			if _, ok := roleOverridesProp.Properties["executor"]; !ok {
				t.Fatal("gorchera_start_job role_overrides missing executor")
			}
			directorProp := roleOverridesProp.Properties["director"]
			if _, ok := directorProp.Properties["provider"]; !ok {
				t.Fatal("gorchera_start_job role_overrides.director missing provider")
			}
			if _, ok := directorProp.Properties["model"]; !ok {
				t.Fatal("gorchera_start_job role_overrides.director missing model")
			}
		case "gorchera_start_chain":
			foundChain = true
			goalsProp, ok := tool.InputSchema.Properties["goals"]
			if !ok || goalsProp.Items == nil {
				t.Fatal("gorchera_start_chain schema missing goals items")
			}
			prop, ok := goalsProp.Items.Properties["ambition_level"]
			if !ok {
				t.Fatal("gorchera_start_chain goal schema missing ambition_level")
			}
			if prop.Type != "string" {
				t.Fatalf("gorchera_start_chain ambition_level type = %q, want string", prop.Type)
			}
			if prop.Default != domain.AmbitionLevelMedium {
				t.Fatalf("gorchera_start_chain ambition_level default = %#v, want %q", prop.Default, domain.AmbitionLevelMedium)
			}
			roleOverridesProp, ok := goalsProp.Items.Properties["role_overrides"]
			if !ok {
				t.Fatal("gorchera_start_chain goal schema missing role_overrides")
			}
			if roleOverridesProp.Type != "object" {
				t.Fatalf("gorchera_start_chain role_overrides type = %q, want object", roleOverridesProp.Type)
			}
			if _, ok := roleOverridesProp.Properties["director"]; !ok {
				t.Fatal("gorchera_start_chain role_overrides missing director")
			}
			directorProp := roleOverridesProp.Properties["director"]
			if _, ok := directorProp.Properties["provider"]; !ok {
				t.Fatal("gorchera_start_chain role_overrides.director missing provider")
			}
			if _, ok := directorProp.Properties["model"]; !ok {
				t.Fatal("gorchera_start_chain role_overrides.director missing model")
			}
		case "gorchera_resume":
			foundResume = true
			prop, ok := tool.InputSchema.Properties["extra_steps"]
			if !ok {
				t.Fatal("gorchera_resume schema missing extra_steps")
			}
			if prop.Type != "integer" {
				t.Fatalf("gorchera_resume extra_steps type = %q, want integer", prop.Type)
			}
			if prop.Minimum == nil || *prop.Minimum != 1 {
				t.Fatalf("gorchera_resume extra_steps minimum = %#v, want 1", prop.Minimum)
			}
			if prop.Maximum == nil || *prop.Maximum != 20 {
				t.Fatalf("gorchera_resume extra_steps maximum = %#v, want 20", prop.Maximum)
			}
		}
	}

	if !foundJob {
		t.Fatal("gorchera_start_job schema not found")
	}
	if !foundChain {
		t.Fatal("gorchera_start_chain schema not found")
	}
	if !foundResume {
		t.Fatal("gorchera_resume schema not found")
	}
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

func TestOptionalExtraStepsArgValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args map[string]any
		want int
		err  string
	}{
		{name: "missing", args: map[string]any{}, want: 0},
		{name: "valid", args: map[string]any{"extra_steps": 5}, want: 5},
		{name: "fractional", args: map[string]any{"extra_steps": 1.5}, err: "extra_steps must be an integer"},
		{name: "too small", args: map[string]any{"extra_steps": 0}, err: "extra_steps must be between 1 and 20"},
		{name: "too large", args: map[string]any{"extra_steps": 21}, err: "extra_steps must be between 1 and 20"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := optionalExtraStepsArg(tc.args)
			if tc.err != "" {
				if err == nil || err.Error() != tc.err {
					t.Fatalf("optionalExtraStepsArg error = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("optionalExtraStepsArg returned error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("optionalExtraStepsArg = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestDiffToolSchemaExposed(t *testing.T) {
	t.Parallel()

	tools := toolList()
	for _, tool := range tools {
		if tool.Name != "gorchera_diff" {
			continue
		}
		jobIDProp, ok := tool.InputSchema.Properties["job_id"]
		if !ok {
			t.Fatal("gorchera_diff schema missing job_id")
		}
		if jobIDProp.Type != "string" {
			t.Fatalf("gorchera_diff job_id type = %q, want string", jobIDProp.Type)
		}
		pathspecProp, ok := tool.InputSchema.Properties["pathspec"]
		if !ok {
			t.Fatal("gorchera_diff schema missing pathspec")
		}
		if pathspecProp.Type != "string" {
			t.Fatalf("gorchera_diff pathspec type = %q, want string", pathspecProp.Type)
		}
		if len(tool.InputSchema.Required) != 1 || tool.InputSchema.Required[0] != "job_id" {
			t.Fatalf("gorchera_diff required = %#v, want [job_id]", tool.InputSchema.Required)
		}
		return
	}
	t.Fatal("gorchera_diff schema not found")
}

func TestToolDiffReturnsSharedWorkspaceDiff(t *testing.T) {
	t.Parallel()

	server, _, root := newTestServer(t, mock.New())
	workspace := newGitWorkspace(t)
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("# test workspace\nshared change\n"), 0o644); err != nil {
		t.Fatalf("failed to modify shared workspace: %v", err)
	}
	job := saveDiffTestJob(t, root, "job-diff-shared", workspace, workspace, string(domain.WorkspaceModeShared))

	result, err := server.toolDiff(context.Background(), map[string]any{"job_id": job.ID})
	if err != nil {
		t.Fatalf("toolDiff returned error: %v", err)
	}
	text := toolResultText(t, result)
	if !strings.Contains(text, "diff --git a/README.md b/README.md") {
		t.Fatalf("expected README diff, got %q", text)
	}
	if !strings.Contains(text, "+shared change") {
		t.Fatalf("expected modified README content in diff, got %q", text)
	}
}

func TestToolDiffReturnsIsolatedWorkspaceDiff(t *testing.T) {
	t.Parallel()

	server, _, root := newTestServer(t, mock.New())
	workspace := newGitWorkspace(t)
	worktreeDir := filepath.Join(t.TempDir(), "isolated-worktree")
	gitRun(t, workspace, "worktree", "add", "--detach", worktreeDir, "HEAD")
	if err := os.WriteFile(filepath.Join(worktreeDir, "README.md"), []byte("# test workspace\nisolated change\n"), 0o644); err != nil {
		t.Fatalf("failed to modify isolated workspace: %v", err)
	}
	job := saveDiffTestJob(t, root, "job-diff-isolated", worktreeDir, workspace, string(domain.WorkspaceModeIsolated))

	result, err := server.toolDiff(context.Background(), map[string]any{"job_id": job.ID})
	if err != nil {
		t.Fatalf("toolDiff returned error: %v", err)
	}
	text := toolResultText(t, result)
	if !strings.Contains(text, "diff --git a/README.md b/README.md") {
		t.Fatalf("expected README diff, got %q", text)
	}
	if !strings.Contains(text, "+isolated change") {
		t.Fatalf("expected isolated README content in diff, got %q", text)
	}
}

func TestToolDiffPathspecRestrictsOutput(t *testing.T) {
	t.Parallel()

	server, _, root := newTestServer(t, mock.New())
	workspace := newGitWorkspace(t)
	if err := os.WriteFile(filepath.Join(workspace, "notes.txt"), []byte("notes base\n"), 0o644); err != nil {
		t.Fatalf("failed to seed notes.txt: %v", err)
	}
	gitRun(t, workspace, "add", "notes.txt")
	gitRun(t, workspace, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "add notes")
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("# test workspace\nreadme change\n"), 0o644); err != nil {
		t.Fatalf("failed to modify README: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "notes.txt"), []byte("notes change\n"), 0o644); err != nil {
		t.Fatalf("failed to modify notes.txt: %v", err)
	}
	job := saveDiffTestJob(t, root, "job-diff-pathspec", workspace, workspace, string(domain.WorkspaceModeShared))

	result, err := server.toolDiff(context.Background(), map[string]any{
		"job_id":   job.ID,
		"pathspec": "README.md",
	})
	if err != nil {
		t.Fatalf("toolDiff returned error: %v", err)
	}
	text := toolResultText(t, result)
	if !strings.Contains(text, "diff --git a/README.md b/README.md") {
		t.Fatalf("expected README diff, got %q", text)
	}
	if strings.Contains(text, "notes.txt") {
		t.Fatalf("expected notes.txt to be filtered out, got %q", text)
	}
}

func TestToolDiffEdgeCases(t *testing.T) {
	t.Parallel()

	server, _, root := newTestServer(t, mock.New())

	t.Run("unknown job", func(t *testing.T) {
		resp := server.handleToolCall(mustToolCallRequest(t, "gorchera_diff", map[string]any{
			"job_id": "job-missing",
		}))
		assertToolTextResponse(t, resp, "error: job not found: job-missing")
	})

	t.Run("missing workspace path", func(t *testing.T) {
		job := saveDiffTestJob(t, root, "job-diff-no-workspace", "", "", string(domain.WorkspaceModeShared))
		resp := server.handleToolCall(mustToolCallRequest(t, "gorchera_diff", map[string]any{
			"job_id": job.ID,
		}))
		assertToolTextResponse(t, resp, "error: workspace path is missing for job: "+job.ID)
	})

	t.Run("workspace path not found", func(t *testing.T) {
		missingWorkspace := filepath.Join(root, "missing-workspace")
		job := saveDiffTestJob(t, root, "job-diff-missing-workspace", missingWorkspace, missingWorkspace, string(domain.WorkspaceModeShared))
		resp := server.handleToolCall(mustToolCallRequest(t, "gorchera_diff", map[string]any{
			"job_id": job.ID,
		}))
		assertToolTextResponse(t, resp, "error: workspace path not found: "+missingWorkspace)
	})

	t.Run("not a git worktree", func(t *testing.T) {
		workspace := t.TempDir()
		job := saveDiffTestJob(t, root, "job-diff-not-git", workspace, workspace, string(domain.WorkspaceModeShared))
		resp := server.handleToolCall(mustToolCallRequest(t, "gorchera_diff", map[string]any{
			"job_id": job.ID,
		}))
		assertToolTextResponse(t, resp, "error: workspace is not a git worktree: "+workspace)
	})

	t.Run("no changes", func(t *testing.T) {
		workspace := newGitWorkspace(t)
		job := saveDiffTestJob(t, root, "job-diff-no-changes", workspace, workspace, string(domain.WorkspaceModeShared))
		resp := server.handleToolCall(mustToolCallRequest(t, "gorchera_diff", map[string]any{
			"job_id": job.ID,
		}))
		assertToolTextResponse(t, resp, "no changes")
	})
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

func TestToolStatusWaitReturnsTerminalStateAfterRelease(t *testing.T) {
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
	if !isTerminalJobStatus(current.Status) {
		t.Fatalf("expected terminal status, got %s", current.Status)
	}
}

func TestToolStatusWaitReturnsBlockedForOperatorCancellation(t *testing.T) {
	setStatusWaitTimings(t, 20*time.Millisecond, time.Second)

	control := newMCPWaitProvider("mcp-wait-cancel")
	server, service, _ := newTestServer(t, control)
	workspace := t.TempDir()

	job := startMCPJob(t, server, workspace, string(control.name))

	_, _ = service.Cancel(context.Background(), job.ID, "operator stop")

	waitForJobStatus(t, service, job.ID, domain.JobStatusBlocked)

	result, err := server.toolStatus(context.Background(), map[string]any{
		"job_id": job.ID,
		"wait":   false,
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

func TestToolChainStatusWaitReturnsTerminalStateAfterRelease(t *testing.T) {
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
	if !isTerminalChainStatus(chain.Status) {
		t.Fatalf("expected terminal chain status, got %s", chain.Status)
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

func TestJobTerminalNotificationBufferedUntilWriterInstalled(t *testing.T) {
	t.Parallel()

	server, _, _ := newTestServer(t, mock.New())
	server.sendJobTerminalNotification("job-1", domain.JobStatusBlocked, "buffered", nil)

	var out lockedBuffer
	server.installWriter(&out)

	notifications := parseNotificationLines(t, out.String())
	if len(notifications) != 1 {
		t.Fatalf("expected 1 buffered notification, got %d", len(notifications))
	}
	if got := notifications[0]["method"]; got != "notifications/job_terminal" {
		t.Fatalf("notification method = %#v, want notifications/job_terminal", got)
	}
	params, _ := notifications[0]["params"].(map[string]any)
	if got := params["job_id"]; got != "job-1" {
		t.Fatalf("job_id = %#v, want %q", got, "job-1")
	}
	if got := params["status"]; got != string(domain.JobStatusBlocked) {
		t.Fatalf("status = %#v, want %q", got, domain.JobStatusBlocked)
	}
}

func TestHandleEventNotificationEmitsTerminalNotificationForCancelledChainGoal(t *testing.T) {
	t.Parallel()

	server, _, root := newTestServer(t, mock.New())
	var out lockedBuffer
	server.installWriter(&out)

	job := &domain.Job{
		ID:            "job-terminal-cancelled",
		Goal:          "terminal notification",
		WorkspaceDir:  t.TempDir(),
		Status:        domain.JobStatusBlocked,
		Provider:      domain.ProviderMock,
		RoleProfiles:  domain.DefaultRoleProfiles(domain.ProviderMock),
		MaxSteps:      4,
		BlockedReason: "chain goal skipped by operator",
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}
	stateStore := store.NewStateStore(filepath.Join(root, "state"))
	if err := stateStore.SaveJob(context.Background(), job); err != nil {
		t.Fatalf("failed to save job: %v", err)
	}

	server.handleEventNotification(orchestrator.EventNotification{
		JobID:   job.ID,
		Kind:    "job_cancelled",
		Message: job.BlockedReason,
	})

	deadline := time.Now().Add(2 * time.Second)
	for {
		notifications := parseNotificationLines(t, out.String())
		for _, notification := range notifications {
			if notification["method"] != "notifications/job_terminal" {
				continue
			}
			params, _ := notification["params"].(map[string]any)
			if params["job_id"] != job.ID {
				continue
			}
			if params["summary"] != job.BlockedReason {
				t.Fatalf("summary = %#v, want %q", params["summary"], job.BlockedReason)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for terminal notification")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func parseNotificationLines(t *testing.T, raw string) []map[string]any {
	t.Helper()

	lines := strings.Split(strings.TrimSpace(raw), "\n")
	out := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("failed to decode notification line %q: %v", line, err)
		}
		out = append(out, msg)
	}
	return out
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
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
	t.Cleanup(service.Shutdown)

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

func (p *mcpWaitProvider) RunLeader(ctx context.Context, job domain.Job) (string, error) {
	if len(job.Steps) > 0 {
		return `{"action":"complete","target":"none","task_type":"none","reason":"wait provider complete"}`, nil
	}
	select {
	case <-p.release:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return `{"action":"run_worker","target":"executor","task_type":"implement","task_text":"perform wait-provider implementation"}`, nil
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

func assertToolTextResponse(t *testing.T, resp *jsonRPCResponse, want string) {
	t.Helper()

	if resp == nil {
		t.Fatal("expected response")
	}
	if resp.Error != nil {
		t.Fatalf("expected tool text result, got rpc error %#v", resp.Error)
	}
	result, ok := resp.Result.(toolResult)
	if !ok {
		t.Fatalf("expected toolResult, got %T", resp.Result)
	}
	if got := toolResultText(t, result); got != want {
		t.Fatalf("tool text = %q, want %q", got, want)
	}
}

func saveDiffTestJob(t *testing.T, root, jobID, workspaceDir, requestedWorkspaceDir, workspaceMode string) *domain.Job {
	t.Helper()

	job := &domain.Job{
		ID:                    jobID,
		Goal:                  "diff test",
		WorkspaceDir:          workspaceDir,
		RequestedWorkspaceDir: requestedWorkspaceDir,
		WorkspaceMode:         workspaceMode,
		Status:                domain.JobStatusDone,
		Provider:              domain.ProviderMock,
		RoleProfiles:          domain.DefaultRoleProfiles(domain.ProviderMock),
		MaxSteps:              1,
		CreatedAt:             time.Now().UTC(),
		UpdatedAt:             time.Now().UTC(),
	}
	stateStore := store.NewStateStore(filepath.Join(root, "state"))
	if err := stateStore.SaveJob(context.Background(), job); err != nil {
		t.Fatalf("failed to save job: %v", err)
	}
	return job
}

func newGitWorkspace(t *testing.T) string {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is required for diff tests: %v", err)
	}

	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("# test workspace\n"), 0o644); err != nil {
		t.Fatalf("failed to seed git workspace: %v", err)
	}

	gitRun(t, workspace, "init")
	gitRun(t, workspace, "add", "README.md")
	gitRun(t, workspace, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "init")
	return workspace
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(output))
	}
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

func waitForTerminalChainStatus(t *testing.T, service *orchestrator.Service, chainID string) {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)
	for {
		chain, err := service.GetChain(context.Background(), chainID)
		if err == nil && isTerminalChainStatus(chain.Status) {
			return
		}

		if time.Now().After(deadline) {
			if err != nil {
				t.Fatalf("timed out waiting for chain %s: %v", chainID, err)
			}
			t.Fatalf("timed out waiting for chain %s to reach a terminal status, got %s", chainID, chain.Status)
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
	if len(job.Steps) > 0 {
		return `{"action":"complete","target":"none","task_type":"none","reason":"chain goal complete"}`, nil
	}
	if strings.Contains(job.Goal, "hold") {
		select {
		case <-p.release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return `{"action":"run_worker","target":"executor","task_type":"implement","task_text":"perform chain implementation"}`, nil
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
