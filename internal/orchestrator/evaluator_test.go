package orchestrator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gorchera/internal/domain"
)

func TestVerificationSatisfiedNormalRequiresEngineEvidence(t *testing.T) {
	t.Parallel()

	// A step with no artifacts at all has no engine_build/engine_test paths,
	// so it is treated as a legacy job and engine verification is skipped.
	// Verification should pass (legacy compat -- see C1 fix).
	job := domain.Job{
		Steps: []domain.Step{
			{TaskType: "implement", Status: domain.StepStatusSucceeded},
		},
	}
	contract := VerificationContract{
		RequiredStepTypes: []string{"implement"},
	}

	passed, missing := verificationSatisfiedNormal(job, contract)
	if !passed {
		t.Fatalf("expected legacy job (no engine artifacts) to pass verification, missing=%v", missing)
	}
}

func TestVerificationSatisfiedNormalRequiresEngineEvidenceWhenPathPresent(t *testing.T) {
	t.Parallel()

	// A step that has an engine_build artifact path but the file does not exist
	// (or is unreadable) must NOT be treated as a legacy job -- the path signals
	// that engine verification was attempted. Verification should fail.
	job := domain.Job{
		Steps: []domain.Step{
			{
				TaskType: "implement",
				Status:   domain.StepStatusSucceeded,
				// Non-existent path, but the filename contains "engine_build"
				// so loadEngineCheckArtifacts will try (and fail) to read it.
				Artifacts: []string{"/nonexistent/engine_build_0.json"},
			},
		},
	}
	contract := VerificationContract{
		RequiredStepTypes: []string{"implement"},
	}

	passed, missing := verificationSatisfiedNormal(job, contract)
	if passed {
		t.Fatalf("expected verification to fail when engine artifact path is present but unreadable, missing=%v", missing)
	}
	if len(missing) == 0 {
		t.Fatal("expected missing engine verification coverage")
	}
}

func TestVerificationSatisfiedNormalAcceptsSkippedEngineChecks(t *testing.T) {
	t.Parallel()

	artifacts := writeEngineArtifactsForTest(t, engineCheckSkipped, engineCheckSkipped)
	job := domain.Job{
		Steps: []domain.Step{
			{TaskType: "implement", Status: domain.StepStatusSucceeded, Artifacts: artifacts},
		},
	}

	passed, missing := verificationSatisfiedNormal(job, VerificationContract{RequiredStepTypes: []string{"implement"}})
	if !passed {
		t.Fatalf("expected skipped engine checks to satisfy coverage, missing=%v", missing)
	}
}

func TestMergeEvaluatorReportNormalIgnoresOptionalProviderMissing(t *testing.T) {
	t.Parallel()

	job := domain.Job{
		Steps: []domain.Step{
			{Target: "B", TaskType: "implement", Status: domain.StepStatusSucceeded, Artifacts: writeEngineArtifactsForTest(t, engineCheckPassed, engineCheckPassed)},
		},
	}
	verification := VerificationContract{
		RequiredStepTypes: []string{"implement"},
	}
	sprint := domain.SprintContract{
		RequiredStepTypes:   []string{"implement"},
		ThresholdSuccessCnt: 1,
		ThresholdMinSteps:   1,
		StrictnessLevel:     "normal",
	}
	providerReport := domain.EvaluatorReport{
		Status:           "blocked",
		Passed:           false,
		Score:            67,
		Reason:           "missing required step coverage: review, test",
		MissingStepTypes: []string{"review", "test"},
	}

	report := mergeEvaluatorReport(job, verification, sprint, providerReport)
	if !report.Passed {
		t.Fatalf("expected merged report to pass, got %#v", report)
	}
	if report.Status != "passed" {
		t.Fatalf("expected passed status, got %q", report.Status)
	}
	if report.Score != 100 {
		t.Fatalf("expected score 100, got %d", report.Score)
	}
	if len(report.MissingStepTypes) != 0 {
		t.Fatalf("expected optional provider missing types to be ignored, got %v", report.MissingStepTypes)
	}
}

func TestMergeEvaluatorReportRubricAllPass(t *testing.T) {
	t.Parallel()

	job := domain.Job{
		Steps: []domain.Step{
			{Target: "B", TaskType: "implement", Status: domain.StepStatusSucceeded, Artifacts: writeEngineArtifactsForTest(t, engineCheckPassed, engineCheckPassed)},
		},
	}
	verification := VerificationContract{
		RequiredStepTypes: []string{"implement"},
		RubricAxes: []domain.RubricAxis{
			{Name: "functionality", Weight: 0.6, MinThreshold: 0.7},
			{Name: "code_quality", Weight: 0.4, MinThreshold: 0.6},
		},
	}
	sprint := domain.SprintContract{
		RequiredStepTypes:   []string{"implement"},
		ThresholdSuccessCnt: 1,
		StrictnessLevel:     "normal",
	}
	providerReport := domain.EvaluatorReport{
		Status: "passed",
		Passed: true,
		Score:  90,
		Reason: "all steps succeeded",
		RubricScores: []domain.RubricScore{
			{Axis: "functionality", Score: 0.9},
			{Axis: "code_quality", Score: 0.8},
		},
	}

	report := mergeEvaluatorReport(job, verification, sprint, providerReport)
	if !report.Passed {
		t.Fatalf("expected rubric all-pass report to pass, got %#v", report)
	}
	if report.Status != "passed" {
		t.Fatalf("expected status passed, got %q", report.Status)
	}
	if len(report.RubricScores) != 2 {
		t.Fatalf("expected 2 rubric scores stored, got %d", len(report.RubricScores))
	}
	for _, rs := range report.RubricScores {
		if !rs.Passed {
			t.Fatalf("expected axis %q to pass, got score %.2f", rs.Axis, rs.Score)
		}
	}
}

func TestMergeEvaluatorReportRubricAxisFail(t *testing.T) {
	t.Parallel()

	job := domain.Job{
		Steps: []domain.Step{
			{Target: "B", TaskType: "implement", Status: domain.StepStatusSucceeded, Artifacts: writeEngineArtifactsForTest(t, engineCheckPassed, engineCheckPassed)},
		},
	}
	verification := VerificationContract{
		RequiredStepTypes: []string{"implement"},
		RubricAxes: []domain.RubricAxis{
			{Name: "functionality", Weight: 0.6, MinThreshold: 0.7},
			{Name: "test_coverage", Weight: 0.4, MinThreshold: 0.8},
		},
	}
	sprint := domain.SprintContract{
		RequiredStepTypes:   []string{"implement"},
		ThresholdSuccessCnt: 1,
		StrictnessLevel:     "normal",
	}
	providerReport := domain.EvaluatorReport{
		Status: "passed",
		Passed: true,
		Score:  85,
		Reason: "implement succeeded",
		RubricScores: []domain.RubricScore{
			{Axis: "functionality", Score: 0.9},
			{Axis: "test_coverage", Score: 0.5},
		},
	}

	report := mergeEvaluatorReport(job, verification, sprint, providerReport)
	if report.Passed {
		t.Fatalf("expected rubric axis fail to demote report, got passed=true")
	}
	if report.Status != "failed" {
		t.Fatalf("expected status failed, got %q", report.Status)
	}
	if len(report.RubricScores) != 2 {
		t.Fatalf("expected 2 rubric scores stored, got %d", len(report.RubricScores))
	}
	failCount := 0
	for _, rs := range report.RubricScores {
		if !rs.Passed {
			failCount++
		}
	}
	if failCount != 1 {
		t.Fatalf("expected exactly 1 failed axis, got %d", failCount)
	}
	if !strings.Contains(report.Reason, "test_coverage") {
		t.Fatalf("expected reason to mention failed axis, got %q", report.Reason)
	}
}

func TestMergeEvaluatorReportNoRubric(t *testing.T) {
	t.Parallel()

	job := domain.Job{
		Steps: []domain.Step{
			{Target: "B", TaskType: "implement", Status: domain.StepStatusSucceeded, Artifacts: writeEngineArtifactsForTest(t, engineCheckPassed, engineCheckPassed)},
		},
	}
	verification := VerificationContract{
		RequiredStepTypes: []string{"implement"},
	}
	sprint := domain.SprintContract{
		RequiredStepTypes:   []string{"implement"},
		ThresholdSuccessCnt: 1,
		StrictnessLevel:     "normal",
	}
	providerReport := domain.EvaluatorReport{
		Status: "passed",
		Passed: true,
		Score:  90,
		Reason: "implement succeeded",
	}

	report := mergeEvaluatorReport(job, verification, sprint, providerReport)
	if !report.Passed {
		t.Fatalf("expected no-rubric report to pass unchanged, got %#v", report)
	}
	if report.Status != "passed" {
		t.Fatalf("expected status passed, got %q", report.Status)
	}
	if len(report.RubricScores) != 0 {
		t.Fatalf("expected no rubric scores when no axes defined, got %v", report.RubricScores)
	}
}

func TestBuildSprintContractBalancedRequiresImplementOnly(t *testing.T) {
	// Reviewer merged into evaluator: balanced pipeline no longer requires a
	// separate review step type. The evaluator performs code verification.
	t.Parallel()

	contract := buildSprintContract(domain.Job{
		Goal:         "balanced pipeline contract",
		PipelineMode: string(domain.PipelineModeBalanced),
	}, domain.PlanningArtifact{})

	if len(contract.RequiredStepTypes) != 1 || contract.RequiredStepTypes[0] != "implement" {
		t.Fatalf("expected only implement to be required, got %v", contract.RequiredStepTypes)
	}
	if contract.ThresholdSuccessCnt != 1 {
		t.Fatalf("expected success threshold 1, got %d", contract.ThresholdSuccessCnt)
	}
}

func TestBuildSprintContractLightRequiresImplementOnly(t *testing.T) {
	t.Parallel()

	contract := buildSprintContract(domain.Job{
		Goal:         "light pipeline contract",
		PipelineMode: string(domain.PipelineModeLight),
	}, domain.PlanningArtifact{})

	if len(contract.RequiredStepTypes) != 1 || contract.RequiredStepTypes[0] != "implement" {
		t.Fatalf("expected only implement to be required, got %v", contract.RequiredStepTypes)
	}
	if contract.ThresholdSuccessCnt != 1 {
		t.Fatalf("expected success threshold 1, got %d", contract.ThresholdSuccessCnt)
	}
}

func TestMergeEvaluatorReportConsistencyCheckDemotesContradictoryPassed(t *testing.T) {
	t.Parallel()

	job := domain.Job{
		Steps: []domain.Step{
			{Target: "B", TaskType: "implement", Status: domain.StepStatusSucceeded, Artifacts: writeEngineArtifactsForTest(t, engineCheckPassed, engineCheckPassed)},
		},
	}
	verification := VerificationContract{RequiredStepTypes: []string{"implement"}}
	sprint := domain.SprintContract{
		RequiredStepTypes:   []string{"implement"},
		ThresholdSuccessCnt: 1,
		StrictnessLevel:     "normal",
	}

	// Provider JSON says passed=true but reason text says "gate failure" -- contradiction.
	providerReport := domain.EvaluatorReport{
		Status: "passed",
		Passed: true,
		Score:  80,
		Reason: "This is a gate failure, not a pass -- required checks are not satisfied.",
	}

	report := mergeEvaluatorReport(job, verification, sprint, providerReport)
	if report.Passed {
		t.Fatalf("expected consistency check to demote contradictory passed=true with failure text, got passed=true, status=%q, reason=%q", report.Status, report.Reason)
	}
	if report.Status == "passed" {
		t.Fatalf("expected non-passing status after consistency check, got %q", report.Status)
	}
}

func TestMergeEvaluatorReportConsistencyCheckPreservesCleanPassedReport(t *testing.T) {
	t.Parallel()

	job := domain.Job{
		Steps: []domain.Step{
			{Target: "B", TaskType: "implement", Status: domain.StepStatusSucceeded, Artifacts: writeEngineArtifactsForTest(t, engineCheckPassed, engineCheckPassed)},
		},
	}
	verification := VerificationContract{RequiredStepTypes: []string{"implement"}}
	sprint := domain.SprintContract{
		RequiredStepTypes:   []string{"implement"},
		ThresholdSuccessCnt: 1,
		StrictnessLevel:     "normal",
	}

	// Clean pass: passed=true with an affirmative reason -- no contradiction.
	providerReport := domain.EvaluatorReport{
		Status: "passed",
		Passed: true,
		Score:  95,
		Reason: "All required implement steps completed. Build and tests passed.",
	}

	report := mergeEvaluatorReport(job, verification, sprint, providerReport)
	if !report.Passed {
		t.Fatalf("expected clean passed report to remain passing, got %#v", report)
	}
	if report.Status != "passed" {
		t.Fatalf("expected status passed, got %q", report.Status)
	}
}

func TestEvaluatorConsistencyPhrasesAllTriggerDemotion(t *testing.T) {
	t.Parallel()

	phrases := []string{
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

	for _, phrase := range phrases {
		phrase := phrase
		t.Run(phrase, func(t *testing.T) {
			t.Parallel()

			report := domain.EvaluatorReport{
				Status: "passed",
				Passed: true,
				Reason: "The implementation has an issue: " + phrase + " for required checks.",
			}
			if !evaluatorTextContradicts(report) {
				t.Fatalf("expected phrase %q to trigger contradiction detection", phrase)
			}
		})
	}
}

func TestEvaluatorConsistencyCheckIgnoresAlreadyFailedReport(t *testing.T) {
	t.Parallel()

	// A report with passed=false should never be considered contradictory
	// (the text may describe failure, which is consistent with passed=false).
	report := domain.EvaluatorReport{
		Status: "failed",
		Passed: false,
		Reason: "gate failure: implement step did not pass.",
	}
	if evaluatorTextContradicts(report) {
		t.Fatal("expected no contradiction when passed=false regardless of reason text")
	}
}

// TestEvaluatorConsistencyNoFalsePositiveOnBorderlinePhrase verifies that
// "not satisfied" in isolation (without a qualifying compound like
// "requirements not satisfied") does NOT trigger contradiction demotion.
// This guards against the false-positive case described in the review where a
// legitimate passing explanation contains the substring "not satisfied" as part
// of a different clause (e.g. "the concern was not satisfied by any attacker").
func TestEvaluatorConsistencyNoFalsePositiveOnBorderlinePhrase(t *testing.T) {
	t.Parallel()

	borderlineCases := []string{
		// "not satisfied" alone -- too broad, must not trigger
		"All checks pass. The previous audit concern was not satisfied by any attacker vector after the fix.",
		"The gate condition was not satisfied before the patch, but is now fully resolved.",
		"Edge case was not satisfied in older versions; current version handles it correctly.",
	}

	for _, reason := range borderlineCases {
		reason := reason
		t.Run(reason[:40], func(t *testing.T) {
			t.Parallel()
			report := domain.EvaluatorReport{
				Status: "passed",
				Passed: true,
				Reason: reason,
			}
			if evaluatorTextContradicts(report) {
				t.Fatalf("false positive: borderline reason should not trigger contradiction: %q", reason)
			}
		})
	}

	// Compound forms must still trigger.
	triggerCases := []string{
		"requirements not satisfied: build step missing",
		"contract not satisfied for this job",
	}
	for _, reason := range triggerCases {
		reason := reason
		t.Run("triggers:"+reason[:20], func(t *testing.T) {
			t.Parallel()
			report := domain.EvaluatorReport{
				Status: "passed",
				Passed: true,
				Reason: reason,
			}
			if !evaluatorTextContradicts(report) {
				t.Fatalf("expected compound phrase to trigger contradiction: %q", reason)
			}
		})
	}
}

func writeEngineArtifactsForTest(t *testing.T, buildStatus, testStatus string) []string {
	t.Helper()

	dir := t.TempDir()
	artifacts := []struct {
		name   string
		record EngineCheckArtifact
	}{
		{
			name: "step-01-engine_build.json",
			record: EngineCheckArtifact{
				Kind:    "build",
				Status:  buildStatus,
				Command: "go build ./...",
				Reason:  "test fixture",
			},
		},
		{
			name: "step-01-engine_test.json",
			record: EngineCheckArtifact{
				Kind:    "test",
				Status:  testStatus,
				Command: "go test ./...",
				Reason:  "test fixture",
			},
		},
	}
	paths := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		path := filepath.Join(dir, artifact.name)
		data, err := json.Marshal(artifact.record)
		if err != nil {
			t.Fatalf("failed to marshal engine artifact: %v", err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("failed to write engine artifact: %v", err)
		}
		paths = append(paths, path)
	}
	return paths
}
