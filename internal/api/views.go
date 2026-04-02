package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gorchera/internal/domain"
)

type ArtifactView struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Kind    string `json:"kind,omitempty"`
	Content any    `json:"content,omitempty"`
	Error   string `json:"error,omitempty"`
}

type ParallelPolicyView struct {
	MaxParallelWorkers int      `json:"max_parallel_workers"`
	ApprovalAuthority  string   `json:"approval_authority"`
	ScopeRequirement   string   `json:"scope_requirement"`
	ContextPolicy      []string `json:"context_policy,omitempty"`
	Note               string   `json:"note,omitempty"`
}

type PlanningView struct {
	JobID             string              `json:"job_id"`
	Goal              string              `json:"goal"`
	TechStack         string              `json:"tech_stack,omitempty"`
	WorkspaceDir      string              `json:"workspace_dir,omitempty"`
	Provider          domain.ProviderName `json:"provider"`
	SprintContractRef string              `json:"sprint_contract_ref,omitempty"`
	PlanningArtifacts []string            `json:"planning_artifact_refs,omitempty"`
	Artifacts         []ArtifactView      `json:"artifacts,omitempty"`
	ParallelPolicy    ParallelPolicyView  `json:"parallel_policy"`
}

type EvaluatorView struct {
	JobID     string                  `json:"job_id"`
	Provider  domain.ProviderName     `json:"provider"`
	ReportRef string                  `json:"report_ref,omitempty"`
	Report    *domain.EvaluatorReport `json:"report,omitempty"`
	Error     string                  `json:"error,omitempty"`
}

type VerificationView struct {
	JobID                   string                       `json:"job_id"`
	Goal                    string                       `json:"goal"`
	Provider                domain.ProviderName          `json:"provider"`
	SprintContractRef       string                       `json:"sprint_contract_ref,omitempty"`
	SprintContract          *domain.SprintContract       `json:"sprint_contract,omitempty"`
	VerificationContractRef string                       `json:"verification_contract_ref,omitempty"`
	VerificationContract    *domain.VerificationContract `json:"verification_contract,omitempty"`
	EvaluatorReportRef      string                       `json:"evaluator_report_ref,omitempty"`
	EvaluatorReport         *domain.EvaluatorReport      `json:"evaluator_report,omitempty"`
	RoleProfiles            *domain.RoleProfiles         `json:"role_profiles,omitempty"`
	DerivedChecks           []string                     `json:"derived_checks,omitempty"`
	ParallelPolicy          ParallelPolicyView           `json:"parallel_policy"`
	Note                    string                       `json:"note,omitempty"`
}

type ProfileView struct {
	JobID                 string               `json:"job_id"`
	Provider              domain.ProviderName  `json:"provider"`
	RoleProfilesAvailable bool                 `json:"role_profiles_available"`
	RoleProfiles          *domain.RoleProfiles `json:"role_profiles,omitempty"`
	ParallelPolicy        ParallelPolicyView   `json:"parallel_policy"`
	Note                  string               `json:"note,omitempty"`
}

func defaultParallelPolicyView() ParallelPolicyView {
	return ParallelPolicyView{
		MaxParallelWorkers: 2,
		ApprovalAuthority:  "leader/orchestrator",
		ScopeRequirement:   "disjoint write scope required",
		ContextPolicy: []string{
			"planner may propose parallel candidates",
			"leader authorizes worker fan-out",
			"executor cannot spawn workers on its own",
			"context must stay artifact-scoped and minimal",
		},
		Note: "runtime fan-out is implemented and enforced by the orchestrator",
	}
}

func BuildPlanningView(job *domain.Job) PlanningView {
	view := PlanningView{
		JobID:             job.ID,
		Goal:              job.Goal,
		TechStack:         job.TechStack,
		WorkspaceDir:      job.WorkspaceDir,
		Provider:          job.Provider,
		SprintContractRef: job.SprintContractRef,
		PlanningArtifacts: append([]string(nil), job.PlanningArtifacts...),
		ParallelPolicy:    defaultParallelPolicyView(),
	}

	seen := make(map[string]struct{})
	planningRoot := jobArtifactsRoot(job)
	for _, path := range append(append([]string(nil), job.PlanningArtifacts...), job.SprintContractRef) {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		view.Artifacts = append(view.Artifacts, loadArtifactView(planningRoot, path))
	}
	return view
}

func BuildEvaluatorView(job *domain.Job) EvaluatorView {
	view := EvaluatorView{
		JobID:     job.ID,
		Provider:  job.Provider,
		ReportRef: job.EvaluatorReportRef,
	}
	if strings.TrimSpace(job.EvaluatorReportRef) == "" {
		view.Error = "evaluator report is not available"
		return view
	}

	data, err := safeReadFile(jobArtifactsRoot(job), job.EvaluatorReportRef)
	if err != nil {
		view.Error = err.Error()
		return view
	}
	var report domain.EvaluatorReport
	if err := json.Unmarshal(data, &report); err != nil {
		view.Error = err.Error()
		return view
	}
	view.Report = &report
	return view
}

func BuildVerificationView(job *domain.Job) VerificationView {
	view := VerificationView{
		JobID:                   job.ID,
		Goal:                    job.Goal,
		Provider:                job.Provider,
		SprintContractRef:       job.SprintContractRef,
		VerificationContractRef: job.VerificationContractRef,
		EvaluatorReportRef:      job.EvaluatorReportRef,
		RoleProfiles:            &job.RoleProfiles,
		ParallelPolicy:          defaultParallelPolicyView(),
		Note:                    "verification is a read-only contract derived from the sprint contract, evaluator report, and role profiles",
	}

	root := jobArtifactsRoot(job)
	if sprint := loadSprintContract(root, job.SprintContractRef); sprint != nil {
		view.SprintContract = sprint
		view.DerivedChecks = append(view.DerivedChecks, sprint.RequiredStepTypes...)
		view.DerivedChecks = append(view.DerivedChecks, sprint.AcceptanceCriteria...)
		if view.VerificationContractRef == "" {
			view.VerificationContractRef = job.SprintContractRef
		}
	}

	if report := loadEvaluatorReport(root, job.EvaluatorReportRef); report != nil {
		view.EvaluatorReport = report
		view.DerivedChecks = append(view.DerivedChecks, report.Evidence...)
		if len(report.MissingStepTypes) > 0 {
			view.DerivedChecks = append(view.DerivedChecks, "missing:"+strings.Join(report.MissingStepTypes, ","))
		}
	}

	if job.VerificationContract != nil {
		contract := *job.VerificationContract
		view.VerificationContract = &contract
	} else {
		contract := deriveVerificationContract(job, view.SprintContract, view.EvaluatorReport)
		view.VerificationContract = &contract
	}

	return view
}

func BuildProfileView(job *domain.Job) ProfileView {
	return ProfileView{
		JobID:                 job.ID,
		Provider:              job.Provider,
		RoleProfilesAvailable: true,
		RoleProfiles:          &job.RoleProfiles,
		ParallelPolicy:        defaultParallelPolicyView(),
		Note:                  "leader and worker routing use these persisted profiles; planner and evaluator are also provider-backed phases now",
	}
}

func loadSprintContract(root, path string) *domain.SprintContract {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	data, err := safeReadFile(root, path)
	if err != nil {
		return nil
	}
	var contract domain.SprintContract
	if err := json.Unmarshal(data, &contract); err != nil {
		return nil
	}
	return &contract
}

func loadEvaluatorReport(root, path string) *domain.EvaluatorReport {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	data, err := safeReadFile(root, path)
	if err != nil {
		return nil
	}
	var report domain.EvaluatorReport
	if err := json.Unmarshal(data, &report); err != nil {
		return nil
	}
	return &report
}

func deriveVerificationContract(job *domain.Job, sprint *domain.SprintContract, report *domain.EvaluatorReport) domain.VerificationContract {
	scope := append([]string(nil), job.DoneCriteria...)
	requiredChecks := []string{
		"planner_artifacts_present",
		"tester_executes_required_steps",
		"evaluator_gate_passes",
	}
	requiredArtifacts := []string{}
	if strings.TrimSpace(job.SprintContractRef) != "" {
		requiredArtifacts = append(requiredArtifacts, job.SprintContractRef)
	}
	if strings.TrimSpace(job.EvaluatorReportRef) != "" {
		requiredArtifacts = append(requiredArtifacts, job.EvaluatorReportRef)
	}
	if sprint != nil {
		scope = append(scope, sprint.AcceptanceCriteria...)
		requiredChecks = append(requiredChecks, sprint.RequiredStepTypes...)
	}
	if report != nil && len(report.MissingStepTypes) > 0 {
		requiredChecks = append(requiredChecks, "missing:"+strings.Join(report.MissingStepTypes, ","))
	}
	return domain.VerificationContract{
		Version:           1,
		Goal:              job.Goal,
		Scope:             dedupeStrings(scope),
		RequiredCommands:  append([]string(nil), requiredChecks...),
		RequiredArtifacts: dedupeStrings(requiredArtifacts),
		RequiredChecks:    dedupeStrings(requiredChecks),
		DisallowedActions: []string{"self-approval", "unbounded parallel fan-out"},
		MaxSeconds:        0,
		Notes:             "derived from sprint contract and evaluator evidence",
		OwnerRole:         domain.RoleEvaluator,
	}
}

func dedupeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

// safeReadFile reads path only if it is within allowedRoot, preventing path traversal.
// Both allowedRoot and path are resolved to absolute paths before comparison.
func safeReadFile(allowedRoot, path string) ([]byte, error) {
	absRoot, err := filepath.Abs(allowedRoot)
	if err != nil {
		return nil, fmt.Errorf("resolving root %q: %w", allowedRoot, err)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolving path %q: %w", path, err)
	}
	cleanRoot := filepath.Clean(absRoot)
	cleanPath := filepath.Clean(absPath)
	// Accept the root itself or any path strictly under root (with separator boundary).
	if cleanPath != cleanRoot && !strings.HasPrefix(cleanPath, cleanRoot+string(filepath.Separator)) {
		return nil, fmt.Errorf("path %q is outside allowed root %q", path, allowedRoot)
	}
	return os.ReadFile(cleanPath)
}

// jobArtifactsRoot returns the allowed root directory for artifact reads for a given job.
// Falls back to the current directory when WorkspaceDir is unset.
func jobArtifactsRoot(job *domain.Job) string {
	if job.WorkspaceDir != "" {
		return job.WorkspaceDir
	}
	return "."
}

func loadArtifactView(root, path string) ArtifactView {
	view := ArtifactView{
		Name: filepath.Base(path),
		Path: path,
		Kind: artifactKind(path),
	}

	data, err := safeReadFile(root, path)
	if err != nil {
		view.Error = err.Error()
		return view
	}

	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		var decoded any
		if err := json.Unmarshal(data, &decoded); err == nil {
			view.Content = decoded
			return view
		} else {
			view.Content = string(data)
			view.Error = err.Error()
			return view
		}
	default:
		view.Content = string(data)
		return view
	}
}

func artifactKind(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		return "json"
	case ".md":
		return "markdown"
	default:
		return "text"
	}
}
