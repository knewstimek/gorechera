package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"gorchera/internal/domain"
	"gorchera/internal/provider/mock"
	"gorchera/internal/schema"
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
		executable: "definitely-not-present-gorchera-codex",
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

func TestClaudeAdapterExtractsStructuredPayloadFromJSONEnvelopes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		output string
		want   string
	}{
		{
			name:   "structured_output",
			output: `{"structured_output":{"status":"success","summary":"ok"}}`,
			want:   `{"status":"success","summary":"ok"}`,
		},
		{
			name:   "parsed_output",
			output: `{"parsed_output":{"status":"success","summary":"ok"}}`,
			want:   `{"status":"success","summary":"ok"}`,
		},
		{
			name:   "object result",
			output: `{"result":{"status":"success","summary":"ok"}}`,
			want:   `{"status":"success","summary":"ok"}`,
		},
		{
			name:   "string result",
			output: `{"result":"{\"status\":\"success\",\"summary\":\"ok\"}"}`,
			want:   `{"status":"success","summary":"ok"}`,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := extractJSONResult(tc.output); got != tc.want {
				t.Fatalf("extractJSONResult(%q) = %q, want %q", tc.output, got, tc.want)
			}
		})
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

func TestSessionManagerUsesRoleOverrideProvidersAcrossRoles(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	for _, name := range []domain.ProviderName{
		domain.ProviderName("global-provider"),
		domain.ProviderName("role-profile-provider"),
		domain.ProviderName("leader-provider"),
		domain.ProviderName("planner-provider"),
		domain.ProviderName("evaluator-provider"),
		domain.ProviderName("executor-provider"),
		domain.ProviderName("reviewer-provider"),
		domain.ProviderName("tester-provider"),
	} {
		registry.Register(roleTrackingAdapter{name: name})
	}

	manager := NewSessionManager(registry)

	cases := []struct {
		name string
		job  domain.Job
		run  func(domain.Job) (string, error)
		want string
	}{
		{
			name: "leader",
			job: domain.Job{
				Provider: domain.ProviderName("global-provider"),
				RoleProfiles: domain.RoleProfiles{
					Leader: domain.ExecutionProfile{Provider: domain.ProviderName("role-profile-provider"), Model: "leader-profile-model"},
				},
				RoleOverrides: map[string]domain.RoleProfile{
					"leader": {Provider: domain.ProviderName("leader-provider"), Model: "leader-override-model"},
				},
			},
			run: func(job domain.Job) (string, error) {
				return manager.RunLeader(context.Background(), job)
			},
			want: "leader-provider:leader",
		},
		{
			name: "planner",
			job: domain.Job{
				Provider: domain.ProviderName("global-provider"),
				RoleProfiles: domain.RoleProfiles{
					Planner: domain.ExecutionProfile{Provider: domain.ProviderName("role-profile-provider"), Model: "planner-profile-model"},
				},
				RoleOverrides: map[string]domain.RoleProfile{
					"planner": {Provider: domain.ProviderName("planner-provider"), Model: "planner-override-model"},
				},
			},
			run: func(job domain.Job) (string, error) {
				return manager.RunPlanner(context.Background(), job)
			},
			want: "planner-provider:planner",
		},
		{
			name: "evaluator",
			job: domain.Job{
				Provider: domain.ProviderName("global-provider"),
				RoleProfiles: domain.RoleProfiles{
					Evaluator: domain.ExecutionProfile{Provider: domain.ProviderName("role-profile-provider"), Model: "evaluator-profile-model"},
				},
				RoleOverrides: map[string]domain.RoleProfile{
					"evaluator": {Provider: domain.ProviderName("evaluator-provider"), Model: "evaluator-override-model"},
				},
			},
			run: func(job domain.Job) (string, error) {
				return manager.RunEvaluator(context.Background(), job)
			},
			want: "evaluator-provider:evaluator",
		},
		{
			name: "worker executor",
			job: domain.Job{
				Provider: domain.ProviderName("global-provider"),
				RoleProfiles: domain.RoleProfiles{
					Executor: domain.ExecutionProfile{Provider: domain.ProviderName("role-profile-provider"), Model: "executor-profile-model"},
				},
				RoleOverrides: map[string]domain.RoleProfile{
					"executor": {Provider: domain.ProviderName("executor-provider"), Model: "executor-override-model"},
				},
			},
			run: func(job domain.Job) (string, error) {
				return manager.RunWorker(context.Background(), job, domain.LeaderOutput{TaskType: "implement"})
			},
			want: "executor-provider:worker:implement",
		},
		{
			name: "worker reviewer",
			job: domain.Job{
				Provider: domain.ProviderName("global-provider"),
				RoleProfiles: domain.RoleProfiles{
					Reviewer: domain.ExecutionProfile{Provider: domain.ProviderName("role-profile-provider"), Model: "reviewer-profile-model"},
				},
				RoleOverrides: map[string]domain.RoleProfile{
					"reviewer": {Provider: domain.ProviderName("reviewer-provider"), Model: "reviewer-override-model"},
				},
			},
			run: func(job domain.Job) (string, error) {
				return manager.RunWorker(context.Background(), job, domain.LeaderOutput{TaskType: "review"})
			},
			want: "reviewer-provider:worker:review",
		},
		{
			name: "worker tester",
			job: domain.Job{
				Provider: domain.ProviderName("global-provider"),
				RoleProfiles: domain.RoleProfiles{
					Tester: domain.ExecutionProfile{Provider: domain.ProviderName("role-profile-provider"), Model: "tester-profile-model"},
				},
				RoleOverrides: map[string]domain.RoleProfile{
					"tester": {Provider: domain.ProviderName("tester-provider"), Model: "tester-override-model"},
				},
			},
			run: func(job domain.Job) (string, error) {
				return manager.RunWorker(context.Background(), job, domain.LeaderOutput{TaskType: "test"})
			},
			want: "tester-provider:worker:test",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := tc.run(tc.job)
			if err != nil {
				t.Fatalf("expected override provider to succeed, got error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}

func TestSessionManagerRoleOverridesFallBackCleanly(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	for _, name := range []domain.ProviderName{
		domain.ProviderName("global-provider"),
		domain.ProviderName("role-profile-provider"),
		domain.ProviderName("leader-override-provider"),
		domain.ProviderName("executor-override-provider"),
		domain.ProviderName("tester-override-provider"),
	} {
		registry.Register(profileEchoAdapter{name: name})
	}
	manager := NewSessionManager(registry)

	cases := []struct {
		name string
		job  domain.Job
		run  func(domain.Job) (string, error)
		want string
	}{
		{
			name: "leader override provider keeps role profile model",
			job: domain.Job{
				Provider: domain.ProviderName("global-provider"),
				RoleProfiles: domain.RoleProfiles{
					Leader: domain.ExecutionProfile{Provider: domain.ProviderName("role-profile-provider"), Model: "leader-profile-model"},
				},
				RoleOverrides: map[string]domain.RoleProfile{
					"leader": {Provider: domain.ProviderName("leader-override-provider")},
				},
			},
			run: func(job domain.Job) (string, error) {
				return manager.RunLeader(context.Background(), job)
			},
			want: "leader-override-provider:leader:leader-profile-model",
		},
		{
			name: "planner override model keeps role profile provider",
			job: domain.Job{
				Provider: domain.ProviderName("global-provider"),
				RoleProfiles: domain.RoleProfiles{
					Planner: domain.ExecutionProfile{Provider: domain.ProviderName("role-profile-provider"), Model: "planner-profile-model"},
				},
				RoleOverrides: map[string]domain.RoleProfile{
					"planner": {Model: "planner-override-model"},
				},
			},
			run: func(job domain.Job) (string, error) {
				return manager.RunPlanner(context.Background(), job)
			},
			want: "role-profile-provider:planner:planner-override-model",
		},
		{
			name: "evaluator override model keeps job provider when role provider empty",
			job: domain.Job{
				Provider: domain.ProviderName("global-provider"),
				RoleProfiles: domain.RoleProfiles{
					Evaluator: domain.ExecutionProfile{Model: "evaluator-profile-model"},
				},
				RoleOverrides: map[string]domain.RoleProfile{
					"evaluator": {Model: "evaluator-override-model"},
				},
			},
			run: func(job domain.Job) (string, error) {
				return manager.RunEvaluator(context.Background(), job)
			},
			want: "global-provider:evaluator:evaluator-override-model",
		},
		{
			name: "executor override provider keeps role profile model",
			job: domain.Job{
				Provider: domain.ProviderName("global-provider"),
				RoleProfiles: domain.RoleProfiles{
					Executor: domain.ExecutionProfile{Provider: domain.ProviderName("role-profile-provider"), Model: "executor-profile-model"},
				},
				RoleOverrides: map[string]domain.RoleProfile{
					"executor": {Provider: domain.ProviderName("executor-override-provider")},
				},
			},
			run: func(job domain.Job) (string, error) {
				return manager.RunWorker(context.Background(), job, domain.LeaderOutput{TaskType: "implement"})
			},
			want: "executor-override-provider:executor:executor-profile-model",
		},
		{
			name: "reviewer override model keeps role profile provider",
			job: domain.Job{
				Provider: domain.ProviderName("global-provider"),
				RoleProfiles: domain.RoleProfiles{
					Reviewer: domain.ExecutionProfile{Provider: domain.ProviderName("role-profile-provider"), Model: "reviewer-profile-model"},
				},
				RoleOverrides: map[string]domain.RoleProfile{
					"reviewer": {Model: "reviewer-override-model"},
				},
			},
			run: func(job domain.Job) (string, error) {
				return manager.RunWorker(context.Background(), job, domain.LeaderOutput{TaskType: "review"})
			},
			want: "role-profile-provider:reviewer:reviewer-override-model",
		},
		{
			name: "tester override provider keeps role profile model",
			job: domain.Job{
				Provider: domain.ProviderName("global-provider"),
				RoleProfiles: domain.RoleProfiles{
					Tester: domain.ExecutionProfile{Provider: domain.ProviderName("role-profile-provider"), Model: "tester-profile-model"},
				},
				RoleOverrides: map[string]domain.RoleProfile{
					"tester": {Provider: domain.ProviderName("tester-override-provider")},
				},
			},
			run: func(job domain.Job) (string, error) {
				return manager.RunWorker(context.Background(), job, domain.LeaderOutput{TaskType: "test"})
			},
			want: "tester-override-provider:tester:tester-profile-model",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := tc.run(tc.job)
			if err != nil {
				t.Fatalf("expected role override fallback to succeed, got error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}

func TestSessionManagerFallsBackToJobProviderWhenRoleProviderEmpty(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	registry.Register(roleTrackingAdapter{name: domain.ProviderName("global-provider")})

	manager := NewSessionManager(registry)

	cases := []struct {
		name string
		job  domain.Job
		run  func(domain.Job) (string, error)
		want string
	}{
		{
			name: "leader",
			job: domain.Job{
				Provider: domain.ProviderName("global-provider"),
				RoleProfiles: domain.RoleProfiles{
					Leader: domain.ExecutionProfile{Model: "leader-model"},
				},
			},
			run: func(job domain.Job) (string, error) {
				return manager.RunLeader(context.Background(), job)
			},
			want: "global-provider:leader",
		},
		{
			name: "planner",
			job: domain.Job{
				Provider: domain.ProviderName("global-provider"),
				RoleProfiles: domain.RoleProfiles{
					Planner: domain.ExecutionProfile{Model: "planner-model"},
				},
			},
			run: func(job domain.Job) (string, error) {
				return manager.RunPlanner(context.Background(), job)
			},
			want: "global-provider:planner",
		},
		{
			name: "evaluator",
			job: domain.Job{
				Provider: domain.ProviderName("global-provider"),
				RoleProfiles: domain.RoleProfiles{
					Evaluator: domain.ExecutionProfile{Model: "evaluator-model"},
				},
			},
			run: func(job domain.Job) (string, error) {
				return manager.RunEvaluator(context.Background(), job)
			},
			want: "global-provider:evaluator",
		},
		{
			name: "worker executor",
			job: domain.Job{
				Provider: domain.ProviderName("global-provider"),
				RoleProfiles: domain.RoleProfiles{
					Executor: domain.ExecutionProfile{Model: "executor-model"},
				},
			},
			run: func(job domain.Job) (string, error) {
				return manager.RunWorker(context.Background(), job, domain.LeaderOutput{TaskType: "implement"})
			},
			want: "global-provider:worker:implement",
		},
		{
			name: "worker reviewer",
			job: domain.Job{
				Provider: domain.ProviderName("global-provider"),
				RoleProfiles: domain.RoleProfiles{
					Reviewer: domain.ExecutionProfile{Model: "reviewer-model"},
				},
			},
			run: func(job domain.Job) (string, error) {
				return manager.RunWorker(context.Background(), job, domain.LeaderOutput{TaskType: "review"})
			},
			want: "global-provider:worker:review",
		},
		{
			name: "worker tester",
			job: domain.Job{
				Provider: domain.ProviderName("global-provider"),
				RoleProfiles: domain.RoleProfiles{
					Tester: domain.ExecutionProfile{Model: "tester-model"},
				},
			},
			run: func(job domain.Job) (string, error) {
				return manager.RunWorker(context.Background(), job, domain.LeaderOutput{TaskType: "test"})
			},
			want: "global-provider:worker:test",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := tc.run(tc.job)
			if err != nil {
				t.Fatalf("expected global provider fallback to succeed, got error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
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

func TestCodexAdapterAddsModelFlagForGPTRoleProfiles(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		job    func(string) domain.Job
		invoke func(*CodexAdapter, domain.Job) (string, error)
	}{
		{
			name: "leader",
			job: func(root string) domain.Job {
				return domain.Job{
					Goal:         "Leader uses codex model",
					WorkspaceDir: root,
					Provider:     domain.ProviderCodex,
					RoleProfiles: domain.RoleProfiles{
						Leader: domain.ExecutionProfile{Provider: domain.ProviderCodex, Model: "gpt-5.4"},
					},
				}
			},
			invoke: func(adapter *CodexAdapter, job domain.Job) (string, error) {
				return adapter.RunLeader(context.Background(), job)
			},
		},
		{
			name: "planner",
			job: func(root string) domain.Job {
				return domain.Job{
					Goal:         "Planner uses codex model",
					WorkspaceDir: root,
					Provider:     domain.ProviderCodex,
					RoleProfiles: domain.RoleProfiles{
						Planner: domain.ExecutionProfile{Provider: domain.ProviderCodex, Model: "gpt-5.4"},
					},
				}
			},
			invoke: func(adapter *CodexAdapter, job domain.Job) (string, error) {
				return adapter.RunPlanner(context.Background(), job)
			},
		},
		{
			name: "evaluator",
			job: func(root string) domain.Job {
				return domain.Job{
					Goal:         "Evaluator uses codex model",
					WorkspaceDir: root,
					Provider:     domain.ProviderCodex,
					RoleProfiles: domain.RoleProfiles{
						Evaluator: domain.ExecutionProfile{Provider: domain.ProviderCodex, Model: "gpt-5.4"},
					},
				}
			},
			invoke: func(adapter *CodexAdapter, job domain.Job) (string, error) {
				return adapter.RunEvaluator(context.Background(), job)
			},
		},
		{
			name: "worker",
			job: func(root string) domain.Job {
				return domain.Job{
					Goal:         "Worker uses codex model",
					WorkspaceDir: root,
					Provider:     domain.ProviderCodex,
					RoleProfiles: domain.RoleProfiles{
						Executor: domain.ExecutionProfile{Provider: domain.ProviderCodex, Model: "gpt-5.4"},
					},
				}
			},
			invoke: func(adapter *CodexAdapter, job domain.Job) (string, error) {
				return adapter.RunWorker(context.Background(), job, domain.LeaderOutput{
					Action:   "run_worker",
					Target:   "B",
					TaskType: "implement",
					TaskText: "execute work",
				})
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			var capturedArgs []string
			adapter := newTestCodexAdapter(t, func(_ string, args []string) {
				capturedArgs = append([]string(nil), args...)
			})

			out, err := tc.invoke(adapter, tc.job(root))
			if err != nil {
				t.Fatalf("expected %s run to succeed, got error: %v", tc.name, err)
			}
			if out == "" {
				t.Fatalf("expected %s output", tc.name)
			}
			assertCodexModelFlag(t, capturedArgs, "gpt-5.4")
		})
	}
}

func TestClaudeAdapterBuildsCLIArgsAndModelFlagForRoleProfiles(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		job        func() domain.Job
		invoke     func(*ClaudeAdapter, domain.Job) (string, error)
		wantModel  string
		wantPrompt string
	}{
		{
			name: "leader",
			job: func() domain.Job {
				return domain.Job{
					Goal:     "Leader uses claude model",
					Provider: domain.ProviderClaude,
					RoleProfiles: domain.RoleProfiles{
						Leader: domain.ExecutionProfile{Provider: domain.ProviderClaude, Model: "sonnet"},
					},
				}
			},
			invoke: func(adapter *ClaudeAdapter, job domain.Job) (string, error) {
				return adapter.RunLeader(context.Background(), job)
			},
			wantModel:  "sonnet",
			wantPrompt: "leader component",
		},
		{
			name: "planner",
			job: func() domain.Job {
				return domain.Job{
					Goal:     "Planner uses claude model",
					Provider: domain.ProviderClaude,
					RoleProfiles: domain.RoleProfiles{
						Planner: domain.ExecutionProfile{Provider: domain.ProviderClaude, Model: "haiku"},
					},
				}
			},
			invoke: func(adapter *ClaudeAdapter, job domain.Job) (string, error) {
				return adapter.RunPlanner(context.Background(), job)
			},
			wantModel:  "haiku",
			wantPrompt: "planner component",
		},
		{
			name: "evaluator",
			job: func() domain.Job {
				return domain.Job{
					Goal:     "Evaluator uses claude model",
					Provider: domain.ProviderClaude,
					RoleProfiles: domain.RoleProfiles{
						Evaluator: domain.ExecutionProfile{Provider: domain.ProviderClaude, Model: "sonnet"},
					},
				}
			},
			invoke: func(adapter *ClaudeAdapter, job domain.Job) (string, error) {
				return adapter.RunEvaluator(context.Background(), job)
			},
			wantModel:  "sonnet",
			wantPrompt: "evaluator component",
		},
		{
			name: "worker",
			job: func() domain.Job {
				return domain.Job{
					Goal:     "Worker uses claude model",
					Provider: domain.ProviderClaude,
					RoleProfiles: domain.RoleProfiles{
						Executor: domain.ExecutionProfile{Provider: domain.ProviderClaude, Model: "haiku"},
					},
				}
			},
			invoke: func(adapter *ClaudeAdapter, job domain.Job) (string, error) {
				return adapter.RunWorker(context.Background(), job, domain.LeaderOutput{
					Action:   "run_worker",
					Target:   "B",
					TaskType: "implement",
					TaskText: "execute work",
				})
			},
			wantModel:  "haiku",
			wantPrompt: "executor worker",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var capturedArgs []string
			var capturedPrompt string
			adapter := newTestClaudeAdapter(t, func(stdinData string, args []string) {
				capturedPrompt = stdinData
				capturedArgs = append([]string(nil), args...)
			})

			out, err := tc.invoke(adapter, tc.job())
			if err != nil {
				t.Fatalf("expected %s run to succeed, got error: %v", tc.name, err)
			}
			if out == "" {
				t.Fatalf("expected %s output", tc.name)
			}
			if !strings.Contains(capturedPrompt, tc.wantPrompt) {
				t.Fatalf("expected %s prompt to contain %q, got %q", tc.name, tc.wantPrompt, capturedPrompt)
			}
			assertClaudeBaseArgs(t, capturedArgs)
			assertClaudeModelFlag(t, capturedArgs, tc.wantModel)
			assertClaudeJSONSchemaMinified(t, capturedArgs)
		})
	}
}

func TestCodexAdapterSuppressesModelFlagForClaudeShorthandAndEmptyModels(t *testing.T) {
	t.Parallel()

	for _, model := range []string{"", "opus", "sonnet", "haiku"} {
		model := model
		t.Run(modelOrEmpty(model), func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			var capturedArgs []string
			adapter := newTestCodexAdapter(t, func(_ string, args []string) {
				capturedArgs = append([]string(nil), args...)
			})

			out, err := adapter.RunLeader(context.Background(), domain.Job{
				Goal:         "Suppress codex model flag",
				WorkspaceDir: root,
				Provider:     domain.ProviderCodex,
				RoleProfiles: domain.RoleProfiles{
					Leader: domain.ExecutionProfile{Provider: domain.ProviderCodex, Model: model},
				},
			})
			if err != nil {
				t.Fatalf("expected leader run to succeed, got error: %v", err)
			}
			if out == "" {
				t.Fatal("expected leader output")
			}
			assertCodexModelFlagAbsent(t, capturedArgs)
		})
	}
}

func TestIsCodexModel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		model string
		want  bool
	}{
		{model: "", want: true},
		{model: "gpt-5.4", want: true},
		{model: "GPT-5.4-mini", want: true},
		{model: "opus", want: false},
		{model: "sonnet", want: false},
		{model: "haiku", want: false},
		{model: "claude-3-7-sonnet", want: false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(modelOrEmpty(tc.model), func(t *testing.T) {
			t.Parallel()

			if got := isCodexModel(tc.model); got != tc.want {
				t.Fatalf("isCodexModel(%q) = %t, want %t", tc.model, got, tc.want)
			}
		})
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

type roleTrackingAdapter struct {
	name domain.ProviderName
}

func (a roleTrackingAdapter) Name() domain.ProviderName {
	return a.name
}

func (a roleTrackingAdapter) RunLeader(_ context.Context, _ domain.Job) (string, error) {
	return fmt.Sprintf("%s:leader", a.name), nil
}

func (a roleTrackingAdapter) RunWorker(_ context.Context, _ domain.Job, task domain.LeaderOutput) (string, error) {
	return fmt.Sprintf("%s:worker:%s", a.name, task.TaskType), nil
}

func (a roleTrackingAdapter) RunPlanner(_ context.Context, _ domain.Job) (string, error) {
	return fmt.Sprintf("%s:planner", a.name), nil
}

func (a roleTrackingAdapter) RunEvaluator(_ context.Context, _ domain.Job) (string, error) {
	return fmt.Sprintf("%s:evaluator", a.name), nil
}

type profileEchoAdapter struct {
	name domain.ProviderName
}

func (a profileEchoAdapter) Name() domain.ProviderName {
	return a.name
}

func (a profileEchoAdapter) RunLeader(_ context.Context, job domain.Job) (string, error) {
	profile := job.RoleProfiles.ProfileFor(domain.RoleLeader, job.Provider)
	return fmt.Sprintf("%s:leader:%s", a.name, profile.Model), nil
}

func (a profileEchoAdapter) RunWorker(_ context.Context, job domain.Job, task domain.LeaderOutput) (string, error) {
	role := domain.RoleForTaskType(task.TaskType)
	profile := job.RoleProfiles.ProfileFor(role, job.Provider)
	return fmt.Sprintf("%s:%s:%s", a.name, role, profile.Model), nil
}

func (a profileEchoAdapter) RunPlanner(_ context.Context, job domain.Job) (string, error) {
	profile := job.RoleProfiles.ProfileFor(domain.RolePlanner, job.Provider)
	return fmt.Sprintf("%s:planner:%s", a.name, profile.Model), nil
}

func (a profileEchoAdapter) RunEvaluator(_ context.Context, job domain.Job) (string, error) {
	profile := job.RoleProfiles.ProfileFor(domain.RoleEvaluator, job.Provider)
	return fmt.Sprintf("%s:evaluator:%s", a.name, profile.Model), nil
}

func newTestCodexAdapter(t *testing.T, capture func(string, []string)) *CodexAdapter {
	t.Helper()

	return &CodexAdapter{
		executable: "go",
		probeArgs:  []string{"version"},
		probeTime:  2 * time.Second,
		runTime:    2 * time.Second,
		runCommand: func(_ context.Context, _ string, _ time.Duration, _ string, _ []string, _ string, args ...string) (CommandResult, error) {
			t.Helper()
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
			if capture != nil {
				capture(outputPath, args)
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
}

func newTestClaudeAdapter(t *testing.T, capture func(string, []string)) *ClaudeAdapter {
	t.Helper()

	return &ClaudeAdapter{
		executable: "go",
		probeArgs:  []string{"version"},
		probeTime:  2 * time.Second,
		runTime:    2 * time.Second,
		runCommand: func(_ context.Context, _ string, _ time.Duration, _ string, _ []string, stdinData string, args ...string) (CommandResult, error) {
			t.Helper()
			if capture != nil {
				capture(stdinData, args)
			}
			return CommandResult{Stdout: `{"structured_output":{"status":"success","summary":"ok"}}`}, nil
		},
	}
}

func assertCodexModelFlag(t *testing.T, args []string, want string) {
	t.Helper()

	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--model" {
			if args[i+1] != want {
				t.Fatalf("expected --model %q, got %q in args %v", want, args[i+1], args)
			}
			return
		}
	}
	t.Fatalf("expected --model %q in args %v", want, args)
}

func assertCodexModelFlagAbsent(t *testing.T, args []string) {
	t.Helper()

	for _, arg := range args {
		if arg == "--model" {
			t.Fatalf("expected --model to be absent from args %v", args)
		}
	}
}

func assertClaudeBaseArgs(t *testing.T, args []string) {
	t.Helper()

	want := []string{"-p", "--permission-mode", "dontAsk", "--output-format", "json", "--json-schema", "--no-session-persistence"}
	for _, token := range want {
		if !containsArg(args, token) {
			t.Fatalf("expected Claude args to include %q, got %v", token, args)
		}
	}
}

func assertClaudeModelFlag(t *testing.T, args []string, want string) {
	t.Helper()

	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--model" {
			if args[i+1] != want {
				t.Fatalf("expected --model %q, got %q in args %v", want, args[i+1], args)
			}
			return
		}
	}
	t.Fatalf("expected --model %q in args %v", want, args)
}

func assertClaudeJSONSchemaMinified(t *testing.T, args []string) {
	t.Helper()

	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--json-schema" {
			if strings.Contains(args[i+1], "\n") {
				t.Fatalf("expected minified --json-schema argument, got %q", args[i+1])
			}
			return
		}
	}
	t.Fatalf("expected --json-schema in args %v", args)
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func modelOrEmpty(model string) string {
	if model == "" {
		return "empty"
	}
	return model
}
