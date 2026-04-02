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
			if !containsArg(capturedArgs, "--ephemeral") {
				t.Fatalf("expected %s args to include --ephemeral, got %v", tc.name, capturedArgs)
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
			if !containsArg(capturedArgs, "--ephemeral") {
				t.Fatalf("expected args to include --ephemeral, got %v", capturedArgs)
			}
			assertCodexModelFlagAbsent(t, capturedArgs)
		})
	}
}

func TestCodexAdapterFallsBackToFreshForLegacyCLI(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	var invocations [][]string
	adapter := &CodexAdapter{
		executable: "go",
		probeArgs:  []string{"version"},
		probeTime:  2 * time.Second,
		runTime:    2 * time.Second,
		runCommand: func(_ context.Context, _ string, _ time.Duration, _ string, _ []string, _ string, args ...string) (CommandResult, error) {
			invocations = append(invocations, append([]string(nil), args...))

			if containsArg(args, "--ephemeral") {
				return CommandResult{}, errors.New("error: unexpected argument '--ephemeral' found")
			}
			if !containsArg(args, "--fresh") {
				return CommandResult{}, errors.New("expected fallback invocation to include --fresh")
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
			if err := os.WriteFile(outputPath, []byte(`{"action":"complete","target":"none","task_type":"none","reason":"legacy codex accepted fallback"}`), 0o644); err != nil {
				return CommandResult{}, err
			}
			return CommandResult{}, nil
		},
	}

	out, err := adapter.RunLeader(context.Background(), domain.Job{
		Goal:         "Support legacy codex sessions",
		WorkspaceDir: root,
		Provider:     domain.ProviderCodex,
		RoleProfiles: domain.RoleProfiles{
			Leader: domain.ExecutionProfile{Provider: domain.ProviderCodex, Model: "gpt-5.4"},
		},
	})
	if err != nil {
		t.Fatalf("expected fallback run to succeed, got error: %v", err)
	}
	if out == "" {
		t.Fatal("expected leader output")
	}
	if len(invocations) != 2 {
		t.Fatalf("expected 2 codex invocations, got %d", len(invocations))
	}
	if !containsArg(invocations[0], "--ephemeral") {
		t.Fatalf("expected first invocation to include --ephemeral, got %v", invocations[0])
	}
	if !containsArg(invocations[1], "--fresh") {
		t.Fatalf("expected second invocation to include --fresh, got %v", invocations[1])
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
	if !strings.Contains(plannerPrompt, "invariants_to_preserve") {
		t.Fatal("expected planner prompt to request invariants_to_preserve")
	}
	if !strings.Contains(plannerPrompt, "use [] when none apply") {
		t.Fatal("expected planner prompt to require an empty invariants array when none apply")
	}
	evaluatorPrompt := buildEvaluatorPrompt(job)
	if !strings.Contains(evaluatorPrompt, "Verification contract") {
		t.Fatal("expected evaluator prompt to include verification contract payload")
	}
	if !strings.Contains(evaluatorPrompt, "Do NOT pass the job merely because one implement step succeeded.") {
		t.Fatal("expected evaluator prompt to enforce gate-based completion")
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
	if !strings.Contains(workerPrompt, "Task why:") || !strings.Contains(workerPrompt, "Scope boundary:") {
		t.Fatal("expected worker prompt to render task why and scope boundary sections")
	}
}

func TestPlannerSchemaUsesStrictRequiredProperties(t *testing.T) {
	t.Parallel()

	var doc struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal([]byte(plannerSchema()), &doc); err != nil {
		t.Fatalf("failed to decode planner schema: %v", err)
	}
	if _, ok := doc.Properties["invariants_to_preserve"]; !ok {
		t.Fatal("expected planner schema to declare invariants_to_preserve")
	}
	if !containsString(doc.Required, "invariants_to_preserve") {
		t.Fatal("expected planner schema to require invariants_to_preserve in strict mode")
	}
	if len(doc.Properties) != len(doc.Required) {
		t.Fatalf("expected strict schema to require every top-level property, got %d properties and %d required entries", len(doc.Properties), len(doc.Required))
	}
}

func TestReviewerPromptUsesAdversarialGuidance(t *testing.T) {
	t.Parallel()

	prompt := buildWorkerPrompt(domain.Job{
		Goal:        "Harden restart safety",
		Provider:    domain.ProviderMock,
		Constraints: []string{"Do not break retry idempotency"},
	}, domain.LeaderOutput{
		Action:   "run_worker",
		Target:   "C",
		TaskType: "review",
		TaskText: "Review the recovery patch for regressions\n\ntask_why: validate lifecycle safety before completion\n\nscope_boundary: stay within the recovery patch and adjacent tests",
	})

	if !strings.Contains(prompt, "You are a reviewer component") {
		t.Fatal("expected reviewer-specific prompt")
	}
	if !strings.Contains(prompt, "look for counterexamples") {
		t.Fatal("expected adversarial reviewer guidance")
	}
	if !strings.Contains(prompt, "idempotency") || !strings.Contains(prompt, "state-transition") {
		t.Fatal("expected reviewer prompt to mention lifecycle invariants")
	}
	if !strings.Contains(prompt, "Task why:\nvalidate lifecycle safety before completion") {
		t.Fatal("expected reviewer prompt to render parsed task why")
	}
	if !strings.Contains(prompt, "Invariants to preserve:\n- Do not break retry idempotency") {
		t.Fatal("expected reviewer prompt to render invariants")
	}
}

func TestAuditTaskUsesReviewerPromptBranch(t *testing.T) {
	t.Parallel()

	prompt := buildWorkerPrompt(domain.Job{
		Goal:     "Check restart and retry safety",
		Provider: domain.ProviderMock,
	}, domain.LeaderOutput{
		Action:   "run_worker",
		Target:   "C",
		TaskType: "audit",
		TaskText: "Audit lifecycle and retry invariants for hidden regressions",
	})

	if !strings.Contains(prompt, "Assigned audit task") {
		t.Fatal("expected audit task to use reviewer/audit prompt branch")
	}
	if !strings.Contains(prompt, "risk discovery and contract validation") {
		t.Fatal("expected audit prompt to stay risk-focused")
	}
	if !strings.Contains(prompt, "Task why:") || !strings.Contains(prompt, "Scope boundary:") {
		t.Fatal("expected audit prompt to include contextual sections")
	}
}

func TestWorkerPromptRendersContextSectionsAcrossRoles(t *testing.T) {
	t.Parallel()

	job := domain.Job{
		Goal:                 "Propagate prompt context safely",
		Provider:             domain.ProviderMock,
		Constraints:          []string{"Keep reviewer/tester separation intact", "Do not modify evaluator prompts"},
		LeaderContextSummary: "Workers need task intent to avoid stale-status bugs.",
	}
	taskText := "Implement prompt updates in protocol.go\n\ntask_why: workers need intent and invariants to avoid stale-status bugs\n\nscope_boundary: limit changes to prompt construction and related tests"

	cases := []struct {
		name     string
		taskType string
		roleText string
	}{
		{name: "executor", taskType: "implement", roleText: "You are an executor worker"},
		{name: "reviewer", taskType: "review", roleText: "You are a reviewer component"},
		{name: "tester", taskType: "test", roleText: "You are a tester component"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			prompt := buildWorkerPrompt(job, domain.LeaderOutput{
				Action:   "run_worker",
				Target:   "B",
				TaskType: tc.taskType,
				TaskText: taskText,
			})
			if !strings.Contains(prompt, tc.roleText) {
				t.Fatalf("expected role-specific prompt text %q", tc.roleText)
			}
			if !strings.Contains(prompt, "Task why:\nworkers need intent and invariants to avoid stale-status bugs") {
				t.Fatal("expected task why section in worker prompt")
			}
			if !strings.Contains(prompt, "Invariants to preserve:\n- Keep reviewer/tester separation intact\n- Do not modify evaluator prompts") {
				t.Fatal("expected invariants section in worker prompt")
			}
			if !strings.Contains(prompt, "Scope boundary:\nlimit changes to prompt construction and related tests") {
				t.Fatal("expected scope boundary section in worker prompt")
			}
		})
	}
}

func TestLeaderPromptIncludesRunWorkersGuidance(t *testing.T) {
	t.Parallel()

	prompt := buildLeaderPrompt(domain.Job{
		Goal:        "Fan out parallel work",
		Provider:    domain.ProviderMock,
		Constraints: []string{"Keep reviewer/tester separation intact"},
	})
	if !strings.Contains(prompt, "run_workers") {
		t.Fatal("expected leader prompt to mention run_workers")
	}
	// Updated prompt uses "exactly 2 workers" phrasing instead of "at most 2 workers"
	if !strings.Contains(prompt, "2 worker") {
		t.Fatal("expected leader prompt to mention parallel worker limit")
	}
	if !strings.Contains(prompt, "dispatch an explicit review step") {
		t.Fatal("expected leader prompt to include conditional audit guidance")
	}
	if !strings.Contains(prompt, `"audit"`) {
		t.Fatal("expected leader prompt to mention audit task type")
	}
	if !strings.Contains(prompt, "Planning invariants to preserve:\n- Keep reviewer/tester separation intact") {
		t.Fatal("expected leader prompt to include planning invariants")
	}
	if !strings.Contains(prompt, "task_why:") || !strings.Contains(prompt, "scope_boundary:") {
		t.Fatal("expected leader prompt to instruct task_why and scope_boundary shaping")
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

func TestSessionManagerRetriesFallbackModelOnceOnPreStructuredFailure(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	adapter := &fallbackModelRetryAdapter{name: domain.ProviderName("model-retry")}
	registry.Register(adapter)

	manager := NewSessionManager(registry)
	out, err := manager.RunLeader(context.Background(), domain.Job{
		Provider: domain.ProviderName("unused-primary"),
		RoleProfiles: domain.RoleProfiles{
			Leader: domain.ExecutionProfile{
				Provider:      adapter.name,
				Model:         "primary-model",
				FallbackModel: "fallback-model",
			},
		},
	})
	if err != nil {
		t.Fatalf("expected fallback model retry to succeed, got error: %v", err)
	}
	if out != "model-retry:leader:fallback-model" {
		t.Fatalf("unexpected fallback model output: %s", out)
	}
	if adapter.calls != 2 {
		t.Fatalf("expected exactly 2 invocations, got %d", adapter.calls)
	}
	if len(adapter.models) != 2 || adapter.models[0] != "primary-model" || adapter.models[1] != "fallback-model" {
		t.Fatalf("expected retry models [primary-model fallback-model], got %v", adapter.models)
	}
}

func TestSessionManagerDoesNotRetryFallbackModelWhenBlankOrEqual(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		fallbackModel string
	}{
		{name: "blank", fallbackModel: ""},
		{name: "equal", fallbackModel: "primary-model"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			registry := NewRegistry()
			adapter := &alwaysFailingLeaderAdapter{name: domain.ProviderName("no-retry")}
			registry.Register(adapter)

			manager := NewSessionManager(registry)
			_, err := manager.RunLeader(context.Background(), domain.Job{
				Provider: domain.ProviderName("unused-primary"),
				RoleProfiles: domain.RoleProfiles{
					Leader: domain.ExecutionProfile{
						Provider:      adapter.name,
						Model:         "primary-model",
						FallbackModel: tc.fallbackModel,
					},
				},
			})
			if err == nil {
				t.Fatal("expected provider error")
			}
			if adapter.calls != 1 {
				t.Fatalf("expected exactly 1 invocation, got %d", adapter.calls)
			}
			if len(adapter.models) != 1 || adapter.models[0] != "primary-model" {
				t.Fatalf("expected only primary model attempt, got %v", adapter.models)
			}
		})
	}
}

func TestSessionManagerFallsBackToSecondaryProvider(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	adapter := &fallbackLeaderAdapter{name: domain.ProviderName("role-fallback")}
	registry.Register(adapter)

	manager := NewSessionManager(registry)
	out, err := manager.RunLeader(context.Background(), domain.Job{
		Provider: domain.ProviderName("unused-primary"),
		RoleProfiles: domain.RoleProfiles{
			Leader: domain.ExecutionProfile{
				Provider:         domain.ProviderName("missing-primary"),
				FallbackProvider: domain.ProviderName("role-fallback"),
				FallbackModel:    "unused-fallback-model",
			},
		},
	})
	if err != nil {
		t.Fatalf("expected fallback provider to succeed, got error: %v", err)
	}
	if out != `{"action":"complete","target":"none","task_type":"none","reason":"fallback used"}` {
		t.Fatalf("unexpected fallback output: %s", out)
	}
	if adapter.calls != 1 {
		t.Fatalf("expected fallback provider to run once, got %d calls", adapter.calls)
	}
}

func TestSessionManagerDoesNotRetryFallbackModelAfterFallbackProviderSelection(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	adapter := &alwaysFailingLeaderAdapter{name: domain.ProviderName("fallback-provider")}
	registry.Register(adapter)

	manager := NewSessionManager(registry)
	_, err := manager.RunLeader(context.Background(), domain.Job{
		Provider: domain.ProviderName("unused-primary"),
		RoleProfiles: domain.RoleProfiles{
			Leader: domain.ExecutionProfile{
				Provider:         domain.ProviderName("missing-primary"),
				FallbackProvider: adapter.name,
				Model:            "primary-model",
				FallbackModel:    "fallback-model",
			},
		},
	})
	if err == nil {
		t.Fatal("expected fallback provider error")
	}
	if adapter.calls != 1 {
		t.Fatalf("expected fallback provider to run once without model retry, got %d calls", adapter.calls)
	}
	if len(adapter.models) != 1 || adapter.models[0] != "primary-model" {
		t.Fatalf("expected only the primary model attempt on fallback provider, got %v", adapter.models)
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
	name  domain.ProviderName
	calls int
}

func (a *fallbackLeaderAdapter) Name() domain.ProviderName {
	return a.name
}

func (a *fallbackLeaderAdapter) RunLeader(_ context.Context, _ domain.Job) (string, error) {
	a.calls++
	return `{"action":"complete","target":"none","task_type":"none","reason":"fallback used"}`, nil
}

func (a *fallbackLeaderAdapter) RunWorker(_ context.Context, _ domain.Job, _ domain.LeaderOutput) (string, error) {
	return `{"status":"success","summary":"fallback worker"}`, nil
}

type fallbackModelRetryAdapter struct {
	name   domain.ProviderName
	calls  int
	models []string
}

func (a *fallbackModelRetryAdapter) Name() domain.ProviderName {
	return a.name
}

func (a *fallbackModelRetryAdapter) RunLeader(_ context.Context, job domain.Job) (string, error) {
	a.calls++
	profile := job.RoleProfiles.ProfileFor(domain.RoleLeader, job.Provider)
	a.models = append(a.models, profile.Model)
	if a.calls == 1 {
		return "", commandFailedError(a.name, "test-provider", errors.New("primary model failed"))
	}
	return fmt.Sprintf("%s:leader:%s", a.name, profile.Model), nil
}

func (a *fallbackModelRetryAdapter) RunWorker(_ context.Context, _ domain.Job, _ domain.LeaderOutput) (string, error) {
	return `{"status":"success","summary":"fallback model worker"}`, nil
}

type alwaysFailingLeaderAdapter struct {
	name   domain.ProviderName
	calls  int
	models []string
}

func (a *alwaysFailingLeaderAdapter) Name() domain.ProviderName {
	return a.name
}

func (a *alwaysFailingLeaderAdapter) RunLeader(_ context.Context, job domain.Job) (string, error) {
	a.calls++
	profile := job.RoleProfiles.ProfileFor(domain.RoleLeader, job.Provider)
	a.models = append(a.models, profile.Model)
	return "", commandFailedError(a.name, "test-provider", errors.New("command failed"))
}

func (a *alwaysFailingLeaderAdapter) RunWorker(_ context.Context, _ domain.Job, _ domain.LeaderOutput) (string, error) {
	return `{"status":"success","summary":"always failing worker"}`, nil
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

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
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
