package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"gorchera/internal/domain"
	"gorchera/internal/provider"
)

func (s *Service) ensurePlanning(ctx context.Context, job *domain.Job) error {
	if len(job.PlanningArtifacts) > 0 && strings.TrimSpace(job.SprintContractRef) != "" {
		return nil
	}

	plannerOutput, err := s.runPlannerPhase(ctx, job)
	if err != nil {
		if isShutdownInterruption(ctx, err) {
			return s.interruptJob(context.Background(), job, "orchestrator shutdown interrupted the planner phase")
		}
		if unsupportedPhase(err) {
			return s.persistPlanning(ctx, job, buildPlanningArtifact(*job, nil))
		}
		_, failErr := s.failJob(ctx, job, fmt.Sprintf("planner execution failed: %v", err))
		return failErr
	}

	// If the job uses adaptive decomposition, adopt the planner's recommendations
	// before building the sprint contract so the resolved level is used throughout.
	if job.StrictnessLevel == "auto" {
		switch plannerOutput.RecommendedStrictness {
		case "strict", "normal", "lenient":
			job.StrictnessLevel = plannerOutput.RecommendedStrictness
		default:
			// Empty or unrecognised recommendation: fall back to normal.
			job.StrictnessLevel = "normal"
		}
		if plannerOutput.RecommendedMaxSteps > 0 {
			job.MaxSteps = plannerOutput.RecommendedMaxSteps
		}
	}

	planning := buildPlanningArtifact(*job, &plannerOutput)
	return s.persistPlanning(ctx, job, planning)
}

func (s *Service) persistPlanning(ctx context.Context, job *domain.Job, planning domain.PlanningArtifact) error {
	specPath, err := s.artifacts.MaterializeTextArtifact(job.ID, "product_spec.md", planningMarkdown(planning))
	if err != nil {
		return err
	}
	planPath, err := s.artifacts.MaterializeJSONArtifact(job.ID, "execution_plan.json", planning)
	if err != nil {
		return err
	}
	contract := buildSprintContract(*job, planning)
	contractPath, err := s.artifacts.MaterializeJSONArtifact(job.ID, "sprint_contract.json", contract)
	if err != nil {
		return err
	}
	job.SprintContractRef = contractPath
	verification := buildVerificationContract(*job, planning, contract, []string{specPath, planPath, contractPath})
	verificationPath, err := s.artifacts.MaterializeJSONArtifact(job.ID, "verification_contract.json", verification)
	if err != nil {
		return err
	}

	job.VerificationContract = buildPersistedVerificationContract(*job, planning, contract, verification, verificationPath)
	job.VerificationContractRef = verificationPath
	job.PlanningArtifacts = []string{specPath, planPath, contractPath, verificationPath}
	job.Summary = planning.Summary
	job.LeaderContextSummary = planning.Summary
	s.addEvent(job, "job_planned", fmt.Sprintf("planned %d artifacts", len(job.PlanningArtifacts)))
	s.touch(job)
	return s.state.SaveJob(ctx, job)
}

func (s *Service) runPlannerPhase(ctx context.Context, job *domain.Job) (domain.PlanningArtifact, error) {
	phaseJob := *job
	raw, err := s.sessions.RunPlanner(ctx, phaseJob)
	if err != nil {
		return domain.PlanningArtifact{}, err
	}
	s.accumulateTokenUsage(job, phaseJob.CurrentStep, estimateProviderUsage(phaseJob, domain.RolePlanner, raw, phaseJob))
	var out domain.PlanningArtifact
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return domain.PlanningArtifact{}, err
	}
	if err := validatePlanningArtifact(&out, *job); err != nil {
		return domain.PlanningArtifact{}, err
	}
	return out, nil
}

func buildPlanningArtifact(job domain.Job, seed *domain.PlanningArtifact) domain.PlanningArtifact {
	var productScope []string
	if seed != nil && len(seed.ProductScope) > 0 {
		productScope = append([]string(nil), seed.ProductScope...)
	}
	var nonGoals []string
	if seed != nil && len(seed.NonGoals) > 0 {
		nonGoals = append([]string(nil), seed.NonGoals...)
	}
	var proposedSteps []string
	if seed != nil && len(seed.ProposedSteps) > 0 {
		proposedSteps = append([]string(nil), seed.ProposedSteps...)
	}
	acceptance := append([]string(nil), job.DoneCriteria...)
	var successSignals []string
	if seed != nil && len(seed.SuccessSignals) > 0 {
		successSignals = append([]string(nil), seed.SuccessSignals...)
	}
	return domain.PlanningArtifact{
		Goal:                 firstNonEmptyValue(seed, func(p *domain.PlanningArtifact) string { return p.Goal }, job.Goal),
		TechStack:            firstNonEmptyValue(seed, func(p *domain.PlanningArtifact) string { return p.TechStack }, job.TechStack),
		WorkspaceDir:         firstNonEmptyValue(seed, func(p *domain.PlanningArtifact) string { return p.WorkspaceDir }, job.WorkspaceDir),
		Summary:              planningSummary(job, seed),
		ProductScope:         productScope,
		NonGoals:             nonGoals,
		ProposedSteps:        proposedSteps,
		Acceptance:           acceptance,
		SuccessSignals:       successSignals,
		VerificationContract: cloneVerificationContract(seed),
	}
}

func planningSummary(job domain.Job, seed *domain.PlanningArtifact) string {
	if seed != nil {
		if seed.Summary != "" {
			return seed.Summary
		}
	}
	return fmt.Sprintf("Plan for %s", job.Goal)
}

func planningMarkdown(plan domain.PlanningArtifact) string {
	var b strings.Builder
	b.WriteString("# Product Spec\n\n")
	b.WriteString(fmt.Sprintf("Goal: %s\n\n", plan.Goal))
	b.WriteString("## Scope\n")
	for _, item := range plan.ProductScope {
		b.WriteString("- ")
		b.WriteString(item)
		b.WriteString("\n")
	}
	b.WriteString("\n## Non-Goals\n")
	for _, item := range plan.NonGoals {
		b.WriteString("- ")
		b.WriteString(item)
		b.WriteString("\n")
	}
	b.WriteString("\n## Proposed Steps\n")
	for _, item := range plan.ProposedSteps {
		b.WriteString("- ")
		b.WriteString(item)
		b.WriteString("\n")
	}
	b.WriteString("\n## Acceptance\n")
	for _, item := range plan.Acceptance {
		b.WriteString("- ")
		b.WriteString(item)
		b.WriteString("\n")
	}
	return b.String()
}

func buildSprintContract(job domain.Job, planning domain.PlanningArtifact) domain.SprintContract {
	level := normalizeStrictnessLevel(job.StrictnessLevel)

	// required step types vary by strictness:
	// - strict: implement + review + test all required as worker steps
	// - normal: implement required; review optional; validation can be a
	//   succeeded test/build/command step
	// - lenient: no mandatory step type; provider report or system command exit 0 is enough
	var required []string
	thresholdSuccessCnt := 0
	thresholdMinSteps := 0
	switch level {
	case "strict":
		required = []string{"implement", "review", "test"}
		if hasSystemIntent(job) {
			required = append(required, "search")
		}
		thresholdSuccessCnt = len(required)
		thresholdMinSteps = len(required)
	case "normal":
		required = []string{"implement"}
		thresholdSuccessCnt = 1
		thresholdMinSteps = 1
	case "lenient":
		required = []string{}
	case "auto":
		// "auto" should be resolved in ensurePlanning before reaching here.
		// If it reaches here (e.g. planner phase skipped), fall back to "normal".
		level = "normal"
		required = []string{"implement"}
		thresholdSuccessCnt = 1
		thresholdMinSteps = 1
	}

	return domain.SprintContract{
		Version:              1,
		Goal:                 job.Goal,
		RequiredStepTypes:    required,
		AcceptanceCriteria:   append([]string(nil), planning.Acceptance...),
		BlockingCriteria:     []string{"missing required step coverage", "thresholds not satisfied"},
		ThresholdSuccessCnt:  thresholdSuccessCnt,
		ThresholdMinSteps:    thresholdMinSteps,
		ThresholdRequireEval: true,
		StrictnessLevel:      level,
	}
}

// normalizeStrictnessLevel canonicalises the level string and falls back to
// "normal" for empty or unrecognised values.
// "auto" is passed through so ensurePlanning can adopt the planner's recommendation.
func normalizeStrictnessLevel(level string) string {
	switch strings.TrimSpace(strings.ToLower(level)) {
	case "strict":
		return "strict"
	case "lenient":
		return "lenient"
	case "auto":
		return "auto"
	default:
		return "normal"
	}
}

func normalizeContextMode(mode string) string {
	switch strings.TrimSpace(strings.ToLower(mode)) {
	case "summary":
		return "summary"
	case "minimal":
		return "minimal"
	case "auto":
		// Pass through so the leader payload builder can resolve it at runtime.
		return "auto"
	default:
		return "full"
	}
}

func hasSystemIntent(job domain.Job) bool {
	for _, criterion := range job.DoneCriteria {
		if strings.Contains(strings.ToLower(criterion), "system") {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func firstNonEmptyValue(seed *domain.PlanningArtifact, extract func(*domain.PlanningArtifact) string, fallback string) string {
	if seed != nil {
		if value := strings.TrimSpace(extract(seed)); value != "" {
			return value
		}
	}
	return fallback
}

func cloneVerificationContract(seed *domain.PlanningArtifact) *domain.VerificationContract {
	if seed == nil || seed.VerificationContract == nil {
		return nil
	}
	contract := *seed.VerificationContract
	contract.Scope = append([]string(nil), contract.Scope...)
	contract.RequiredCommands = append([]string(nil), contract.RequiredCommands...)
	contract.RequiredArtifacts = append([]string(nil), contract.RequiredArtifacts...)
	contract.RequiredChecks = append([]string(nil), contract.RequiredChecks...)
	contract.DisallowedActions = append([]string(nil), contract.DisallowedActions...)
	return &contract
}

func validatePlanningArtifact(plan *domain.PlanningArtifact, job domain.Job) error {
	if strings.TrimSpace(plan.Goal) == "" {
		return fmt.Errorf("planner output requires goal")
	}
	if strings.TrimSpace(plan.Summary) == "" {
		return fmt.Errorf("planner output requires summary")
	}
	// Populate Acceptance from job criteria when the planner omits it,
	// so the sprint contract has criteria to enforce. Without pointer
	// semantics this assignment was silently discarded.
	if len(plan.Acceptance) == 0 {
		plan.Acceptance = job.DoneCriteria
	}
	return nil
}

func unsupportedPhase(err error) bool {
	var perr *provider.ProviderError
	return errors.As(err, &perr) && perr.Kind == provider.ErrorKindUnsupportedPhase
}
