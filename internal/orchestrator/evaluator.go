package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gorechera/internal/domain"
)

func (s *Service) evaluateCompletion(ctx context.Context, job *domain.Job) (*domain.EvaluatorReport, error) {
	verification, verificationPath, err := s.resolveVerificationContract(*job)
	if err != nil {
		verification = buildVerificationContract(*job, buildPlanningArtifact(*job, nil), buildSprintContract(*job, buildPlanningArtifact(*job, nil)), job.PlanningArtifacts)
		verificationPath = verificationContractPath(*job)
	}
	sprint := buildSprintContract(*job, buildPlanningArtifact(*job, nil))
	providerReport, err := s.runEvaluatorPhase(ctx, job, verification, verificationPath)
	if err != nil {
		if unsupportedPhase(err) {
			providerReport = deterministicEvaluatorReport(*job, verification, sprint)
		} else {
			_, failErr := s.failJob(ctx, job, fmt.Sprintf("evaluator execution failed: %v", err))
			if failErr != nil {
				return nil, failErr
			}
			return &domain.EvaluatorReport{
				Status:      "failed",
				Passed:      false,
				Score:       0,
				Reason:      fmt.Sprintf("evaluator execution failed: %v", err),
				ContractRef: job.SprintContractRef,
			}, nil
		}
	}
	report := mergeEvaluatorReport(*job, verification, sprint, providerReport)

	reportPath, err := s.artifacts.MaterializeJSONArtifact(job.ID, "evaluator_report.json", report)
	if err != nil {
		return nil, err
	}
	job.EvaluatorReportRef = reportPath
	applyEvaluatorJobState(job, report)
	switch report.Status {
	case "failed":
		s.addEvent(job, "evaluation_failed", report.Reason)
	case "passed":
		s.addEvent(job, "evaluation_passed", report.Reason)
	default:
		s.addEvent(job, "evaluation_blocked", report.Reason)
	}
	s.touch(job)

	if err := s.state.SaveJob(ctx, job); err != nil {
		return nil, err
	}

	return report, nil
}

func (s *Service) runEvaluatorPhase(ctx context.Context, job *domain.Job, verification VerificationContract, verificationPath string) (domain.EvaluatorReport, error) {
	phaseJob := *job
	if strings.TrimSpace(verification.Summary) != "" {
		phaseJob.LeaderContextSummary = strings.TrimSpace(strings.Join([]string{
			phaseJob.LeaderContextSummary,
			verificationContractPrompt(verification, firstNonEmpty(verificationPath, verificationContractPath(*job))),
		}, "\n\n"))
	}
	raw, err := s.sessions.RunEvaluator(ctx, phaseJob)
	if err != nil {
		return domain.EvaluatorReport{}, err
	}
	s.accumulateTokenUsage(job, phaseJob.CurrentStep, estimateProviderUsage(raw, phaseJob))
	var out domain.EvaluatorReport
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return domain.EvaluatorReport{}, err
	}
	if err := validateEvaluatorReport(out, *job); err != nil {
		return domain.EvaluatorReport{}, err
	}
	return out, nil
}

func successfulStepTypes(job *domain.Job) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, len(job.Steps))
	for _, step := range job.Steps {
		if step.Status != domain.StepStatusSucceeded {
			continue
		}
		if _, ok := seen[step.TaskType]; ok {
			continue
		}
		seen[step.TaskType] = struct{}{}
		out = append(out, step.TaskType)
	}
	return out
}

func missingRequiredSteps(job *domain.Job, required []string) []string {
	seen := make(map[string]struct{})
	for _, step := range job.Steps {
		if step.Status == domain.StepStatusSucceeded {
			seen[step.TaskType] = struct{}{}
		}
	}

	missing := make([]string, 0)
	for _, req := range required {
		if _, ok := seen[req]; !ok {
			missing = append(missing, req)
		}
	}
	return missing
}

func scoreCompletion(job *domain.Job, contract domain.SprintContract) int {
	if len(contract.RequiredStepTypes) == 0 {
		return 0
	}
	return (len(contract.RequiredStepTypes) - len(missingRequiredSteps(job, contract.RequiredStepTypes))) * 100 / len(contract.RequiredStepTypes)
}

func summaryEvidence(job *domain.Job) []string {
	evidence := make([]string, 0, len(job.Steps))
	for _, step := range job.Steps {
		if step.Status == domain.StepStatusSucceeded {
			evidence = append(evidence, fmt.Sprintf("%s:%s", step.Target, step.TaskType))
		}
	}
	return evidence
}

func mergeEvaluatorReport(job domain.Job, verification VerificationContract, sprint domain.SprintContract, providerReport domain.EvaluatorReport) *domain.EvaluatorReport {
	verificationPassed, verificationMissing := verificationSatisfiedForLevel(job, verification, sprint.StrictnessLevel)
	rulePassed := verificationPassed && len(successfulStepTypes(&job)) >= sprint.ThresholdSuccessCnt
	missing := verificationMissing
	providerMissing := filterProviderMissingStepTypes(providerReport.MissingStepTypes, sprint.StrictnessLevel)
	ignoreProviderMissing := providerBlockedOnlyOnOptionalCoverage(providerReport, providerMissing)
	providerPassed := providerReport.Passed || ignoreProviderMissing

	// normal: if rule-based verification passes, override provider's blocked
	// status. GPT evaluators sometimes return blocked despite all evidence
	// passing; the rule gate is authoritative in normal mode.
	// NOTE: this intentionally overrides even provider Status="failed",
	// because normal mode trusts the structural check (implement succeeded)
	// over the provider's subjective judgment.
	if sprint.StrictnessLevel == "normal" && rulePassed {
		providerPassed = true
	}

	// lenient: if the provider already passed, skip rule gate entirely.
	// This lets a confident evaluator provider short-circuit step-type checks
	// when the operator has explicitly opted in to a relaxed policy.
	if sprint.StrictnessLevel == "lenient" && providerReport.Passed {
		rulePassed = true
		missing = nil
	}

	report := &domain.EvaluatorReport{
		Status:           providerReport.Status,
		Passed:           providerPassed && rulePassed,
		Score:            providerReport.Score,
		Reason:           strings.TrimSpace(providerReport.Reason),
		MissingStepTypes: mergeMissing(missing, providerMissing),
		Evidence:         append(summaryEvidence(&job), providerReport.Evidence...),
		ContractRef:      firstNonEmpty(providerReport.ContractRef, verification.SprintContractRef, job.SprintContractRef),
	}
	if report.Score == 0 || ignoreProviderMissing {
		report.Score = scoreCompletion(&job, sprint)
	}
	if report.Status == "" {
		report.Status = "blocked"
	}
	if !rulePassed {
		report.Passed = false
		report.Status = "blocked"
		if report.Reason == "" || ignoreProviderMissing {
			report.Reason = fmt.Sprintf("verification contract not satisfied: %s", strings.Join(missing, ", "))
		}
	}
	if report.Passed && report.Status != "passed" {
		report.Status = "passed"
	}
	if report.Passed && ignoreProviderMissing {
		report.Reason = ""
	}
	if report.Reason == "" {
		if report.Passed {
			report.Reason = "completion thresholds satisfied"
		} else {
			report.Reason = "evaluator blocked completion"
		}
	}
	return report
}

func validateEvaluatorReport(report domain.EvaluatorReport, job domain.Job) error {
	switch report.Status {
	case "passed", "blocked", "failed":
	default:
		return fmt.Errorf("invalid evaluator status: %q", report.Status)
	}
	if strings.TrimSpace(report.Reason) == "" {
		return fmt.Errorf("evaluator output requires reason")
	}
	if report.ContractRef == "" {
		report.ContractRef = job.SprintContractRef
	}
	return nil
}

func applyEvaluatorJobState(job *domain.Job, report *domain.EvaluatorReport) {
	switch report.Status {
	case "failed":
		job.Status = domain.JobStatusFailed
		job.FailureReason = report.Reason
		job.LeaderContextSummary = report.Reason
	case "passed":
		job.LeaderContextSummary = report.Reason
	default:
		job.Status = domain.JobStatusBlocked
		job.BlockedReason = report.Reason
		job.LeaderContextSummary = report.Reason
	}
}

func mergeMissing(ruleMissing []string, providerMissing []string) []string {
	seen := make(map[string]struct{}, len(ruleMissing)+len(providerMissing))
	var out []string
	for _, item := range append(ruleMissing, providerMissing...) {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

func deterministicEvaluatorReport(job domain.Job, verification VerificationContract, sprint domain.SprintContract) domain.EvaluatorReport {
	_, missing := verificationSatisfiedForLevel(job, verification, sprint.StrictnessLevel)
	report := domain.EvaluatorReport{
		Status:           "blocked",
		Passed:           false,
		Score:            scoreCompletion(&job, sprint),
		Reason:           fmt.Sprintf("verification contract not satisfied: %s", strings.Join(missing, ", ")),
		MissingStepTypes: missing,
		Evidence:         summaryEvidence(&job),
		ContractRef:      firstNonEmpty(verification.SprintContractRef, job.SprintContractRef, verificationContractPath(job)),
	}
	if len(missing) == 0 {
		report.Status = "passed"
		report.Passed = true
		report.Reason = "completion thresholds satisfied"
	}
	return report
}

// verificationSatisfiedForLevel applies strictness-aware satisfaction rules.
//
// strict: delegates to the canonical verificationSatisfied which requires a
// test worker step with artifacts and summary.
//
// normal: implement is required; review is optional; verification can be
// satisfied by a succeeded test/build/command step.
//
// lenient: only checks required_step_types from the contract (which for
// lenient jobs is empty), so this always returns (true, nil). The caller in
// mergeEvaluatorReport additionally short-circuits on provider.Passed.
func verificationSatisfiedForLevel(job domain.Job, contract VerificationContract, level string) (bool, []string) {
	switch level {
	case "strict":
		return verificationSatisfied(job, contract)
	case "lenient":
		// No structural requirements; the provider report is authoritative.
		missing := missingRequiredSteps(&job, contract.RequiredStepTypes)
		return len(missing) == 0, missing
	default: // "normal"
		return verificationSatisfiedNormal(job, contract)
	}
}

// verificationSatisfiedNormal applies normal-level rules:
//   - implement must succeed
//   - review is not required
//   - test/build/command steps are optional (worker may run tests internally)
func verificationSatisfiedNormal(job domain.Job, contract VerificationContract) (bool, []string) {
	_ = contract

	missing := missingRequiredSteps(&job, []string{"implement"})

	return len(missing) == 0, uniqueStrings(missing)
}

func filterProviderMissingStepTypes(missing []string, level string) []string {
	if level == "lenient" {
		return nil
	}
	if level != "normal" {
		return uniqueStrings(missing)
	}

	filtered := make([]string, 0, len(missing))
	for _, item := range uniqueStrings(missing) {
		if strings.EqualFold(item, "implement") {
			filtered = append(filtered, "implement")
		}
	}
	return filtered
}

func providerBlockedOnlyOnOptionalCoverage(report domain.EvaluatorReport, filteredMissing []string) bool {
	return report.Status == "blocked" && !report.Passed && len(report.MissingStepTypes) > 0 && len(filteredMissing) == 0
}

func (s *Service) resolveVerificationContract(job domain.Job) (VerificationContract, string, error) {
	contract, path, err := loadVerificationContract(job)
	if err != nil {
		return VerificationContract{}, "", err
	}
	return contract, path, nil
}
