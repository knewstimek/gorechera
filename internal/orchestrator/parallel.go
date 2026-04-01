package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"gorechera/internal/domain"
	"gorechera/internal/schema"
)

const maxParallelWorkers = 2

const parallelSpecPrefix = "parallel:"

type parallelWorkerSpec struct {
	Target       string   `json:"target"`
	TaskType     string   `json:"task_type"`
	TaskText     string   `json:"task_text"`
	WriteScope   string   `json:"write_scope"`
	ArtifactRefs []string `json:"artifact_refs,omitempty"`
}

type parallelWorkerPlan struct {
	StepIndex int
	ScopeKey  string
	Task      domain.LeaderOutput
}

type parallelWorkerResult struct {
	Plan          parallelWorkerPlan
	Worker        domain.WorkerOutput
	ArtifactPaths []string
	TokenUsage    domain.TokenUsage
	Err           error
}

func buildWorkerPlans(leader domain.LeaderOutput) ([]parallelWorkerPlan, error) {
	if leader.Action == "run_workers" {
		return buildParallelWorkerPlansFromTasks(leader.Tasks)
	}
	primaryArtifacts, parallelSpecs, err := splitParallelArtifacts(leader.Artifacts)
	if err != nil {
		return nil, err
	}
	primary := parallelWorkerPlan{
		ScopeKey: scopeKey("primary", leader.Target, leader.TaskType),
		Task: domain.LeaderOutput{
			Action:    "run_worker",
			Target:    leader.Target,
			TaskType:  leader.TaskType,
			TaskText:  leader.TaskText,
			Artifacts: primaryArtifacts,
		},
	}

	plans := []parallelWorkerPlan{primary}
	if len(parallelSpecs) == 0 {
		return plans, nil
	}
	if len(parallelSpecs)+1 > maxParallelWorkers {
		return nil, fmt.Errorf("parallel fan-out exceeds max_parallel_workers=%d", maxParallelWorkers)
	}

	usedTargets := map[string]struct{}{normalizeKey(primary.Task.Target): {}}
	usedScopes := map[string]struct{}{normalizeKey(primary.Task.Target): {}}

	for _, spec := range parallelSpecs {
		if err := validateParallelSpec(spec); err != nil {
			return nil, err
		}
		normalizedTarget := normalizeKey(spec.Target)
		normalizedScope := normalizeKey(spec.WriteScope)
		if _, ok := usedTargets[normalizedTarget]; ok {
			return nil, fmt.Errorf("parallel fan-out requires disjoint targets; duplicate target %q", spec.Target)
		}
		if _, ok := usedScopes[normalizedTarget]; ok {
			return nil, fmt.Errorf("parallel fan-out requires disjoint targets and scopes; target %q overlaps a write scope", spec.Target)
		}
		if _, ok := usedTargets[normalizedScope]; ok {
			return nil, fmt.Errorf("parallel fan-out requires disjoint write scopes; duplicate scope %q", spec.WriteScope)
		}
		if _, ok := usedScopes[normalizedScope]; ok {
			return nil, fmt.Errorf("parallel fan-out requires disjoint write scopes; duplicate scope %q", spec.WriteScope)
		}
		usedTargets[normalizedTarget] = struct{}{}
		usedScopes[normalizedScope] = struct{}{}
		plans = append(plans, parallelWorkerPlan{
			ScopeKey: scopeKey("parallel", spec.WriteScope, spec.TaskType),
			Task: domain.LeaderOutput{
				Action:    "run_worker",
				Target:    spec.Target,
				TaskType:  spec.TaskType,
				TaskText:  spec.TaskText,
				Artifacts: append([]string(nil), spec.ArtifactRefs...),
			},
		})
	}

	return plans, nil
}

func buildParallelWorkerPlansFromTasks(tasks []domain.WorkerTask) ([]parallelWorkerPlan, error) {
	if len(tasks) == 0 {
		return nil, fmt.Errorf("parallel fan-out requires worker tasks")
	}
	if len(tasks) > maxParallelWorkers {
		return nil, fmt.Errorf("parallel fan-out exceeds max_parallel_workers=%d", maxParallelWorkers)
	}

	usedTargets := make(map[string]struct{}, len(tasks))
	usedScopes := make(map[string]struct{}, len(tasks))
	plans := make([]parallelWorkerPlan, 0, len(tasks))
	for _, task := range tasks {
		if strings.TrimSpace(task.Target) == "" {
			return nil, fmt.Errorf("parallel fan-out requires target")
		}
		if strings.TrimSpace(task.TaskType) == "" {
			return nil, fmt.Errorf("parallel fan-out requires task_type")
		}
		if strings.TrimSpace(task.TaskText) == "" {
			return nil, fmt.Errorf("parallel fan-out requires task_text")
		}

		normalizedTarget := normalizeKey(task.Target)
		scope := firstNonEmpty(extractWriteScope(task.Artifacts), task.Target)
		normalizedScope := normalizeKey(scope)
		if _, ok := usedTargets[normalizedTarget]; ok {
			return nil, fmt.Errorf("parallel fan-out requires disjoint targets; duplicate target %q", task.Target)
		}
		if _, ok := usedScopes[normalizedTarget]; ok {
			return nil, fmt.Errorf("parallel fan-out requires disjoint targets and scopes; target %q overlaps a write scope", task.Target)
		}
		if _, ok := usedTargets[normalizedScope]; ok {
			return nil, fmt.Errorf("parallel fan-out requires disjoint write scopes; duplicate scope %q", scope)
		}
		if _, ok := usedScopes[normalizedScope]; ok {
			return nil, fmt.Errorf("parallel fan-out requires disjoint write scopes; duplicate scope %q", scope)
		}
		usedTargets[normalizedTarget] = struct{}{}
		usedScopes[normalizedScope] = struct{}{}
		plans = append(plans, parallelWorkerPlan{
			ScopeKey: scopeKey("parallel", scope, task.TaskType),
			Task: domain.LeaderOutput{
				Action:    "run_worker",
				Target:    task.Target,
				TaskType:  task.TaskType,
				TaskText:  task.TaskText,
				Artifacts: append([]string(nil), task.Artifacts...),
			},
		})
	}
	return plans, nil
}

func splitParallelArtifacts(artifacts []string) ([]string, []parallelWorkerSpec, error) {
	regular := make([]string, 0, len(artifacts))
	parallelSpecs := make([]parallelWorkerSpec, 0)
	for _, raw := range artifacts {
		spec, ok, err := parseParallelSpec(raw)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			parallelSpecs = append(parallelSpecs, spec)
			continue
		}
		trimmed := strings.TrimSpace(raw)
		if trimmed != "" {
			regular = append(regular, trimmed)
		}
	}
	return regular, parallelSpecs, nil
}

func parseParallelSpec(raw string) (parallelWorkerSpec, bool, error) {
	trimmed := strings.TrimSpace(raw)
	if !strings.HasPrefix(strings.ToLower(trimmed), parallelSpecPrefix) {
		return parallelWorkerSpec{}, false, nil
	}
	payload := strings.TrimSpace(trimmed[len(parallelSpecPrefix):])
	if payload == "" {
		return parallelWorkerSpec{}, true, fmt.Errorf("parallel worker spec payload is empty")
	}
	var spec parallelWorkerSpec
	if err := json.Unmarshal([]byte(payload), &spec); err != nil {
		return parallelWorkerSpec{}, true, fmt.Errorf("invalid parallel worker spec: %w", err)
	}
	return spec, true, nil
}

func validateParallelSpec(spec parallelWorkerSpec) error {
	if strings.TrimSpace(spec.Target) == "" {
		return fmt.Errorf("parallel worker requires target")
	}
	if strings.TrimSpace(spec.TaskType) == "" {
		return fmt.Errorf("parallel worker requires task_type")
	}
	if strings.TrimSpace(spec.TaskText) == "" {
		return fmt.Errorf("parallel worker requires task_text")
	}
	if strings.TrimSpace(spec.WriteScope) == "" {
		return fmt.Errorf("parallel worker requires write_scope")
	}
	return nil
}

func scopeKey(prefix, scope, taskType string) string {
	return fmt.Sprintf("%s:%s:%s", prefix, normalizeKey(scope), normalizeKey(taskType))
}

func normalizeKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func extractWriteScope(artifacts []string) string {
	for _, raw := range artifacts {
		spec, ok, err := parseParallelSpec(raw)
		if err == nil && ok {
			return spec.WriteScope
		}
	}
	return ""
}

func (s *Service) runParallelWorkerPlans(ctx context.Context, job *domain.Job, plans []parallelWorkerPlan) error {
	if len(plans) == 0 {
		return nil
	}

	now := time.Now().UTC()
	job.Status = domain.JobStatusWaitingWorker
	start := len(job.Steps)
	for i := range plans {
		plans[i].StepIndex = job.CurrentStep + 1
		plans[i].Task = decorateTaskForVerification(*job, plans[i].Task)
		job.CurrentStep++
		job.Steps = append(job.Steps, domain.Step{
			Index:     plans[i].StepIndex,
			Target:    plans[i].Task.Target,
			TaskType:  plans[i].Task.TaskType,
			TaskText:  plans[i].Task.TaskText,
			Status:    domain.StepStatusActive,
			StartedAt: now,
		})
	}

	s.addEvent(job, "parallel_workers_requested", fmt.Sprintf("parallel fan-out with %d workers", len(plans)))
	s.touch(job)
	if err := s.state.SaveJob(ctx, job); err != nil {
		return err
	}

	results := make([]parallelWorkerResult, len(plans))
	var wg sync.WaitGroup
	for i := range plans {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = s.executeParallelWorkerPlan(ctx, *job, plans[i])
		}(i)
	}
	wg.Wait()

	overallStatus := domain.JobStatusRunning
	var blockedReason string
	var failedReason string

	for i := range results {
		res := results[i]
		step := &job.Steps[start+i]
		s.accumulateTokenUsage(job, step.Index, res.TokenUsage)
		step.FinishedAt = time.Now().UTC()
		if res.Err != nil {
			step.Status = domain.StepStatusFailed
			step.ErrorReason = res.Err.Error()
			step.Summary = res.Err.Error()
			if failedReason == "" {
				failedReason = res.Err.Error()
			}
			overallStatus = domain.JobStatusFailed
			continue
		}

		step.Artifacts = append([]string(nil), res.ArtifactPaths...)
		step.Summary = res.Worker.Summary
		step.BlockedReason = res.Worker.BlockedReason
		step.ErrorReason = res.Worker.ErrorReason

		switch res.Worker.Status {
		case "success":
			step.Status = domain.StepStatusSucceeded
		case "blocked":
			step.Status = domain.StepStatusBlocked
			if blockedReason == "" {
				blockedReason = firstNonEmpty(res.Worker.BlockedReason, res.Worker.Summary, "parallel worker blocked")
			}
		case "failed":
			step.Status = domain.StepStatusFailed
			if failedReason == "" {
				failedReason = firstNonEmpty(res.Worker.ErrorReason, res.Worker.Summary, "parallel worker failed")
			}
		}
	}

	switch {
	case blockedReason != "":
		overallStatus = domain.JobStatusBlocked
		job.BlockedReason = blockedReason
		job.FailureReason = ""
		s.addEvent(job, "parallel_workers_blocked", blockedReason)
	case failedReason != "":
		if overallStatus != domain.JobStatusFailed {
			overallStatus = domain.JobStatusRunning
		}
		job.FailureReason = failedReason
		job.BlockedReason = ""
		s.addEvent(job, "parallel_workers_failed", failedReason)
	default:
		overallStatus = domain.JobStatusRunning
		s.addEvent(job, "parallel_workers_succeeded", fmt.Sprintf("parallel fan-out with %d workers completed", len(plans)))
	}

	job.Status = overallStatus
	job.LeaderContextSummary = summarizeParallelResults(results)
	s.touch(job)
	return s.state.SaveJob(ctx, job)
}

func (s *Service) executeParallelWorkerPlan(ctx context.Context, job domain.Job, plan parallelWorkerPlan) parallelWorkerResult {
	task := plan.Task
	if strings.EqualFold(task.TaskType, "test") {
		if contract, path, err := loadVerificationContract(job); err == nil {
			task.TaskText = strings.TrimSpace(strings.Join([]string{
				task.TaskText,
				verificationContractPrompt(contract, path),
			}, "\n\n"))
		}
	}

	rawWorker, err := s.sessions.RunWorker(ctx, job, task)
	if err != nil {
		return parallelWorkerResult{Plan: plan, Err: fmt.Errorf("worker execution failed: %w", err)}
	}
	usage := estimateProviderUsage(rawWorker, job, task)

	var worker domain.WorkerOutput
	if err := json.Unmarshal([]byte(rawWorker), &worker); err != nil {
		return parallelWorkerResult{Plan: plan, TokenUsage: usage, Err: fmt.Errorf("invalid worker json: %w", err)}
	}
	if err := schema.ValidateWorkerOutput(worker); err != nil {
		return parallelWorkerResult{Plan: plan, TokenUsage: usage, Err: fmt.Errorf("worker schema validation failed: %w", err)}
	}

	artifacts, err := s.artifacts.MaterializeWorkerArtifacts(job.ID, plan.StepIndex, worker)
	if err != nil {
		return parallelWorkerResult{Plan: plan, TokenUsage: usage, Err: fmt.Errorf("artifact materialization failed: %w", err)}
	}

	return parallelWorkerResult{
		Plan:          plan,
		Worker:        worker,
		ArtifactPaths: artifacts,
		TokenUsage:    usage,
	}
}

func decorateTaskForVerification(job domain.Job, task domain.LeaderOutput) domain.LeaderOutput {
	if !strings.EqualFold(task.TaskType, "test") {
		return task
	}
	if contract, path, err := loadVerificationContract(job); err == nil {
		task.TaskText = strings.TrimSpace(strings.Join([]string{
			task.TaskText,
			verificationContractPrompt(contract, path),
		}, "\n\n"))
	}
	return task
}

func summarizeParallelResults(results []parallelWorkerResult) string {
	summaries := make([]string, 0, len(results))
	for _, res := range results {
		if res.Err != nil {
			summaries = append(summaries, res.Err.Error())
			continue
		}
		if strings.TrimSpace(res.Worker.Summary) != "" {
			summaries = append(summaries, res.Worker.Summary)
		}
	}
	return strings.Join(summaries, " | ")
}
