package domain

import (
	"strings"
	"time"
)

type JobStatus string

const (
	JobStatusQueued        JobStatus = "queued"
	JobStatusStarting      JobStatus = "starting"
	JobStatusRunning       JobStatus = "running"
	JobStatusWaitingLeader JobStatus = "waiting_leader"
	JobStatusWaitingWorker JobStatus = "waiting_worker"
	JobStatusBlocked       JobStatus = "blocked"
	JobStatusFailed        JobStatus = "failed"
	JobStatusDone          JobStatus = "done"
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
	RolePlanner   RoleName = "planner"
	RoleLeader    RoleName = "leader"
	RoleExecutor  RoleName = "executor"
	RoleReviewer  RoleName = "reviewer"
	RoleTester    RoleName = "tester"
	RoleEvaluator RoleName = "evaluator"
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

type RoleProfile struct {
	Provider ProviderName `json:"provider,omitempty"`
	Model    string       `json:"model,omitempty"`
}

type RoleProfiles struct {
	Planner   ExecutionProfile `json:"planner"`
	Leader    ExecutionProfile `json:"leader"`
	Executor  ExecutionProfile `json:"executor"`
	Reviewer  ExecutionProfile `json:"reviewer"`
	Tester    ExecutionProfile `json:"tester"`
	Evaluator ExecutionProfile `json:"evaluator"`
}

func DefaultRoleProfiles(base ProviderName) RoleProfiles {
	// Each role gets a sensible default model tier.
	// Heavy reasoning roles (planner, leader, evaluator) use opus;
	// execution roles (executor, reviewer, tester) use sonnet for speed/cost.
	return RoleProfiles{
		Planner:   ExecutionProfile{Provider: base, Model: "opus"},
		Leader:    ExecutionProfile{Provider: base, Model: "opus"},
		Executor:  ExecutionProfile{Provider: base, Model: "sonnet"},
		Reviewer:  ExecutionProfile{Provider: base, Model: "sonnet"},
		Tester:    ExecutionProfile{Provider: base, Model: "sonnet"},
		Evaluator: ExecutionProfile{Provider: base, Model: "opus"},
	}
}

func (r RoleProfiles) Normalize(base ProviderName) RoleProfiles {
	r.Planner = r.Planner.withFallback(base)
	r.Leader = r.Leader.withFallback(base)
	r.Executor = r.Executor.withFallback(base)
	r.Reviewer = r.Reviewer.withFallback(base)
	r.Tester = r.Tester.withFallback(base)
	r.Evaluator = r.Evaluator.withFallback(base)
	return r
}

func (r RoleProfiles) ProfileFor(role RoleName, base ProviderName) ExecutionProfile {
	switch role {
	case RolePlanner:
		return r.Planner.withFallback(base)
	case RoleLeader:
		return r.Leader.withFallback(base)
	case RoleExecutor:
		return r.Executor.withFallback(base)
	case RoleReviewer:
		return r.Reviewer.withFallback(base)
	case RoleTester:
		return r.Tester.withFallback(base)
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
	case "review":
		return RoleReviewer
	case "test":
		return RoleTester
	case "implement", "build", "lint", "search", "command":
		return RoleExecutor
	default:
		return RoleExecutor
	}
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
	Goal            string       `json:"goal"`
	Provider        ProviderName `json:"provider"`
	StrictnessLevel string       `json:"strictness_level,omitempty"`
	ContextMode     string       `json:"context_mode,omitempty"`
	MaxSteps        int                    `json:"max_steps"`
	RoleOverrides   map[string]RoleProfile `json:"role_overrides,omitempty"`
	JobID           string                 `json:"job_id,omitempty"`
	Status          string                 `json:"status"`
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
	Goal                 string                `json:"goal"`
	TechStack            string                `json:"tech_stack,omitempty"`
	WorkspaceDir         string                `json:"workspace_dir,omitempty"`
	Summary              string                `json:"summary"`
	ProductScope         []string              `json:"product_scope,omitempty"`
	NonGoals             []string              `json:"non_goals,omitempty"`
	ProposedSteps        []string              `json:"proposed_steps,omitempty"`
	Acceptance           []string              `json:"acceptance,omitempty"`
	SuccessSignals       []string              `json:"success_signals,omitempty"`
	VerificationContract *VerificationContract `json:"verification_contract,omitempty"`
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

type VerificationContract struct {
	Version           int      `json:"version"`
	Goal              string   `json:"goal"`
	Scope             []string `json:"scope,omitempty"`
	RequiredCommands  []string `json:"required_commands,omitempty"`
	RequiredArtifacts []string `json:"required_artifacts,omitempty"`
	RequiredChecks    []string `json:"required_checks,omitempty"`
	DisallowedActions []string `json:"disallowed_actions,omitempty"`
	MaxSeconds        int      `json:"max_seconds,omitempty"`
	Notes             string   `json:"notes,omitempty"`
	OwnerRole         RoleName `json:"owner_role,omitempty"`
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
	Status           string   `json:"status"`
	Passed           bool     `json:"passed"`
	Score            int      `json:"score"`
	Reason           string   `json:"reason,omitempty"`
	MissingStepTypes []string `json:"missing_step_types,omitempty"`
	Evidence         []string `json:"evidence,omitempty"`
	ContractRef      string   `json:"contract_ref,omitempty"`
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
	ID                      string                 `json:"id"`
	Goal                    string                 `json:"goal"`
	TechStack               string                 `json:"tech_stack,omitempty"`
	WorkspaceDir            string                 `json:"workspace_dir,omitempty"`
	Constraints             []string               `json:"constraints,omitempty"`
	DoneCriteria            []string               `json:"done_criteria,omitempty"`
	StrictnessLevel         string                 `json:"strictness_level,omitempty"` // strict | normal | lenient
	ContextMode             string                 `json:"context_mode,omitempty"`     // full | summary | minimal
	RoleProfiles            RoleProfiles           `json:"role_profiles"`
	RoleOverrides           map[string]RoleProfile `json:"role_overrides,omitempty"`
	VerificationContract    *VerificationContract  `json:"verification_contract,omitempty"`
	VerificationContractRef string                 `json:"verification_contract_ref,omitempty"`
	PlanningArtifacts       []string               `json:"planning_artifacts,omitempty"`
	SprintContractRef       string                 `json:"sprint_contract_ref,omitempty"`
	EvaluatorReportRef      string                 `json:"evaluator_report_ref,omitempty"`
	ChainID                 string                 `json:"chain_id,omitempty"`
	ChainGoalIndex          int                    `json:"chain_goal_index,omitempty"`
	ChainContext            *ChainContext           `json:"chain_context,omitempty"`
	Status                  JobStatus              `json:"status"`
	Provider                ProviderName           `json:"provider"`
	MaxSteps                int                    `json:"max_steps"`
	CurrentStep             int                    `json:"current_step"`
	RetryCount              int                    `json:"retry_count"`
	BlockedReason           string                 `json:"blocked_reason,omitempty"`
	FailureReason           string                 `json:"failure_reason,omitempty"`
	PendingApproval         *PendingApproval       `json:"pending_approval,omitempty"`
	Summary                 string                 `json:"summary,omitempty"`
	LeaderContextSummary    string                 `json:"leader_context_summary,omitempty"`
	SupervisorDirective     string                 `json:"supervisor_directive,omitempty"`
	TokenUsage              TokenUsage             `json:"token_usage"`
	Steps                   []Step                 `json:"steps,omitempty"`
	Events                  []Event                `json:"events,omitempty"`
	CreatedAt               time.Time              `json:"created_at"`
	UpdatedAt               time.Time              `json:"updated_at"`
}
