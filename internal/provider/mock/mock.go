package mock

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gorchera/internal/domain"
)

type Adapter struct{}

func New() *Adapter {
	return &Adapter{}
}

func (a *Adapter) Name() domain.ProviderName {
	return domain.ProviderMock
}

func (a *Adapter) RunLeader(_ context.Context, job domain.Job) (string, error) {
	lastStatus := ""
	lastType := ""
	if len(job.Steps) > 0 {
		last := job.Steps[len(job.Steps)-1]
		lastStatus = string(last.Status)
		lastType = last.TaskType
	}

	var out domain.LeaderOutput
	switch {
	case lastStatus == string(domain.StepStatusBlocked):
		out = domain.LeaderOutput{
			Action:   "blocked",
			Target:   "none",
			TaskType: "none",
			Reason:   fmt.Sprintf("worker blocked during %s", lastType),
		}
	case lastStatus == string(domain.StepStatusFailed):
		out = domain.LeaderOutput{
			Action:   "fail",
			Target:   "none",
			TaskType: "none",
			Reason:   fmt.Sprintf("worker failed during %s", lastType),
		}
	case len(job.Steps) == 0 && strings.Contains(strings.ToLower(job.Goal), "parallel"):
		out = domain.LeaderOutput{
			Action: "run_workers",
			Tasks: []domain.WorkerTask{
				{
					Target:   "B",
					TaskType: "implement",
					TaskText: "Create the parallel execution scaffolding for the orchestrator core.",
					Artifacts: []string{
						"parallel_execution_plan.md",
					},
					NextHint: "Return a compact implementation summary.",
				},
				{
					Target:   "C",
					TaskType: "search",
					TaskText: "Search for policy and schema regression risks in the execution scaffolding.",
					Artifacts: []string{
						"parallel_execution_plan.md",
						"search_notes.md",
					},
					NextHint: "Return search findings and any blocking issues.",
				},
			},
			Reason: "parallel fan-out is appropriate for the initial goal",
		}
	case !hasSucceeded(job, "implement"):
		out = domain.LeaderOutput{
			Action:   "run_worker",
			Target:   "B",
			TaskType: "implement",
			TaskText: "Create the initial orchestrator implementation skeleton.",
			Artifacts: []string{
				"project_summary.md",
			},
			NextHint: "Return implementation artifacts and a concise summary.",
		}
	case !hasSucceeded(job, "search"):
		out = domain.LeaderOutput{
			Action:   "run_worker",
			Target:   "C",
			TaskType: "search",
			TaskText: "Search codebase for integration points and summarize findings.",
			Artifacts: []string{
				"patch.diff",
				"implementation_notes.md",
			},
			NextHint: "Return search findings.",
		}
	case !hasSucceeded(job, "test"):
		out = domain.LeaderOutput{
			Action:   "run_worker",
			Target:   "D",
			TaskType: "test",
			TaskText: "Run the designated validation checks and summarize the result.",
			Artifacts: []string{
				"review_report.json",
			},
			NextHint: "Return test status and artifact references.",
		}
	default:
		out = domain.LeaderOutput{
			Action:   "complete",
			Target:   "none",
			TaskType: "none",
			Reason:   "mock provider completed implement, search, and test phases",
		}
	}

	body, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func (a *Adapter) RunPlanner(_ context.Context, job domain.Job) (string, error) {
	plan := domain.PlanningArtifact{
		Goal:         job.Goal,
		TechStack:    job.TechStack,
		WorkspaceDir: job.WorkspaceDir,
		Summary:      fmt.Sprintf("planner prepared a plan for %q", job.Goal),
		ProductScope: []string{
			"stateful multi-agent orchestration core",
			"planner, evaluator, and worker phase separation",
			"role-based execution profiles for provider selection",
		},
		NonGoals: []string{
			"interactive assistant UX",
			"unguarded autonomous writes",
		},
		ProposedSteps: []string{
			"draft product spec",
			"define sprint contract",
			"execute implementation loop",
			"gate completion on evaluator",
		},
		Acceptance: append([]string(nil), job.DoneCriteria...),
		SuccessSignals: []string{
			"planner artifact is persisted",
			"leader and worker phases can consume the result",
		},
		VerificationContract: &domain.VerificationContract{
			Version: 1,
			Goal:    job.Goal,
			Scope: []string{
				"implementation",
				"review",
				"test",
			},
			RequiredCommands: []string{"go test ./..."},
			RequiredArtifacts: []string{
				"planner artifact",
				"sprint contract",
				"evaluator report",
			},
			RequiredChecks: []string{
				"job reached done only after evaluator pass",
				"tester followed verification contract",
			},
			DisallowedActions: []string{"uncontracted completion", "unreviewed skip"},
			MaxSeconds:        300,
			Notes:             "tester must report evidence, not self-approve",
			OwnerRole:         domain.RoleTester,
		},
	}
	body, err := json.Marshal(plan)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func (a *Adapter) RunEvaluator(_ context.Context, job domain.Job) (string, error) {
	missing := missingRequired(job)
	verificationReason := "verification contract not provided"
	if job.VerificationContract != nil {
		verificationReason = fmt.Sprintf("verification contract checks: %d", len(job.VerificationContract.RequiredChecks))
	}
	report := domain.EvaluatorReport{
		Status:           "blocked",
		Passed:           false,
		Score:            scoreFromMissing(job, missing),
		Reason:           fmt.Sprintf("missing required step coverage: %s; %s", strings.Join(missing, ", "), verificationReason),
		MissingStepTypes: missing,
		Evidence:         successEvidence(job),
		ContractRef:      job.SprintContractRef,
	}
	if len(missing) == 0 {
		report.Status = "passed"
		report.Passed = true
		report.Reason = "mock evaluator confirmed required step coverage and verification contract"
	}
	body, err := json.Marshal(report)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func (a *Adapter) RunWorker(_ context.Context, job domain.Job, task domain.LeaderOutput) (string, error) {
	out := domain.WorkerOutput{
		Status:                "success",
		Summary:               fmt.Sprintf("%s completed for goal %q", task.TaskType, job.Goal),
		NextRecommendedAction: nextAction(task.TaskType),
	}

	switch task.TaskType {
	case "implement":
		out.Artifacts = []string{"patch.diff", "implementation_notes.md"}
	case "search":
		out.Artifacts = []string{"search_report.json"}
	case "test":
		out.Artifacts = []string{"test_report.json", "verification_evidence.json"}
		if job.VerificationContract != nil {
			out.Summary = fmt.Sprintf("%s followed %d verification checks", task.TaskType, len(job.VerificationContract.RequiredChecks))
		}
	default:
		out.Artifacts = []string{"worker_output.json"}
	}

	body, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func hasSucceeded(job domain.Job, taskType string) bool {
	for _, step := range job.Steps {
		if step.TaskType == taskType && step.Status == domain.StepStatusSucceeded {
			return true
		}
	}
	return false
}

func nextAction(taskType string) string {
	switch taskType {
	case "implement":
		return "search"
	case "search":
		return "test"
	default:
		return "complete"
	}
}

func missingRequired(job domain.Job) []string {
	required := []string{"implement", "search", "test"}
	if hasSystem(job) {
		required = append(required, "search")
	}
	seen := make(map[string]struct{})
	for _, step := range job.Steps {
		if step.Status == domain.StepStatusSucceeded {
			seen[step.TaskType] = struct{}{}
		}
	}
	missing := make([]string, 0, len(required))
	for _, req := range required {
		if _, ok := seen[req]; !ok {
			missing = append(missing, req)
		}
	}
	return missing
}

func scoreFromMissing(job domain.Job, missing []string) int {
	required := 3
	if hasSystem(job) {
		required++
	}
	if required <= 0 {
		return 0
	}
	return (required - len(missing)) * 100 / required
}

func hasSystem(job domain.Job) bool {
	for _, criterion := range job.DoneCriteria {
		if strings.Contains(strings.ToLower(criterion), "system") {
			return true
		}
	}
	return false
}

func successEvidence(job domain.Job) []string {
	evidence := make([]string, 0, len(job.Steps))
	for _, step := range job.Steps {
		if step.Status == domain.StepStatusSucceeded {
			evidence = append(evidence, fmt.Sprintf("%s:%s", step.Target, step.TaskType))
		}
	}
	return evidence
}
