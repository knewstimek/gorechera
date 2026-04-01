package provider

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gorechera/internal/domain"
)

func leaderSchema() string {
	// OpenAI structured output requires ALL properties to be in "required" and
	// additionalProperties: false at every level (strict schema mode).
	// Optional fields are included with empty/null defaults by the model.
	// action is restricted to an enum so the model cannot invent new values.
	return `{
  "type": "object",
  "properties": {
    "action": {"type": "string", "enum": ["run_worker", "run_workers", "run_system", "summarize", "complete", "fail", "blocked"]},
    "target": {"type": "string"},
    "task_type": {"type": "string"},
    "task_text": {"type": "string"},
    "tasks": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "target": {"type": "string"},
          "task_type": {"type": "string"},
          "task_text": {"type": "string"},
          "artifacts": {"type": "array", "items": {"type": "string"}},
          "reason": {"type": "string"},
          "next_hint": {"type": "string"}
        },
        "required": ["target", "task_type", "task_text", "artifacts", "reason", "next_hint"],
        "additionalProperties": false
      }
    },
    "artifacts": {"type": "array", "items": {"type": "string"}},
    "reason": {"type": "string"},
    "next_hint": {"type": "string"},
    "system_action": {
      "type": "object",
      "properties": {
        "type": {"type": "string"},
        "command": {"type": "string"},
        "args": {"type": "array", "items": {"type": "string"}},
        "workdir": {"type": "string"},
        "description": {"type": "string"}
      },
      "required": ["type", "command", "args", "workdir", "description"],
      "additionalProperties": false
    }
  },
  "required": ["action", "target", "task_type", "task_text", "tasks", "artifacts", "reason", "next_hint", "system_action"],
  "additionalProperties": false
}`
}

func plannerSchema() string {
	// OpenAI structured output requires ALL properties to be in "required" and
	// additionalProperties: false at every level (strict schema mode).
	return `{
  "type": "object",
  "properties": {
    "goal": {"type": "string"},
    "tech_stack": {"type": "string"},
    "workspace_dir": {"type": "string"},
    "summary": {"type": "string"},
    "product_scope": {"type": "array", "items": {"type": "string"}},
    "non_goals": {"type": "array", "items": {"type": "string"}},
    "proposed_steps": {"type": "array", "items": {"type": "string"}},
    "acceptance": {"type": "array", "items": {"type": "string"}},
    "success_signals": {"type": "array", "items": {"type": "string"}},
    "verification_contract": {
      "type": "object",
      "properties": {
        "version": {"type": "integer"},
        "goal": {"type": "string"},
        "scope": {"type": "array", "items": {"type": "string"}},
        "required_commands": {"type": "array", "items": {"type": "string"}},
        "required_artifacts": {"type": "array", "items": {"type": "string"}},
        "required_checks": {"type": "array", "items": {"type": "string"}},
        "disallowed_actions": {"type": "array", "items": {"type": "string"}},
        "max_seconds": {"type": "integer"},
        "notes": {"type": "string"},
        "owner_role": {"type": "string"}
      },
      "required": ["version", "goal", "scope", "required_commands", "required_artifacts", "required_checks", "disallowed_actions", "max_seconds", "notes", "owner_role"],
      "additionalProperties": false
    }
  },
  "required": ["goal", "tech_stack", "workspace_dir", "summary", "product_scope", "non_goals", "proposed_steps", "acceptance", "success_signals", "verification_contract"],
  "additionalProperties": false
}`
}

func evaluatorSchema() string {
	// OpenAI structured output requires ALL properties to be in "required" and
	// additionalProperties: false at every level (strict schema mode).
	// status is an enum so the model cannot invent invalid values like "ready".
	return `{
  "type": "object",
  "properties": {
    "status": {"type": "string", "enum": ["passed", "failed", "blocked"]},
    "passed": {"type": "boolean"},
    "score": {"type": "integer"},
    "reason": {"type": "string"},
    "missing_step_types": {"type": "array", "items": {"type": "string"}},
    "evidence": {"type": "array", "items": {"type": "string"}},
    "contract_ref": {"type": "string"},
    "verification_report": {
      "type": "object",
      "properties": {
        "status": {"type": "string", "enum": ["passed", "failed", "blocked"]},
        "passed": {"type": "boolean"},
        "reason": {"type": "string"},
        "evidence": {"type": "array", "items": {"type": "string"}},
        "missing_checks": {"type": "array", "items": {"type": "string"}},
        "artifacts": {"type": "array", "items": {"type": "string"}},
        "contract_ref": {"type": "string"}
      },
      "required": ["status", "passed", "reason", "evidence", "missing_checks", "artifacts", "contract_ref"],
      "additionalProperties": false
    }
  },
  "required": ["status", "passed", "score", "reason", "missing_step_types", "evidence", "contract_ref", "verification_report"],
  "additionalProperties": false
}`
}

func workerSchema() string {
	// OpenAI structured output requires ALL properties to be in "required" and
	// additionalProperties: false at every level (strict schema mode).
	// status is an enum so the model cannot invent invalid values.
	return `{
  "type": "object",
  "properties": {
    "status": {"type": "string", "enum": ["success", "failed", "blocked"]},
    "summary": {"type": "string"},
    "artifacts": {"type": "array", "items": {"type": "string"}},
    "blocked_reason": {"type": "string"},
    "error_reason": {"type": "string"},
    "next_recommended_action": {"type": "string"}
  },
  "required": ["status", "summary", "artifacts", "blocked_reason", "error_reason", "next_recommended_action"],
  "additionalProperties": false
}`
}

func buildPlannerPrompt(job domain.Job) string {
	payload, _ := json.MarshalIndent(job, "", "  ")
	return strings.TrimSpace(fmt.Sprintf(`
TASK: You are a planner component operating under an orchestrator supervisor. The supervisor manages the overall workflow, and the leader uses your planning artifacts to coordinate executor, reviewer, and tester workers. You define scope and verification expectations but do not perform implementation yourself.
The job data below is complete. Plan it now -- do not ask for more input.
Output only a JSON object matching the schema. No conversation, no preamble.

The goal to plan: %s

Full job state:
%s

Output requirements (all fields required in JSON):
- goal: restate the objective concisely
- summary: one-paragraph plan of how to achieve the goal
- tech_stack: technologies involved (empty string if none)
- workspace_dir: absolute workspace path from job state
- product_scope: array of what is in scope
- non_goals: array of what is explicitly out of scope
- proposed_steps: ordered array of implementation steps
- acceptance: array of measurable acceptance criteria
- success_signals: observable signals that indicate success
- verification_contract: object with version=1, goal=what to verify, required_artifacts=files that must exist after execution
`, job.Goal, string(payload)))
}

func buildEvaluatorPrompt(job domain.Job) string {
	payload, _ := json.MarshalIndent(job, "", "  ")
	contractPayload := "{}"
	if job.VerificationContract != nil {
		if data, err := json.MarshalIndent(job.VerificationContract, "", "  "); err == nil {
			contractPayload = string(data)
		}
	}
	return strings.TrimSpace(fmt.Sprintf(`
TASK: You are an evaluator component operating under an orchestrator supervisor. The supervisor monitors completion outcomes, and the leader plus workers provide the execution evidence you must assess. You verify results against the verification contract and report pass/fail/blocked decisions without performing implementation yourself.
The job data below is complete. Evaluate it now -- do not ask for more input.
Output only a JSON object matching the schema. No conversation, no preamble.

CRITICAL STATUS DECISION:
Look at the "steps" array in job state. Then pick ONE status:
- "passed" + passed=true: use this when an implement step with status "succeeded" covers the job goal
- "failed" + passed=false: use this when steps are missing, failed, or blocked
- "blocked" + passed=false: ONLY use this when you literally cannot see the steps data

Job goal: %s

EVALUATION PROCEDURE:
1. Find any step with status="succeeded" that relates to the goal
2. If found: output status="passed", passed=true, score=90+
3. If not found: output status="failed", passed=false
4. Do NOT use "blocked" if you can read the steps array

Current job state:
%s

Verification contract:
%s
`, job.Goal, string(payload), contractPayload))
}

func buildLeaderPrompt(job domain.Job) string {
	payload := buildLeaderJobPayload(job)
	contractPayload := "{}"
	strictnessLevel := strings.TrimSpace(job.StrictnessLevel)
	if strings.TrimSpace(job.SprintContractRef) != "" {
		if data, err := os.ReadFile(job.SprintContractRef); err == nil {
			contractPayload = string(data)
			var sprint domain.SprintContract
			if err := json.Unmarshal(data, &sprint); err == nil && strings.TrimSpace(sprint.StrictnessLevel) != "" {
				strictnessLevel = strings.TrimSpace(sprint.StrictnessLevel)
			}
		}
	}
	completionRules := []string{
		`Completion rules:`,
		`- Use action="complete" only when the sprint contract is satisfied and the goal is fully achieved.`,
		`- If required step coverage is missing, dispatch the missing work instead of choosing complete.`,
		`- Do NOT use summarize as a substitute for complete. Summarize is only for recording intermediate progress between worker dispatches. If all required work is done and verified, choose complete immediately. Do not summarize more than once consecutively.`,
	}
	if strings.EqualFold(strictnessLevel, "strict") {
		// Strict mode uses the sprint contract as a gate, so the leader must
		// schedule each required worker phase before attempting completion.
		completionRules = append(completionRules,
			`- Strict mode is active. Before action="complete", you MUST dispatch and obtain succeeded worker steps for implement, then review, then test.`,
			`- In strict mode, review must happen after implement succeeds, and test must happen after review succeeds.`,
			`- If implement, review, or test is missing or not succeeded yet, choose run_worker or run_workers for the next required stage instead of complete.`,
		)
	}
	return strings.TrimSpace(fmt.Sprintf(`
TASK: You are a leader component operating under an orchestrator supervisor. The supervisor agent monitors your decisions via MCP tools and may inject [SUPERVISOR] directives into the leader context. You coordinate workers (executor, reviewer, tester) but do not perform implementation yourself.
The job data below is complete. Decide and output the next action now -- do not ask for input.
Output only a JSON object matching the schema. No conversation, no preamble.

Job goal: %s

Valid actions (choose exactly one):
- run_worker: assign a task to a single worker (target: "B", "C", or "D") -- PREFERRED for most tasks including file creation
- run_workers: assign tasks to exactly 2 workers in parallel (disjoint targets)
- run_system: run an allowlisted system command (target must be "SYS")
- summarize: record a summary of progress so far
- complete: mark the job as done (only when the goal is fully achieved)
- fail: mark the job as failed (when it cannot proceed)
- blocked: mark the job as blocked (when external information is needed)

IMPORTANT: Use run_worker (not run_system) for file creation, code writing, and most implementation tasks.
Workers can create files, write code, and perform shell actions in the workspace.
run_system is for build/lint/test/search commands only: allowed executables are go, rg, grep, cargo, make, npm.

For run_worker: set action="run_worker", target=one of "B"/"C"/"D", task_type=one of "implement"/"review"/"test"/"search"/"build"/"lint"/"command", task_text=full description of what to do
For run_workers: set action="run_workers", tasks=array of exactly 2 task objects with distinct targets
For run_system: set action="run_system", target="SYS", task_type=one of "build"/"test"/"lint"/"search"/"command", system_action.command=single allowed executable, system_action.args=array
For summarize: set action="summarize", reason=summary text
For complete/fail/blocked: set action, reason=explanation

%s

Current job state:
%s

Sprint contract:
%s
If the leader context contains a [SUPERVISOR] directive, follow it with highest priority.
Supervisor directives override previous plans.

`, job.Goal, strings.Join(completionRules, "\n"), payload, contractPayload))
}

// buildLeaderJobPayload serializes the job state for the leader prompt,
// respecting the job's ContextMode setting to control payload size.
func buildLeaderJobPayload(job domain.Job) string {
	mode := strings.TrimSpace(strings.ToLower(job.ContextMode))
	if mode == "" {
		mode = "full"
	}
	switch mode {
	case "summary":
		return buildSummaryPayload(job)
	case "minimal":
		return buildMinimalPayload(job)
	default:
		raw, _ := json.MarshalIndent(job, "", "  ")
		return string(raw)
	}
}

func buildSummaryPayload(job domain.Job) string {
	type summaryStep struct {
		Index    int    `json:"index"`
		Type     string `json:"task_type"`
		Status   string `json:"status"`
		Summary  string `json:"summary,omitempty"`
		TaskText string `json:"task_text,omitempty"`
	}
	type summaryJob struct {
		Goal                 string        `json:"goal"`
		Summary              string        `json:"summary,omitempty"`
		LeaderContextSummary string        `json:"leader_context_summary,omitempty"`
		StrictnessLevel      string        `json:"strictness_level,omitempty"`
		ContextMode          string        `json:"context_mode"`
		Status               string        `json:"status"`
		CurrentStep          int           `json:"current_step"`
		MaxSteps             int           `json:"max_steps"`
		TokenUsage           interface{}   `json:"token_usage"`
		Steps                []summaryStep `json:"steps"`
	}
	steps := make([]summaryStep, 0, len(job.Steps))
	for i, s := range job.Steps {
		ss := summaryStep{Index: s.Index, Type: s.TaskType, Status: string(s.Status)}
		// Last 2 steps get full detail; earlier steps get truncated summary
		if i >= len(job.Steps)-2 {
			ss.Summary = s.Summary
			ss.TaskText = s.TaskText
		} else {
			summary := s.Summary
			if len(summary) > 80 {
				summary = summary[:80] + "..."
			}
			ss.Summary = summary
		}
		steps = append(steps, ss)
	}
	out := summaryJob{
		Goal:                 job.Goal,
		Summary:              job.Summary,
		LeaderContextSummary: job.LeaderContextSummary,
		StrictnessLevel:      job.StrictnessLevel,
		ContextMode:          "summary",
		Status:               string(job.Status),
		CurrentStep:          job.CurrentStep,
		MaxSteps:             job.MaxSteps,
		TokenUsage:           job.TokenUsage,
		Steps:                steps,
	}
	raw, _ := json.MarshalIndent(out, "", "  ")
	return string(raw)
}

func buildMinimalPayload(job domain.Job) string {
	succeeded, failed, active := 0, 0, 0
	for _, s := range job.Steps {
		switch s.Status {
		case domain.StepStatusSucceeded:
			succeeded++
		case domain.StepStatusFailed:
			failed++
		default:
			active++
		}
	}
	type minimalStep struct {
		Index    int    `json:"index"`
		Type     string `json:"task_type"`
		Status   string `json:"status"`
		Summary  string `json:"summary,omitempty"`
		TaskText string `json:"task_text,omitempty"`
	}
	type minimalJob struct {
		Goal                 string       `json:"goal"`
		Summary              string       `json:"summary,omitempty"`
		LeaderContextSummary string       `json:"leader_context_summary,omitempty"`
		StrictnessLevel      string       `json:"strictness_level,omitempty"`
		ContextMode          string       `json:"context_mode"`
		Status               string       `json:"status"`
		CurrentStep          int          `json:"current_step"`
		MaxSteps             int          `json:"max_steps"`
		SucceededSteps       int          `json:"succeeded_steps"`
		FailedSteps          int          `json:"failed_steps"`
		ActiveSteps          int          `json:"active_steps"`
		LastStep             *minimalStep `json:"last_step,omitempty"`
	}
	out := minimalJob{
		Goal:                 job.Goal,
		Summary:              job.Summary,
		LeaderContextSummary: job.LeaderContextSummary,
		StrictnessLevel:      job.StrictnessLevel,
		ContextMode:          "minimal",
		Status:               string(job.Status),
		CurrentStep:          job.CurrentStep,
		MaxSteps:             job.MaxSteps,
		SucceededSteps:       succeeded,
		FailedSteps:          failed,
		ActiveSteps:          active,
	}
	if len(job.Steps) > 0 {
		last := job.Steps[len(job.Steps)-1]
		out.LastStep = &minimalStep{
			Index:    last.Index,
			Type:     last.TaskType,
			Status:   string(last.Status),
			Summary:  last.Summary,
			TaskText: last.TaskText,
		}
	}
	raw, _ := json.MarshalIndent(out, "", "  ")
	return string(raw)
}

func buildWorkerPrompt(job domain.Job, task domain.LeaderOutput) string {
	jobPayload, _ := json.MarshalIndent(job, "", "  ")
	taskPayload, _ := json.MarshalIndent(task, "", "  ")
	contractPayload := "{}"
	if job.VerificationContract != nil {
		if data, err := json.MarshalIndent(job.VerificationContract, "", "  "); err == nil {
			contractPayload = string(data)
		}
	}
	return strings.TrimSpace(fmt.Sprintf(`
TASK: You are an executor worker assigned by the leader. You perform the implementation task described below. Report results accurately including files changed, commands run, and any errors encountered.
The assigned task below is complete and ready to execute. Do it now -- do not ask for input.
Output only a JSON object matching the schema. No conversation, no preamble.
status MUST be one of: success, failed, blocked.

Overall job goal: %s

Assigned task:
%s

File management rules:
- Only create or modify files specified in your task
- Do NOT delete files you did not create
- Do NOT modify files outside the workspace directory
- If working in parallel with another worker, only touch your assigned files
- Use actual shell commands to create files (e.g. echo, touch, write commands)

Job state:
%s

Verification contract:
%s
`, job.Goal, string(taskPayload), string(jobPayload), contractPayload))
}

func profilePrompt(role domain.RoleName, job domain.Job) string {
	profile := job.RoleProfiles.ProfileFor(role, job.Provider)
	data, _ := json.MarshalIndent(profile, "", "  ")
	return fmt.Sprintf("%s profile:\n%s", role, string(data))
}

func writeSchemaFile(workspaceDir, name, schema string) (string, error) {
	base := firstNonEmpty(workspaceDir, os.TempDir())
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(base, "gorechera-provider-*")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(schema), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
