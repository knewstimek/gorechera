package provider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"gorechera/internal/domain"
	"gorechera/internal/provider/mock"
	"gorechera/internal/schema"
)

func TestNewRegistryRegistersBuiltInScaffolds(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	if _, err := registry.Get(domain.ProviderCodex); err != nil {
		t.Fatalf("expected codex adapter to be registered: %v", err)
	}
	if _, err := registry.Get(domain.ProviderClaude); err != nil {
		t.Fatalf("expected claude adapter to be registered: %v", err)
	}
}

func TestCodexAdapterDetectsMissingExecutable(t *testing.T) {
	t.Parallel()

	adapter := &CodexAdapter{
		executable: "definitely-not-present-gorechera-codex",
		probeArgs:  []string{"--version"},
		probeTime:  time.Second,
	}

	_, err := adapter.RunLeader(context.Background(), domain.Job{})
	if err == nil {
		t.Fatal("expected missing executable error")
	}

	var perr *ProviderError
	if !errors.As(err, &perr) {
		t.Fatalf("expected ProviderError, got %T: %v", err, err)
	}
	if perr.Kind != ErrorKindMissingExecutable {
		t.Fatalf("expected missing executable kind, got %s", perr.Kind)
	}
}

func TestClaudeAdapterReturnsStructuredResponseWhenExecutableExists(t *testing.T) {
	t.Parallel()

	adapter := &ClaudeAdapter{
		executable: "go",
		probeArgs:  []string{"version"},
		probeTime:  2 * time.Second,
		runTime:    2 * time.Second,
		runCommand: func(_ context.Context, _ string, _ time.Duration, _ string, _ []string, _ string, _ ...string) (CommandResult, error) {
			return CommandResult{Stdout: `{"action":"complete","target":"none","task_type":"none","task_text":"","reason":"ok"}`}, nil
		},
	}

	out, err := adapter.RunLeader(context.Background(), domain.Job{Goal: "test"})
	if err != nil {
		t.Fatalf("expected structured response, got error: %v", err)
	}
	if out == "" {
		t.Fatal("expected non-empty response")
	}
}

func TestCodexAdapterReturnsStructuredResponseWhenExecutableExists(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	adapter := &CodexAdapter{
		executable: "go",
		probeArgs:  []string{"version"},
		probeTime:  2 * time.Second,
		runTime:    2 * time.Second,
		// runCommand now matches runExecutableWithStdin: stdinData before variadic args
		runCommand: func(_ context.Context, _ string, _ time.Duration, _ string, _ []string, _ string, args ...string) (CommandResult, error) {
			outputPath := ""
			for i := 0; i < len(args)-1; i++ {
				if args[i] == "-o" {
					outputPath = args[i+1]
					break
				}
			}
			if outputPath == "" {
				return CommandResult{}, errors.New("missing output path")
			}
			if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
				return CommandResult{}, err
			}
			if err := os.WriteFile(outputPath, []byte(`{"status":"success","summary":"ok"}`), 0o644); err != nil {
				return CommandResult{}, err
			}
			return CommandResult{}, nil
		},
	}

	out, err := adapter.RunWorker(context.Background(), domain.Job{Goal: "test", WorkspaceDir: root}, domain.LeaderOutput{
		Action:   "run_worker",
		Target:   "B",
		TaskType: "implement",
		TaskText: "do work",
	})
	if err != nil {
		t.Fatalf("expected structured response, got error: %v", err)
	}
	if out == "" {
		t.Fatal("expected non-empty response")
	}
}

func TestSessionManagerRunsPlannerAndEvaluatorPhases(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	registry.Register(mock.New())

	manager := NewSessionManager(registry)
	job := domain.Job{
		Goal:         "Create a planner/evaluator phase",
		Provider:     domain.ProviderMock,
		RoleProfiles: domain.DefaultRoleProfiles(domain.ProviderMock),
	}

	plannerRaw, err := manager.RunPlanner(context.Background(), job)
	if err != nil {
		t.Fatalf("expected planner phase to run, got error: %v", err)
	}
	var plan domain.PlanningArtifact
	if err := json.Unmarshal([]byte(plannerRaw), &plan); err != nil {
		t.Fatalf("failed to decode planner output: %v", err)
	}
	if plan.Goal != job.Goal {
		t.Fatalf("expected planner output goal %q, got %q", job.Goal, plan.Goal)
	}

	evaluatorRaw, err := manager.RunEvaluator(context.Background(), job)
	if err != nil {
		t.Fatalf("expected evaluator phase to run, got error: %v", err)
	}
	var report domain.EvaluatorReport
	if err := json.Unmarshal([]byte(evaluatorRaw), &report); err != nil {
		t.Fatalf("failed to decode evaluator output: %v", err)
	}
	if report.Status != "blocked" {
		t.Fatalf("expected blocked evaluator report for empty job, got %s", report.Status)
	}
}

func TestCodexAdapterRunPlannerUsesPlannerProfile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	adapter := &CodexAdapter{
		executable: "go",
		probeArgs:  []string{"version"},
		probeTime:  2 * time.Second,
		runTime:    2 * time.Second,
		// runCommand now matches runExecutableWithStdin: prompt is stdinData (6th arg)
		runCommand: func(_ context.Context, _ string, _ time.Duration, _ string, _ []string, stdinData string, args ...string) (CommandResult, error) {
			// prompt is now fed via stdin, not as a positional arg
			if !strings.Contains(stdinData, "planning component") && !strings.Contains(stdinData, "planner") {
				t.Fatalf("expected planner prompt in stdin, got: %s", stdinData)
			}
			if !strings.Contains(stdinData, "planner-model") {
				t.Fatalf("expected planner profile to appear in stdin prompt, got: %s", stdinData)
			}
			outputPath := ""
			for i := 0; i < len(args)-1; i++ {
				if args[i] == "-o" {
					outputPath = args[i+1]
					break
				}
			}
			if outputPath == "" {
				return CommandResult{}, errors.New("missing output path")
			}
			if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
				return CommandResult{}, err
			}
			if err := os.WriteFile(outputPath, []byte(`{"goal":"Create a planner/evaluator phase","summary":"planner ok"}`), 0o644); err != nil {
				return CommandResult{}, err
			}
			return CommandResult{}, nil
		},
	}

	out, err := adapter.RunPlanner(context.Background(), domain.Job{
		Goal:         "Create a planner/evaluator phase",
		WorkspaceDir: root,
		Provider:     domain.ProviderCodex,
		RoleProfiles: domain.RoleProfiles{
			Planner: domain.ExecutionProfile{Provider: domain.ProviderCodex, Model: "planner-model"},
		},
	})
	if err != nil {
		t.Fatalf("expected planner phase to succeed, got error: %v", err)
	}
	if out == "" {
		t.Fatal("expected planner output")
	}
}

func TestClaudeAdapterRunEvaluatorUsesEvaluatorProfile(t *testing.T) {
	t.Parallel()

	adapter := &ClaudeAdapter{
		executable: "go",
		probeArgs:  []string{"version"},
		probeTime:  2 * time.Second,
		runTime:    2 * time.Second,
		runCommand: func(_ context.Context, _ string, _ time.Duration, _ string, _ []string, stdinData string, _ ...string) (CommandResult, error) {
			// prompt now says "evaluation component" instead of "evaluator agent"
			if !strings.Contains(stdinData, "evaluation component") && !strings.Contains(stdinData, "evaluator") {
				t.Fatalf("expected evaluator prompt, got: %s", stdinData)
			}
			if !strings.Contains(stdinData, "evaluator-model") {
				t.Fatalf("expected evaluator profile to appear in prompt, got: %s", stdinData)
			}
			return CommandResult{Stdout: `{"status":"passed","passed":true,"score":100,"reason":"ok"}`}, nil
		},
	}

	out, err := adapter.RunEvaluator(context.Background(), domain.Job{
		Goal:     "Validate evaluator phase",
		Provider: domain.ProviderClaude,
		RoleProfiles: domain.RoleProfiles{
			Evaluator: domain.ExecutionProfile{Provider: domain.ProviderClaude, Model: "evaluator-model"},
		},
	})
	if err != nil {
		t.Fatalf("expected evaluator phase to succeed, got error: %v", err)
	}
	if out == "" {
		t.Fatal("expected evaluator output")
	}
}

func TestPlannerAndEvaluatorPromptsIncludeVerificationContract(t *testing.T) {
	t.Parallel()

	job := domain.Job{
		Goal:         "Verify contract shaping",
		Provider:     domain.ProviderCodex,
		WorkspaceDir: t.TempDir(),
		VerificationContract: &domain.VerificationContract{
			Version:           1,
			Goal:              "Verify contract shaping",
			Scope:             []string{"implementation"},
			RequiredCommands:  []string{"go test ./..."},
			RequiredChecks:    []string{"done gate"},
			RequiredArtifacts: []string{"verification_evidence.json"},
		},
	}

	plannerPrompt := buildPlannerPrompt(job)
	if !strings.Contains(plannerPrompt, "verification_contract") {
		t.Fatal("expected planner prompt to include verification contract schema guidance")
	}
	evaluatorPrompt := buildEvaluatorPrompt(job)
	if !strings.Contains(evaluatorPrompt, "Verification contract") {
		t.Fatal("expected evaluator prompt to include verification contract payload")
	}
	workerPrompt := buildWorkerPrompt(job, domain.LeaderOutput{
		Action:   "run_worker",
		Target:   "D",
		TaskType: "test",
		TaskText: "run the verification contract",
	})
	if !strings.Contains(workerPrompt, "verification contract") {
		t.Fatal("expected worker prompt to include verification contract payload")
	}
}

func TestLeaderPromptIncludesRunWorkersGuidance(t *testing.T) {
	t.Parallel()

	prompt := buildLeaderPrompt(domain.Job{
		Goal:     "Fan out parallel work",
		Provider: domain.ProviderMock,
	})
	if !strings.Contains(prompt, "run_workers") {
		t.Fatal("expected leader prompt to mention run_workers")
	}
	// Updated prompt uses "exactly 2 workers" phrasing instead of "at most 2 workers"
	if !strings.Contains(prompt, "2 worker") {
		t.Fatal("expected leader prompt to mention parallel worker limit")
	}
}

func TestLeaderPromptIncludesSupervisorDirectiveBeforeJobState(t *testing.T) {
	t.Parallel()

	prompt := buildLeaderPrompt(domain.Job{
		Goal:                "Honor the supervisor directive",
		Provider:            domain.ProviderMock,
		SupervisorDirective: "[SUPERVISOR] prioritize the audit fix",
	})

	supervisorIdx := strings.Index(prompt, "Supervisor directive:\n[SUPERVISOR] prioritize the audit fix")
	if supervisorIdx < 0 {
		t.Fatal("expected separate supervisor directive section")
	}
	jobStateIdx := strings.Index(prompt, "Current job state:")
	if jobStateIdx < 0 {
		t.Fatal("expected current job state section")
	}
	if supervisorIdx > jobStateIdx {
		t.Fatal("expected supervisor directive before job state")
	}
	if strings.Count(prompt, "[SUPERVISOR] prioritize the audit fix") != 1 {
		t.Fatal("expected supervisor directive to appear only once in the prompt")
	}
}

func TestMockPlannerProducesVerificationContract(t *testing.T) {
	t.Parallel()

	adapter := mock.New()
	out, err := adapter.RunPlanner(context.Background(), domain.Job{Goal: "contract"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var plan domain.PlanningArtifact
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatalf("failed to decode planner output: %v", err)
	}
	if plan.VerificationContract == nil {
		t.Fatal("expected verification contract in planner output")
	}
	if err := schema.ValidateVerificationContract(*plan.VerificationContract); err != nil {
		t.Fatalf("expected verification contract to validate: %v", err)
	}
}

func TestMockLeaderProducesRunWorkersForParallelGoal(t *testing.T) {
	t.Parallel()

	adapter := mock.New()
	out, err := adapter.RunLeader(context.Background(), domain.Job{
		Goal:     "Create a parallel orchestrator fan-out",
		Provider: domain.ProviderMock,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var leader domain.LeaderOutput
	if err := json.Unmarshal([]byte(out), &leader); err != nil {
		t.Fatalf("failed to decode leader output: %v", err)
	}
	if leader.Action != "run_workers" {
		t.Fatalf("expected run_workers action, got %s", leader.Action)
	}
	if len(leader.Tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(leader.Tasks))
	}
	if err := schema.ValidateLeaderOutput(leader); err != nil {
		t.Fatalf("expected run_workers leader output to validate: %v", err)
	}
}

func TestBuildMinimalPayloadCountsBlockedFailedAndActiveStepsSeparately(t *testing.T) {
	t.Parallel()

	payload := buildMinimalPayload(domain.Job{
		Goal: "Count step states",
		Steps: []domain.Step{
			{Index: 1, TaskType: "implement", Status: ""},
			{Index: 2, TaskType: "review", Status: domain.StepStatusActive},
			{Index: 3, TaskType: "review", Status: domain.StepStatusBlocked},
			{Index: 4, TaskType: "test", Status: domain.StepStatusFailed},
			{Index: 5, TaskType: "test", Status: domain.StepStatusPending},
			{Index: 6, TaskType: "test", Status: domain.StepStatusSucceeded},
		},
	})

	var out struct {
		SucceededSteps int `json:"succeeded_steps"`
		FailedSteps    int `json:"failed_steps"`
		BlockedSteps   int `json:"blocked_steps"`
		ActiveSteps    int `json:"active_steps"`
	}
	if err := json.Unmarshal([]byte(payload), &out); err != nil {
		t.Fatalf("failed to decode minimal payload: %v", err)
	}

	if out.SucceededSteps != 1 {
		t.Fatalf("expected 1 succeeded step, got %d", out.SucceededSteps)
	}
	if out.FailedSteps != 1 {
		t.Fatalf("expected 1 failed step, got %d", out.FailedSteps)
	}
	if out.BlockedSteps != 1 {
		t.Fatalf("expected 1 blocked step, got %d", out.BlockedSteps)
	}
	if out.ActiveSteps != 2 {
		t.Fatalf("expected only empty-status and active steps to count as active, got %d", out.ActiveSteps)
	}
}

func TestBuildSummaryPayloadTruncatesSummariesOnRuneBoundaries(t *testing.T) {
	t.Parallel()

	longSummary := strings.Repeat("가", 81)
	payload := buildSummaryPayload(domain.Job{
		Goal: "Truncate multibyte summaries safely",
		Steps: []domain.Step{
			{Index: 1, TaskType: "implement", Status: domain.StepStatusSucceeded, Summary: longSummary},
			{Index: 2, TaskType: "review", Status: domain.StepStatusSucceeded, Summary: "reviewed"},
			{Index: 3, TaskType: "test", Status: domain.StepStatusSucceeded, Summary: "tested"},
		},
	})

	var out struct {
		Steps []struct {
			Summary string `json:"summary"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(payload), &out); err != nil {
		t.Fatalf("failed to decode summary payload: %v", err)
	}
	if len(out.Steps) != 3 {
		t.Fatalf("expected 3 steps, got %d", len(out.Steps))
	}

	want := strings.Repeat("가", 80) + "..."
	if out.Steps[0].Summary != want {
		t.Fatalf("expected rune-safe truncation %q, got %q", want, out.Steps[0].Summary)
	}
	if !utf8.ValidString(out.Steps[0].Summary) {
		t.Fatalf("expected valid utf-8 summary, got %q", out.Steps[0].Summary)
	}
}

func TestSessionManagerReportsUnsupportedPlannerPhase(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	registry.Register(leaderOnlyAdapter{name: domain.ProviderName("leader-only")})

	manager := NewSessionManager(registry)
	_, err := manager.RunPlanner(context.Background(), domain.Job{
		Provider: domain.ProviderName("leader-only"),
	})
	if err == nil {
		t.Fatal("expected unsupported planner phase error")
	}

	var perr *ProviderError
	if !errors.As(err, &perr) {
		t.Fatalf("expected ProviderError, got %T: %v", err, err)
	}
	if perr.Kind != ErrorKindUnsupportedPhase {
		t.Fatalf("expected unsupported phase kind, got %s", perr.Kind)
	}
}

func TestSessionManagerFallsBackToSecondaryProvider(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	registry.Register(fallbackLeaderAdapter{name: domain.ProviderName("role-fallback")})

	manager := NewSessionManager(registry)
	out, err := manager.RunLeader(context.Background(), domain.Job{
		Provider: domain.ProviderName("unused-primary"),
		RoleProfiles: domain.RoleProfiles{
			Leader: domain.ExecutionProfile{
				Provider:         domain.ProviderName("missing-primary"),
				FallbackProvider: domain.ProviderName("role-fallback"),
			},
		},
	})
	if err != nil {
		t.Fatalf("expected fallback provider to succeed, got error: %v", err)
	}
	if out != `{"action":"complete","target":"none","task_type":"none","reason":"fallback used"}` {
		t.Fatalf("unexpected fallback output: %s", out)
	}
}

type leaderOnlyAdapter struct {
	name domain.ProviderName
}

func (a leaderOnlyAdapter) Name() domain.ProviderName {
	return a.name
}

func (a leaderOnlyAdapter) RunLeader(_ context.Context, _ domain.Job) (string, error) {
	return `{"action":"complete","target":"none","task_type":"none","reason":"leader only"}`, nil
}

func (a leaderOnlyAdapter) RunWorker(_ context.Context, _ domain.Job, _ domain.LeaderOutput) (string, error) {
	return `{"status":"success","summary":"leader only worker"}`, nil
}

type fallbackLeaderAdapter struct {
	name domain.ProviderName
}

func (a fallbackLeaderAdapter) Name() domain.ProviderName {
	return a.name
}

func (a fallbackLeaderAdapter) RunLeader(_ context.Context, _ domain.Job) (string, error) {
	return `{"action":"complete","target":"none","task_type":"none","reason":"fallback used"}`, nil
}

func (a fallbackLeaderAdapter) RunWorker(_ context.Context, _ domain.Job, _ domain.LeaderOutput) (string, error) {
	return `{"status":"success","summary":"fallback worker"}`, nil
}
