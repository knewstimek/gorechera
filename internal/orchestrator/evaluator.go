package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gorchera/internal/domain"
)

func (s *Service) evaluateCompletion(ctx context.Context, job *domain.Job) (*domain.EvaluatorReport, error) {
	verification, verificationPath, err := s.resolveVerificationContract(*job)
	if err != nil {
		verification = buildVerificationContract(*job, buildPlanningArtifact(*job, nil), buildSprintContract(*job, buildPlanningArtifact(*job, nil)), job.PlanningArtifacts)
		verificationPath = verificationContractPath(*job)
	}
	sprint := buildSprintContract(*job, buildPlanningArtifact(*job, nil))

	// Run mechanical automated checks before calling the evaluator LLM.
	// Results are stored on the job so that buildCompactEvaluatorPayload can
	// include them in the evaluator prompt, providing deterministic evidence.
	if job.VerificationContract != nil && len(job.VerificationContract.AutomatedChecks) > 0 {
		job.PreCheckResults = runAutomatedChecks(
			firstNonEmpty(job.WorkspaceDir, s.workspaceRoot),
			job.VerificationContract.AutomatedChecks,
			job.Steps,
		)
	}

	providerReport, err := s.runEvaluatorPhase(ctx, job, verification, verificationPath)
	if err != nil {
		if isShutdownInterruption(ctx, err) {
			if interruptErr := s.interruptJob(context.Background(), job, "orchestrator shutdown interrupted the evaluator phase"); interruptErr != nil {
				return nil, interruptErr
			}
			return &domain.EvaluatorReport{
				Status:      "blocked",
				Passed:      false,
				Score:       0,
				Reason:      job.BlockedReason,
				ContractRef: job.SprintContractRef,
			}, nil
		}
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
	s.accumulateTokenUsage(job, phaseJob.CurrentStep, estimateProviderUsage(phaseJob, domain.RoleEvaluator, raw, phaseJob))

	// schemaRetry: retry up to schemaRetryMax times when JSON/schema
	// validation fails. The hint is injected into phaseJob so the prompt
	// includes a correction directive on the next call.
	var out domain.EvaluatorReport
	for schemaAttempt := 0; ; schemaAttempt++ {
		parseErr := json.Unmarshal([]byte(raw), &out)
		if parseErr == nil {
			parseErr = validateEvaluatorReport(out, *job)
		}
		if parseErr == nil {
			job.SchemaRetryHint = ""
			return out, nil
		}
		if schemaAttempt >= schemaRetryMax {
			job.SchemaRetryHint = ""
			return domain.EvaluatorReport{}, fmt.Errorf("evaluator schema validation failed after %d attempts: %w", schemaRetryMax+1, parseErr)
		}
		hint := parseErr.Error()
		s.addEvent(job, "schema_retry", fmt.Sprintf("evaluator schema retry %d/%d: %s", schemaAttempt+1, schemaRetryMax, hint))
		job.SchemaRetryHint = hint
		phaseJob.SchemaRetryHint = hint
		raw, err = s.sessions.RunEvaluator(ctx, phaseJob)
		if err != nil {
			job.SchemaRetryHint = ""
			return domain.EvaluatorReport{}, fmt.Errorf("evaluator schema retry failed: %w", err)
		}
		s.accumulateTokenUsage(job, phaseJob.CurrentStep, estimateProviderUsage(phaseJob, domain.RoleEvaluator, raw, phaseJob))
	}
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

// evaluatorTextContradicts returns true when the evaluator reason explicitly
// signals a gate failure while passed=true. Detects cases where the structured
// JSON output disagrees with the evaluator's own prose explanation, which can
// happen when a provider emits passed=true but explains a failing gate in text.
func evaluatorTextContradicts(report domain.EvaluatorReport) bool {
	if !report.Passed {
		return false // no contradiction when already not passing
	}
	reason := strings.ToLower(strings.TrimSpace(report.Reason))
	// Narrow set of phrases that unambiguously signal a failed gate.
	// Broad phrases are intentionally excluded to avoid false positives on
	// evaluators that quote failure conditions while explaining why they pass.
	// "not satisfied" alone is excluded -- it can appear in legitimate passing
	// explanations (e.g. "concern was not satisfied by any attacker vector").
	// Use the more specific compound forms instead.
	failurePhrases := []string{
		"gate failure",
		"not a pass",
		"requirements not satisfied",
		"contract not satisfied",
		"did not pass",
		"does not pass",
		"cannot pass",
		"not passing",
		"fails the gate",
	}
	for _, phrase := range failurePhrases {
		if strings.Contains(reason, phrase) {
			return true
		}
	}
	return false
}

func mergeEvaluatorReport(job domain.Job, verification VerificationContract, sprint domain.SprintContract, providerReport domain.EvaluatorReport) *domain.EvaluatorReport {
	// Detect contradiction before any merge modifies the original text.
	// Applied as a final override at the end so it cannot be undone by
	// rule-based verification passing (rulePassed does not override a
	// contradiction between the evaluator's JSON and prose).
	contradicted := evaluatorTextContradicts(providerReport)

	verificationPassed, verificationMissing := verificationSatisfiedForPipeline(job, verification, sprint)
	rulePassed := verificationPassed && len(successfulStepTypes(&job)) >= sprint.ThresholdSuccessCnt
	missing := verificationMissing
	providerMissing := filterProviderMissingStepTypes(providerReport.MissingStepTypes, sprint.RequiredStepTypes)
	ignoreProviderMissing := providerBlockedOnlyOnOptionalCoverage(providerReport, providerMissing)
	providerPassed := providerReport.Passed || ignoreProviderMissing
	if rulePassed {
		providerPassed = true
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

	// Final consistency gate: if the evaluator's own text explicitly signals a
	// gate failure while the merged result says passed, demote. This fires after
	// all rule-based and rubric logic so it is always the last word.
	// Rationale: the evaluator may emit passed=true in JSON but explain a
	// concrete gate failure in prose; the prose is authoritative in that case.
	if contradicted && report.Passed {
		report.Passed = false
		if report.Status == "passed" {
			report.Status = "failed"
		}
		report.Reason = "[consistency] " + report.Reason
	}

	// Apply rubric axis threshold enforcement when axes are defined.
	// This is additive: existing pass/fail logic runs first; rubric can only
	// demote a passing report, never promote a failing one.
	if len(verification.RubricAxes) > 0 && len(providerReport.RubricScores) > 0 {
		thresholds := make(map[string]float64, len(verification.RubricAxes))
		for _, axis := range verification.RubricAxes {
			thresholds[axis.Name] = axis.MinThreshold
		}
		scored := make([]domain.RubricScore, 0, len(providerReport.RubricScores))
		failedAxes := make([]string, 0)
		for _, rs := range providerReport.RubricScores {
			threshold, ok := thresholds[rs.Axis]
			axisPassed := !ok || rs.Score >= threshold
			scored = append(scored, domain.RubricScore{
				Axis:   rs.Axis,
				Score:  rs.Score,
				Passed: axisPassed,
			})
			if !axisPassed {
				failedAxes = append(failedAxes, fmt.Sprintf("%s (%.2f < %.2f)", rs.Axis, rs.Score, threshold))
			}
		}
		report.RubricScores = scored
		if len(failedAxes) > 0 {
			report.Passed = false
			report.Status = "failed"
			report.Reason = fmt.Sprintf("rubric axis thresholds not met: %s", strings.Join(failedAxes, ", "))
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
	_, missing := verificationSatisfiedForPipeline(job, verification, sprint)
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

func verificationSatisfiedForPipeline(job domain.Job, contract VerificationContract, sprint domain.SprintContract) (bool, []string) {
	missing := append([]string(nil), missingRequiredSteps(&job, sprint.RequiredStepTypes)...)
	if ok, engineMissing := engineVerificationSatisfied(latestImplementStep(&job)); !ok {
		missing = append(missing, engineMissing...)
	}
	return len(missing) == 0, uniqueStrings(missing)
}

func verificationSatisfiedNormal(job domain.Job, contract VerificationContract) (bool, []string) {
	return verificationSatisfiedForPipeline(job, contract, domain.SprintContract{
		RequiredStepTypes:   []string{"implement"},
		ThresholdSuccessCnt: 1,
	})
}

func filterProviderMissingStepTypes(missing []string, required []string) []string {
	requiredSet := make(map[string]struct{}, len(required))
	for _, item := range required {
		requiredSet[strings.ToLower(strings.TrimSpace(item))] = struct{}{}
	}
	filtered := make([]string, 0, len(missing))
	for _, item := range uniqueStrings(missing) {
		if _, ok := requiredSet[strings.ToLower(strings.TrimSpace(item))]; ok {
			filtered = append(filtered, item)
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
