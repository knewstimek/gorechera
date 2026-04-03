package domain

import (
	"strings"
	"time"
)

type JobStatus string

const (
	JobStatusQueued        JobStatus = "queued"
	JobStatusStarting      JobStatus = "starting"
	JobStatusPlanning      JobStatus = "planning"
	JobStatusRunning       JobStatus = "running"
	JobStatusWaitingLeader JobStatus = "waiting_leader"
	JobStatusWaitingWorker JobStatus = "waiting_worker"
	JobStatusBlocked       JobStatus = "blocked"
	JobStatusFailed        JobStatus = "failed"
	JobStatusDone          JobStatus = "done"
)

type WorkspaceMode string

const (
	WorkspaceModeShared   WorkspaceMode = "shared"
	WorkspaceModeIsolated WorkspaceMode = "isolated"
)

type PipelineMode string

const (
	PipelineModeLight    PipelineMode = "light"
	PipelineModeBalanced PipelineMode = "balanced"
	PipelineModeFull     PipelineMode = "full"
)

type StepStatus string

const (
	StepStatusPending   StepStatus = "pending"
	StepStatusActive    StepStatus = "active"
	StepStatusSucceeded StepStatus = "succeeded"
	StepStatusBlocked   StepStatus = "blocked"
	StepStatusFailed    StepStatus = "failed"
	StepStatusSkipped   StepStatus = "skipped"
)

type ProviderName string

const (
	ProviderMock   ProviderName = "mock"
	ProviderCodex  ProviderName = "codex"
	ProviderClaude ProviderName = "claude"
)

type RoleName string

const (
	RoleDirector  RoleName = "director"
	RolePlanner   RoleName = "planner"
	RoleLeader    RoleName = "leader"
	RoleExecutor  RoleName = "executor"
	RoleReviewer  RoleName = "reviewer"
	RoleTester    RoleName = "tester"
	RoleEvaluator RoleName = "evaluator"
)

const (
	AmbitionLevelLow    = "low"
	AmbitionLevelMedium = "medium"
	AmbitionLevelHigh   = "high"
	AmbitionLevelCustom = "custom"
)

type ExecutionProfile struct {
	Provider         ProviderName `json:"provider"`
	Model            string       `json:"model,omitempty"`
	Effort           string       `json:"effort,omitempty"`
	ToolPolicy       string       `json:"tool_policy,omitempty"`
	FallbackProvider ProviderName `json:"fallback_provider,omitempty"`
	FallbackModel    string       `json:"fallback_model,omitempty"`
	MaxBudgetUSD     float64      `json:"max_budget_usd,omitempty"`
}

type RoleOverride struct {
	Provider ProviderName `json:"provider,omitempty"`
	Model    string       `json:"model,omitempty"`
}

type RoleProfiles struct {
	Director  ExecutionProfile `json:"director,omitempty"`
	Planner   ExecutionProfile `json:"planner,omitempty"`
	Leader    ExecutionProfile `json:"leader,omitempty"`
	Executor  ExecutionProfile `json:"executor"`
	Reviewer  ExecutionProfile `json:"reviewer"`
	Tester    ExecutionProfile `json:"tester,omitempty"`
	Evaluator ExecutionProfile `json:"evaluator"`
}

func DefaultRoleProfiles(base ProviderName) RoleProfiles {
	// Heavy reasoning roles (director, evaluator) use opus.
	// Execution roles (executor, reviewer) use sonnet for speed/cost.
	director := ExecutionProfile{Provider: base, Model: "opus"}
	executor := ExecutionProfile{Provider: base, Model: "sonnet"}
	return RoleProfiles{
		Director:  director,
		Planner:   director,
		Leader:    director,
		Executor:  executor,
		Reviewer:  ExecutionProfile{Provider: base, Model: "sonnet"},
		Tester:    executor,
		Evaluator: ExecutionProfile{Provider: base, Model: "opus"},
	}
}

func (r RoleProfiles) Normalize(base ProviderName) RoleProfiles {
	r.Director = firstNonZeroProfile(r.Director, r.Planner, r.Leader).withFallback(base)
	r.Planner = firstNonZeroProfile(r.Planner, r.Director).withFallback(base)
	r.Leader = firstNonZeroProfile(r.Leader, r.Director).withFallback(base)
	r.Executor = r.Executor.withFallback(base)
	r.Reviewer = r.Reviewer.withFallback(base)
	if isZeroExecutionProfile(r.Tester) {
		r.Tester = r.Executor
	} else {
		r.Tester = r.Tester.withFallback(base)
	}
	r.Evaluator = r.Evaluator.withFallback(base)
	return r
}

func (r RoleProfiles) ProfileFor(role RoleName, base ProviderName) ExecutionProfile {
	director := firstNonZeroProfile(r.Director, r.Leader, r.Planner).withFallback(base)
	switch role {
	case RoleDirector:
		return director
	case RolePlanner:
		return firstNonZeroProfile(r.Planner, r.Director, r.Leader).withFallback(base)
	case RoleLeader:
		return firstNonZeroProfile(r.Leader, r.Director, r.Planner).withFallback(base)
	case RoleExecutor:
		return r.Executor.withFallback(base)
	case RoleReviewer:
		return r.Reviewer.withFallback(base)
	case RoleTester:
		return firstNonZeroProfile(r.Tester, r.Executor).withFallback(base)
	case RoleEvaluator:
		return r.Evaluator.withFallback(base)
	default:
		return ExecutionProfile{Provider: base}
	}
}

func (p ExecutionProfile) withFallback(base ProviderName) ExecutionProfile {
	if p.Provider == "" {
		p.Provider = base
	}
	return p
}

func RoleForTaskType(taskType string) RoleName {
	switch strings.ToLower(strings.TrimSpace(taskType)) {
	case "review", "audit":
		// Route to executor so review/audit tasks use executor profile.
		// RoleReviewer is kept for backwards-compatible profile lookup only.
		return RoleExecutor
	case "test":
		return RoleExecutor
	case "implement", "build", "lint", "search", "command":
		return RoleExecutor
	default:
		return RoleExecutor
	}
}

func NormalizePipelineMode(mode string) string {
	switch strings.TrimSpace(strings.ToLower(mode)) {
	case string(PipelineModeLight):
		return string(PipelineModeLight)
	case string(PipelineModeFull):
		return string(PipelineModeFull)
	default:
		return string(PipelineModeBalanced)
	}
}

func NormalizeAmbitionLevel(level string) string {
	switch strings.TrimSpace(strings.ToLower(level)) {
	case AmbitionLevelLow:
		return AmbitionLevelLow
	case AmbitionLevelHigh:
		return AmbitionLevelHigh
	case AmbitionLevelCustom:
		return AmbitionLevelCustom
	default:
		return AmbitionLevelMedium
	}
}

func isZeroExecutionProfile(profile ExecutionProfile) bool {
	return profile == (ExecutionProfile{})
}

func firstNonZeroProfile(values ...ExecutionProfile) ExecutionProfile {
	for _, value := range values {
		if !isZeroExecutionProfile(value) {
			return value
		}
	}
	return ExecutionProfile{}
}

type SystemActionType string

const (
	SystemActionSearch  SystemActionType = "search"
	SystemActionBuild   SystemActionType = "build"
	SystemActionTest    SystemActionType = "test"
	SystemActionLint    SystemActionType = "lint"
	SystemActionCommand SystemActionType = "command"
)

type SystemAction struct {
	Type        SystemActionType `json:"type"`
	Command     string           `json:"command"`
	Args        []string         `json:"args,omitempty"`
	Workdir     string           `json:"workdir,omitempty"`
	Description string           `json:"description,omitempty"`
}

type PendingApproval struct {
	StepIndex    int           `json:"step_index"`
	Reason       string        `json:"reason"`
	RequestedAt  time.Time     `json:"requested_at"`
	Target       string        `json:"target"`
	TaskType     string        `json:"task_type"`
	TaskText     string        `json:"task_text"`
	SystemAction *SystemAction `json:"system_action,omitempty"`
}

type WorkerTask struct {
	Target    string   `json:"target"`
	TaskType  string   `json:"task_type"`
	TaskText  string   `json:"task_text"`
	Artifacts []string `json:"artifacts,omitempty"`
	Reason    string   `json:"reason,omitempty"`
	NextHint  string   `json:"next_hint,omitempty"`
}

type ChainGoalStatus = string

const (
	ChainGoalStatusPending ChainGoalStatus = "pending"
	ChainGoalStatusRunning ChainGoalStatus = "running"
	ChainGoalStatusDone    ChainGoalStatus = "done"
	ChainGoalStatusFailed  ChainGoalStatus = "failed"
	ChainGoalStatusSkipped ChainGoalStatus = "skipped"
)

func ValidChainGoalStatus(status ChainGoalStatus) bool {
	switch status {
	case ChainGoalStatusPending, ChainGoalStatusRunning, ChainGoalStatusDone, ChainGoalStatusFailed, ChainGoalStatusSkipped:
		return true
	default:
		return false
	}
}

type ChainStatus = string

const (
	ChainStatusRunning   ChainStatus = "running"
	ChainStatusPaused    ChainStatus = "paused"
	ChainStatusDone      ChainStatus = "done"
	ChainStatusFailed    ChainStatus = "failed"
	ChainStatusCancelled ChainStatus = "cancelled"
)

func ValidChainStatus(status ChainStatus) bool {
	switch status {
	case ChainStatusRunning, ChainStatusPaused, ChainStatusDone, ChainStatusFailed, ChainStatusCancelled:
		return true
	default:
		return false
	}
}

type ChainGoal struct {
	Goal             string                  `json:"goal"`
	Provider         ProviderName            `json:"provider"`
	PipelineMode     string                  `json:"pipeline_mode,omitempty"`
	StrictnessLevel  string                  `json:"strictness_level,omitempty"`
	AmbitionLevel    string                  `json:"ambition_level,omitempty"`
	AmbitionText     string                  `json:"ambition_text,omitempty"`
	ContextMode      string                  `json:"context_mode,omitempty"`
	MaxSteps         int                     `json:"max_steps"`
	RoleOverrides    map[string]RoleOverride `json:"role_overrides,omitempty"`
	PreBuildCommands []string                `json:"pre_build_commands,omitempty"`
	EngineBuildCmd   string                  `json:"engine_build_cmd,omitempty"`
	EngineTestCmd    string                  `json:"engine_test_cmd,omitempty"`
	JobID            string                  `json:"job_id,omitempty"`
	Status           string                  `json:"status"`
}

type JobChain struct {
	ID           string      `json:"id"`
	Goals        []ChainGoal `json:"goals"`
	CurrentIndex int         `json:"current_index"`
	Status       string      `json:"status"`
	CreatedAt    time.Time   `json:"created_at"`
	UpdatedAt    time.Time   `json:"updated_at"`
}

type PlanningArtifact struct {
	Goal                  string                `json:"goal"`
	TechStack             string                `json:"tech_stack,omitempty"`
	WorkspaceDir          string                `json:"workspace_dir,omitempty"`
	Summary               string                `json:"summary"`
	ProductScope          []string              `json:"product_scope,omitempty"`
	NonGoals              []string              `json:"non_goals,omitempty"`
	ProposedSteps         []string              `json:"proposed_steps,omitempty"`
	InvariantsToPreserve  []string              `json:"invariants_to_preserve,omitempty"`
	Acceptance            []string              `json:"acceptance,omitempty"`
	SuccessSignals        []string              `json:"success_signals,omitempty"`
	VerificationContract  *VerificationContract `json:"verification_contract,omitempty"`
	RecommendedStrictness string                `json:"recommended_strictness,omitempty"`
	RecommendedMaxSteps   int                   `json:"recommended_max_steps,omitempty"`
}

type SprintContract struct {
	Version              int      `json:"version"`
	Goal                 string   `json:"goal"`
	RequiredStepTypes    []string `json:"required_step_types,omitempty"`
	AcceptanceCriteria   []string `json:"acceptance_criteria,omitempty"`
	BlockingCriteria     []string `json:"blocking_criteria,omitempty"`
	ThresholdSuccessCnt  int      `json:"threshold_success_count"`
	ThresholdMinSteps    int      `json:"threshold_min_steps"`
	ThresholdRequireEval bool     `json:"threshold_require_eval"`
	StrictnessLevel      string   `json:"strictness_level,omitempty"` // strict | normal | lenient
}

// RubricAxis defines a scoring dimension for multi-axis evaluation.
// When rubric_axes are present in a VerificationContract, the evaluator
// must score each axis and the orchestrator enforces per-axis thresholds.
type RubricAxis struct {
	Name         string  `json:"name"`
	Weight       float64 `json:"weight"`
	MinThreshold float64 `json:"min_threshold"`
}

// RubricScore records the evaluator's score for a single rubric axis.
type RubricScore struct {
	Axis   string  `json:"axis"`
	Score  float64 `json:"score"`
	Passed bool    `json:"passed"`
}

type VerificationContract struct {
	Version           int          `json:"version"`
	Goal              string       `json:"goal"`
	Scope             []string     `json:"scope,omitempty"`
	RequiredCommands  []string     `json:"required_commands,omitempty"`
	RequiredArtifacts []string     `json:"required_artifacts,omitempty"`
	RequiredChecks    []string     `json:"required_checks,omitempty"`
	DisallowedActions []string     `json:"disallowed_actions,omitempty"`
	MaxSeconds        int          `json:"max_seconds,omitempty"`
	Notes             string       `json:"notes,omitempty"`
	OwnerRole         RoleName     `json:"owner_role,omitempty"`
	RubricAxes        []RubricAxis `json:"rubric_axes,omitempty"`
}

type VerificationReport struct {
	Status        string   `json:"status"`
	Passed        bool     `json:"passed"`
	Reason        string   `json:"reason,omitempty"`
	Evidence      []string `json:"evidence,omitempty"`
	MissingChecks []string `json:"missing_checks,omitempty"`
	Artifacts     []string `json:"artifacts,omitempty"`
	ContractRef   string   `json:"contract_ref,omitempty"`
}

type StructuredReason struct {
	Category        string `json:"category"`
	Detail          string `json:"detail"`
	SuggestedAction string `json:"suggested_action"`
}

type TokenUsage struct {
	InputTokens      int     `json:"input_tokens"`
	OutputTokens     int     `json:"output_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	EstimatedCostUSD float64 `json:"estimated_cost_usd"`
}

type EvaluatorReport struct {
	Status           string        `json:"status"`
	Passed           bool          `json:"passed"`
	Score            int           `json:"score"`
	Reason           string        `json:"reason,omitempty"`
	MissingStepTypes []string      `json:"missing_step_types,omitempty"`
	Evidence         []string      `json:"evidence,omitempty"`
	ContractRef      string        `json:"contract_ref,omitempty"`
	RubricScores     []RubricScore `json:"rubric_scores,omitempty"`
}

type LeaderOutput struct {
	Action       string        `json:"action"`
	Target       string        `json:"target"`
	TaskType     string        `json:"task_type"`
	TaskText     string        `json:"task_text"`
	Artifacts    []string      `json:"artifacts,omitempty"`
	Reason       string        `json:"reason,omitempty"`
	NextHint     string        `json:"next_hint,omitempty"`
	SystemAction *SystemAction `json:"system_action,omitempty"`
	Tasks        []WorkerTask  `json:"tasks,omitempty"`
}

type WorkerOutput struct {
	Status                string            `json:"status"`
	Summary               string            `json:"summary"`
	Artifacts             []string          `json:"artifacts,omitempty"`
	FileContents          map[string]string `json:"file_contents,omitempty"`
	BlockedReason         string            `json:"blocked_reason,omitempty"`
	ErrorReason           string            `json:"error_reason,omitempty"`
	NextRecommendedAction string            `json:"next_recommended_action,omitempty"`
}

type Event struct {
	Time    time.Time `json:"time"`
	Kind    string    `json:"kind"`
	Message string    `json:"message"`
}

// ChainContext carries the previous chain step's results into the next job's planner.
// Pointer type with omitempty ensures first-goal jobs serialize without an empty object.
type ChainContext struct {
	Summary            string `json:"summary,omitempty"`
	EvaluatorReportRef string `json:"evaluator_report_ref,omitempty"`
}

type Step struct {
	Index            int               `json:"index"`
	Target           string            `json:"target"`
	TaskType         string            `json:"task_type"`
	TaskText         string            `json:"task_text"`
	Status           StepStatus        `json:"status"`
	Summary          string            `json:"summary,omitempty"`
	DiffSummary      string            `json:"diff_summary,omitempty"`
	Artifacts        []string          `json:"artifacts,omitempty"`
	BlockedReason    string            `json:"blocked_reason,omitempty"`
	ErrorReason      string            `json:"error_reason,omitempty"`
	StructuredReason *StructuredReason `json:"structured_reason,omitempty"`
	TokenUsage       TokenUsage        `json:"token_usage"`
	StartedAt        time.Time         `json:"started_at"`
	FinishedAt       time.Time         `json:"finished_at"`
}

type Job struct {
	ID                      string                  `json:"id"`
	Goal                    string                  `json:"goal"`
	TechStack               string                  `json:"tech_stack,omitempty"`
	WorkspaceDir            string                  `json:"workspace_dir,omitempty"`
	RequestedWorkspaceDir   string                  `json:"requested_workspace_dir,omitempty"`
	WorkspaceMode           string                  `json:"workspace_mode,omitempty"`
	Constraints             []string                `json:"constraints,omitempty"`
	DoneCriteria            []string                `json:"done_criteria,omitempty"`
	PipelineMode            string                  `json:"pipeline_mode,omitempty"`
	StrictnessLevel         string                  `json:"strictness_level,omitempty"` // strict | normal | lenient
	AmbitionLevel           string                  `json:"ambition_level,omitempty"`   // low | medium | high | custom
	AmbitionText            string                  `json:"ambition_text,omitempty"`    // custom text; replaces default when level=custom, prepended otherwise
	ContextMode             string                  `json:"context_mode,omitempty"`     // full | summary | minimal
	RoleProfiles            RoleProfiles            `json:"role_profiles"`
	RoleOverrides           map[string]RoleOverride `json:"role_overrides,omitempty"`
	VerificationContract    *VerificationContract   `json:"verification_contract,omitempty"`
	VerificationContractRef string                  `json:"verification_contract_ref,omitempty"`
	PlanningArtifacts       []string                `json:"planning_artifacts,omitempty"`
	SprintContractRef       string                  `json:"sprint_contract_ref,omitempty"`
	EvaluatorReportRef      string                  `json:"evaluator_report_ref,omitempty"`
	ChainID                 string                  `json:"chain_id,omitempty"`
	ChainGoalIndex          int                     `json:"chain_goal_index,omitempty"`
	ChainContext            *ChainContext           `json:"chain_context,omitempty"`
	Status                  JobStatus               `json:"status"`
	Provider                ProviderName            `json:"provider"`
	MaxSteps                int                     `json:"max_steps"`
	CurrentStep             int                     `json:"current_step"`
	RetryCount              int                     `json:"retry_count"`
	ResumeExtraStepsUsed    int                     `json:"resume_extra_steps_used,omitempty"`
	BlockedReason           string                  `json:"blocked_reason,omitempty"`
	FailureReason           string                  `json:"failure_reason,omitempty"`
	PendingApproval         *PendingApproval        `json:"pending_approval,omitempty"`
	Summary                 string                  `json:"summary,omitempty"`
	LeaderContextSummary    string                  `json:"leader_context_summary,omitempty"`
	SupervisorDirective     string                  `json:"supervisor_directive,omitempty"`
	// SchemaRetryHint carries the previous schema validation error message so
	// that the next provider call can include a correction hint in its prompt.
	// It is cleared after each successful parse and is never persisted to disk
	// (omitempty ensures it is excluded from JSON storage).
	// PreBuildCommands are run in the workspace directory before engine
	// verification (go build / go test). They are best-effort: failures are
	// logged but do not prevent the build/test from running. Useful for
	// language-agnostic setup steps such as "go mod tidy" or "npm install".
	PreBuildCommands        []string                `json:"pre_build_commands,omitempty"`
	// EngineBuildCmd overrides the default build command ("go build ./...").
	// Parsed via strings.Fields; e.g. "npm run build" or "make build".
	// Empty means use the default.
	EngineBuildCmd          string                  `json:"engine_build_cmd,omitempty"`
	// EngineTestCmd overrides the default test command ("go test ./...").
	// Parsed via strings.Fields; e.g. "npm test" or "make test".
	// Empty means use the default.
	EngineTestCmd           string                  `json:"engine_test_cmd,omitempty"`
	// PromptOverrides carries per-role prompt fragments that the provider
	// prepends to the hardcoded base prompt before each role call.
	// Keys are role names (director, executor, reviewer, evaluator).
	// Values are plain text prepended verbatim with a blank-line separator.
	// Set at job creation time and never mutated during execution.
	PromptOverrides         map[string]string       `json:"prompt_overrides,omitempty"`
	SchemaRetryHint         string                  `json:"schema_retry_hint,omitempty"`
	RunOwnerID              string                  `json:"run_owner_id,omitempty"`
	RunHeartbeatAt          time.Time               `json:"run_heartbeat_at,omitempty"`
	TokenUsage              TokenUsage              `json:"token_usage"`
	Steps                   []Step                  `json:"steps,omitempty"`
	Events                  []Event                 `json:"events,omitempty"`
	CreatedAt               time.Time               `json:"created_at"`
	UpdatedAt               time.Time               `json:"updated_at"`
}

// CloneJob returns a deep copy of the job so that the caller cannot mutate
// the original through shared slice/map/pointer fields. Used by the in-memory
// cache to prevent external callers from corrupting the authoritative state.
func CloneJob(src *Job) *Job {
	if src == nil {
		return nil
	}
	dst := *src

	// Deep-copy slices of value types.
	dst.Constraints = append([]string(nil), src.Constraints...)
	dst.DoneCriteria = append([]string(nil), src.DoneCriteria...)
	dst.PlanningArtifacts = append([]string(nil), src.PlanningArtifacts...)
	dst.PreBuildCommands = append([]string(nil), src.PreBuildCommands...)

	// Deep-copy Steps (slice of structs, but each step has inner slices).
	if len(src.Steps) > 0 {
		dst.Steps = make([]Step, len(src.Steps))
		for i, step := range src.Steps {
			cp := step
			cp.Artifacts = append([]string(nil), step.Artifacts...)
			if step.StructuredReason != nil {
				sr := *step.StructuredReason
				cp.StructuredReason = &sr
			}
			dst.Steps[i] = cp
		}
	}

	// Deep-copy Events.
	if len(src.Events) > 0 {
		dst.Events = make([]Event, len(src.Events))
		copy(dst.Events, src.Events)
	}

	// Deep-copy pointer fields.
	if src.PendingApproval != nil {
		pa := *src.PendingApproval
		if src.PendingApproval.SystemAction != nil {
			sa := *src.PendingApproval.SystemAction
			sa.Args = append([]string(nil), src.PendingApproval.SystemAction.Args...)
			pa.SystemAction = &sa
		}
		dst.PendingApproval = &pa
	}
	if src.VerificationContract != nil {
		vc := *src.VerificationContract
		vc.Scope = append([]string(nil), src.VerificationContract.Scope...)
		vc.RequiredCommands = append([]string(nil), src.VerificationContract.RequiredCommands...)
		vc.RequiredArtifacts = append([]string(nil), src.VerificationContract.RequiredArtifacts...)
		vc.RequiredChecks = append([]string(nil), src.VerificationContract.RequiredChecks...)
		vc.DisallowedActions = append([]string(nil), src.VerificationContract.DisallowedActions...)
		if len(src.VerificationContract.RubricAxes) > 0 {
			vc.RubricAxes = make([]RubricAxis, len(src.VerificationContract.RubricAxes))
			copy(vc.RubricAxes, src.VerificationContract.RubricAxes)
		}
		dst.VerificationContract = &vc
	}
	if src.ChainContext != nil {
		cc := *src.ChainContext
		dst.ChainContext = &cc
	}

	// Deep-copy RoleOverrides map.
	if len(src.RoleOverrides) > 0 {
		dst.RoleOverrides = make(map[string]RoleOverride, len(src.RoleOverrides))
		for k, v := range src.RoleOverrides {
			dst.RoleOverrides[k] = v
		}
	}

	// Deep-copy PromptOverrides map.
	if len(src.PromptOverrides) > 0 {
		dst.PromptOverrides = make(map[string]string, len(src.PromptOverrides))
		for k, v := range src.PromptOverrides {
			dst.PromptOverrides[k] = v
		}
	}

	return &dst
}
