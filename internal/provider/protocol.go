package provider

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"gorchera/internal/domain"
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
      "anyOf": [
        {"type": "null"},
        {
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
      ]
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
    "invariants_to_preserve": {"type": "array", "items": {"type": "string"}},
    "acceptance": {"type": "array", "items": {"type": "string"}},
    "success_signals": {"type": "array", "items": {"type": "string"}},
    "recommended_strictness": {"type": "string", "enum": ["strict", "normal", "lenient"]},
    "recommended_max_steps": {"type": "integer", "minimum": 1},
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
        "owner_role": {"type": "string"},
        "automated_checks": {
          "type": "array",
          "items": {
            "type": "object",
            "properties": {
              "type": {"type": "string", "enum": ["grep", "file_exists", "file_unchanged", "no_new_deps"]},
              "pattern": {"type": "string"},
              "file": {"type": "string"},
              "path": {"type": "string"},
              "ref": {"type": "string"},
              "description": {"type": "string"}
            },
            "required": ["type", "description"],
            "additionalProperties": false
          }
        }
      },
      "required": ["version", "goal", "scope", "required_commands", "required_artifacts", "required_checks", "disallowed_actions", "max_seconds", "notes", "owner_role"],
      "additionalProperties": false
    }
  },
  "required": ["goal", "tech_stack", "workspace_dir", "summary", "product_scope", "non_goals", "proposed_steps", "invariants_to_preserve", "acceptance", "success_signals", "recommended_strictness", "recommended_max_steps", "verification_contract"],
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
    },
    "rubric_scores": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "axis": {"type": "string"},
          "score": {"type": "number"},
          "reasoning": {"type": "string"}
        },
        "required": ["axis", "score", "reasoning"],
        "additionalProperties": false
      }
    }
  },
  "required": ["status", "passed", "score", "reason", "missing_step_types", "evidence", "contract_ref", "verification_report", "rubric_scores"],
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
	plannerJob := job
	plannerJob.AmbitionLevel = ""
	payload, _ := json.MarshalIndent(plannerJob, "", "  ")
	chainSection := ""
	if job.ChainContext != nil && (job.ChainContext.Summary != "" || job.ChainContext.EvaluatorReportRef != "") {
		chainSection = fmt.Sprintf("\n\n## Previous chain step results\n\nSummary: %s\nEvaluator report: %s\n",
			job.ChainContext.Summary, job.ChainContext.EvaluatorReportRef)
	}

	// Build role profiles section so the director knows which models handle each role.
	// This informs the recommended_strictness and recommended_max_steps decisions.
	var roleProfilesSection strings.Builder
	{
		rp := job.RoleProfiles
		type roleEntry struct {
			name    string
			profile domain.ExecutionProfile
		}
		entries := []roleEntry{
			{"director", rp.ProfileFor(domain.RoleDirector, job.Provider)},
			{"executor", rp.Executor},
			{"reviewer", rp.Reviewer},
			{"evaluator", rp.Evaluator},
		}
		roleProfilesSection.WriteString("\nRole profiles (which models handle each role):\n")
		for _, e := range entries {
			model := e.profile.Model
			if model == "" {
				model = "default"
			}
			provider := string(e.profile.Provider)
			if provider == "" {
				provider = "default"
			}
			roleProfilesSection.WriteString(fmt.Sprintf("- %s: %s/%s\n", e.name, provider, model))
		}
	}

	base := strings.TrimSpace(fmt.Sprintf(`
TASK: You are a director planning component operating under an orchestrator supervisor. The supervisor manages the overall workflow, and you define the execution plan, sprint contract, and verification expectations that the director dispatch loop will enforce. You do not perform implementation yourself.
The job data below is complete. Plan it now -- do not ask for more input.
Output only a JSON object matching the schema. No conversation, no preamble.

The goal to plan: %s

Full job state:
%s
%s%s

## Codebase analysis (do this first)
Before writing the spec, read the relevant source files in the workspace to understand current implementation state. List specific files you examined and key findings. This grounds your plan in reality rather than assumptions.

## Concrete improvement descriptions
For each planned change, explain what currently exists, what is wrong or missing, and the specific improvement. Avoid vague descriptions. The executor and reviewer must be able to understand exactly what to change and why.

## Invariants to preserve
List the existing behaviors, contracts, or boundaries that downstream workers must keep intact while making the change. Use an empty array when there are no meaningful invariants.

## Acceptance criteria
Include measurable acceptance criteria for each deliverable. Each criterion must be verifiable by the evaluator. Criteria like 'code is clean' are not acceptable -- use criteria like 'function X returns Y when given Z' or 'go test ./... exits 0'.

Output requirements (all fields required in JSON):
- goal: restate the objective concisely
- summary: one-paragraph plan of how to achieve the goal
- tech_stack: technologies involved (empty string if none)
- workspace_dir: absolute workspace path from job state
- product_scope: array of what is in scope
- non_goals: array of what is explicitly out of scope
- proposed_steps: ordered array of implementation steps
- invariants_to_preserve: array of behaviors/contracts that must not break during implementation (use [] when none apply)
- acceptance: array of measurable acceptance criteria
- success_signals: observable signals that indicate success
- recommended_strictness: recommend "strict", "normal", or "lenient" based on goal complexity and model capabilities. Stronger models (opus) can handle stricter evaluation and fewer steps; weaker models (sonnet, haiku) benefit from normal strictness and more steps. Use "strict" only for goals requiring tight review coverage with capable models; use "normal" for most goals; use "lenient" for simple or exploratory tasks.
- recommended_max_steps: recommend the number of execution steps needed for this goal (minimum 1). Simpler goals with stronger models need fewer steps (e.g. 3-5); complex goals or weaker models may need more (e.g. 6-10).
- verification_contract: object with version=1, goal=what to verify, required_artifacts=files that must exist after execution

When writing the verification_contract, include automated_checks for any requirement that can be verified mechanically:
- type "grep": verify a pattern exists in source files. Set pattern (regex) and file (glob like "*.go").
- type "file_exists": verify a file was created. Set path (relative to workspace).
- type "file_unchanged": verify a file was NOT modified. Set path.
- type "no_new_deps": verify no new external dependencies were added to go.mod.
Keep required_checks only for requirements that need human/AI judgment (code quality, correctness, edge cases).
`, job.Goal, string(payload), chainSection, roleProfilesSection.String()))
	return applyPromptOverrides(base, "director", job.WorkspaceDir, job.PromptOverrides)
}

// ambitionInstruction returns the executor autonomy guidance text.
// When ambition_text is provided:
//   - level=custom: ambition_text fully replaces the default (falls back to medium if blank)
//   - low/medium/high: ambition_text is prepended to the default with a blank-line separator
func ambitionInstruction(level, ambitionText string) string {
	normalized := domain.NormalizeAmbitionLevel(level)
	var base string
	switch normalized {
	case domain.AmbitionLevelLow:
		base = "Do exactly what is described. Do not improve, refactor, or extend beyond the explicit task."
	case domain.AmbitionLevelHigh:
		base = "Achieve the goal and go further. Propose and implement structural improvements, suggest better patterns, flag risks the goal didn't mention. Expand scope if justified."
	case domain.AmbitionLevelCustom:
		// custom with no text: fall back to medium behavior
		if strings.TrimSpace(ambitionText) == "" {
			return "Complete the task. If you notice directly related improvements (missing error handling, obvious edge cases), include them but stay within the stated scope."
		}
		return "Autonomy guidance:\n" + strings.TrimSpace(ambitionText)
	default:
		base = "Complete the task. If you notice directly related improvements (missing error handling, obvious edge cases), include them but stay within the stated scope."
	}
	if strings.TrimSpace(ambitionText) != "" {
		return "Autonomy guidance:\n" + strings.TrimSpace(ambitionText) + "\n\n" + base
	}
	return base
}

// ambitionEvaluationGuidance returns the evaluator gate guidance text.
// Same prepend/replace logic as ambitionInstruction.
func ambitionEvaluationGuidance(level, ambitionText string) string {
	normalized := domain.NormalizeAmbitionLevel(level)
	var base string
	switch normalized {
	case domain.AmbitionLevelLow:
		base = "Ambition level is low. Judge the result against the explicit task only. Do not require extra refactors, improvements, or scope expansion."
	case domain.AmbitionLevelHigh:
		base = "Ambition level is high. Accept justified scope expansion when it materially supports the goal. Do not fail solely because the worker improved structure, proposed better patterns, or flagged adjacent risks beyond the original task."
	case domain.AmbitionLevelCustom:
		if strings.TrimSpace(ambitionText) == "" {
			return "Ambition level is medium. Accept directly related improvements such as obvious error handling or edge-case fixes, but still enforce the stated scope."
		}
		return "Autonomy guidance:\n" + strings.TrimSpace(ambitionText)
	default:
		base = "Ambition level is medium. Accept directly related improvements such as obvious error handling or edge-case fixes, but still enforce the stated scope."
	}
	if strings.TrimSpace(ambitionText) != "" {
		return "Autonomy guidance:\n" + strings.TrimSpace(ambitionText) + "\n\n" + base
	}
	return base
}

func buildEvaluatorPrompt(job domain.Job) string {
	contractPayload := "{}"
	if job.VerificationContract != nil {
		if data, err := json.MarshalIndent(job.VerificationContract, "", "  "); err == nil {
			contractPayload = string(data)
		}
	}

	rubricSection := ""
	if job.VerificationContract != nil && len(job.VerificationContract.RubricAxes) > 0 {
		var b strings.Builder
		b.WriteString("\nRUBRIC SCORING:\n")
		b.WriteString("Score each of the following axes on a 0.0 to 1.0 scale and return results in the rubric_scores array.\n")
		b.WriteString("Each entry must include: axis (name), score (0.0-1.0), reasoning (one sentence).\n")
		b.WriteString("Axes to score:\n")
		for _, axis := range job.VerificationContract.RubricAxes {
			b.WriteString(fmt.Sprintf("- %s (min_threshold: %.2f, weight: %.2f)\n", axis.Name, axis.MinThreshold, axis.Weight))
		}
		rubricSection = b.String()
	}

	// depthGuidance scales verification effort to pipeline_mode.
	pipelineMode := domain.NormalizePipelineMode(job.PipelineMode)
	var depthGuidance string
	switch pipelineMode {
	case string(domain.PipelineModeLight):
		depthGuidance = "Verification depth: QUICK. Check engine results and verify goal satisfaction by reading key changed files. Focus on correctness, not style."
	case string(domain.PipelineModeFull):
		depthGuidance = "Verification depth: EXHAUSTIVE. Read all changed files plus adjacent code. Hunt for counterexamples, regressions, and edge cases beyond the immediate scope."
	default:
		depthGuidance = "Verification depth: THOROUGH. Read all changed files, check edge cases, verify goal alignment. Report concrete issues only."
	}

	// schemaRetrySection is injected when a previous evaluator attempt produced
	// an invalid response so the model knows exactly what to correct.
	schemaRetrySection := ""
	if strings.TrimSpace(job.SchemaRetryHint) != "" {
		schemaRetrySection = fmt.Sprintf("\nCORRECTION REQUIRED: Your previous response failed schema validation: %s\nRespond with valid JSON matching the required schema.\n", job.SchemaRetryHint)
	}

	base := strings.TrimSpace(fmt.Sprintf(`
TASK: You are an evaluator for an orchestrator-managed job. You verify results against the verification contract and report pass/fail/blocked. You do not implement anything yourself.
The job data below is complete. Evaluate it now. Output only a JSON object matching the schema.

ROLE:
- You are a release gate, not a cheerleader.
- Do NOT pass merely because a worker reported success or one implement step succeeded.
- %s
- %s

Job goal: %s

PROCEDURE (mandatory -- follow every step in order):
1. Read diff_summary and error_reason in each step to understand what changed and what failed.
2. Open and read the artifact files listed in each step. These contain engine build/test results and worker outputs. Do NOT rely solely on step summaries.
3. Read the actual source files that were changed (use diff_summary to identify filenames) and verify they satisfy the goal.
4. Check the verification contract below. Confirm each required_check is satisfied by actual evidence you read.
5. Check input/output contracts, invariants, edge cases, lifecycle/restart/retry/recovery/idempotency issues where relevant.
6. Look for missing validation, hidden regressions, and contradictions between artifacts and actual code.
7. Decide:
   - status="passed": contract satisfied, goal achieved, no material contradiction in evidence you read.
   - status="failed": missing coverage, unmet criteria, regressions, or unresolved failures. Provide concrete missing_step_types and evidence.
   - status="blocked": evidence genuinely insufficient even after reading the workspace.
8. Base your decision on what you actually read, not on what the worker claimed.
%s%s%s
Current job state:
%s

Verification contract:
%s
`, ambitionEvaluationGuidance(job.AmbitionLevel, job.AmbitionText), depthGuidance, job.Goal, rubricSection, schemaRetrySection, "", buildCompactEvaluatorPayload(job), contractPayload))
	return applyPromptOverrides(base, "evaluator", job.WorkspaceDir, job.PromptOverrides)
}

func buildLeaderPrompt(job domain.Job) string {
	payload := buildLeaderJobPayload(job)
	contractPayload := "{}"
	strictnessLevel := strings.TrimSpace(job.StrictnessLevel)
	invariantsSection := fmt.Sprintf("Planning invariants to preserve:\n%s\n\n", formatPromptList(job.Constraints, "- None provided."))
	supervisorSection := ""
	if strings.TrimSpace(job.SupervisorDirective) != "" {
		supervisorSection = fmt.Sprintf("Supervisor directive:\n%s\n\n", job.SupervisorDirective)
	}
	// schemaRetrySection is injected when a previous attempt produced an
	// invalid response so the model knows exactly what to correct.
	schemaRetrySection := ""
	if strings.TrimSpace(job.SchemaRetryHint) != "" {
		schemaRetrySection = fmt.Sprintf("CORRECTION REQUIRED: Your previous response failed schema validation: %s\nRespond with valid JSON matching the required schema.\n\n", job.SchemaRetryHint)
	}
	if strings.TrimSpace(job.SprintContractRef) != "" {
		if data, err := os.ReadFile(job.SprintContractRef); err == nil {
			contractPayload = string(data)
			var sprint domain.SprintContract
			if err := json.Unmarshal(data, &sprint); err == nil && strings.TrimSpace(sprint.StrictnessLevel) != "" {
				strictnessLevel = strings.TrimSpace(sprint.StrictnessLevel)
			}
		}
	}
	pipelineMode := domain.NormalizePipelineMode(job.PipelineMode)
	completionRules := []string{
		`Completion rules:`,
		`- Use action="complete" only when the sprint contract is satisfied and the goal is fully achieved.`,
		`- Engine-managed go build ./... and go test ./... run automatically after each successful implement step. Do NOT dispatch tester work to reproduce that gate.`,
		`- [no test files] in engine test artifacts is normal -- it means the package has no _test.go files, NOT a test failure. Do not retry or attempt to fix this.`,
		`- If required step coverage is missing, dispatch the missing work instead of choosing complete.`,
		`- Do NOT use summarize as a substitute for complete. Summarize is only for recording intermediate progress between worker dispatches. If all required work is done and verified, choose complete immediately. Do not summarize more than once consecutively.`,
		`- If the change touches lifecycle, restart, retry, recovery, concurrency, deduplication, external pricing/config, authentication boundaries, or UI/event-delivery boundaries, the evaluator will verify for regressions and counterexamples -- you do not need to dispatch a separate review step.`,
	}
	switch pipelineMode {
	case string(domain.PipelineModeLight):
		completionRules = append(completionRules,
			`- Pipeline mode is light. After implement succeeds and engine checks pass, the evaluator performs quick verification. If the evaluator fails, you will receive specific findings in context -- dispatch fix steps to address them before re-completing.`,
		)
	case string(domain.PipelineModeFull):
		completionRules = append(completionRules,
			`- Pipeline mode is full. After implement succeeds and engine checks pass, the evaluator performs exhaustive code review including adjacent code and edge cases. If the evaluator fails, you will receive specific findings in context -- dispatch fix steps to address them before re-completing.`,
		)
	default:
		completionRules = append(completionRules,
			`- After implement succeeds and engine checks pass, the evaluator performs thorough code review. If the evaluator fails, you will receive specific findings in context -- dispatch fix steps to address them before re-completing.`,
		)
	}
	if strings.EqualFold(strictnessLevel, "strict") {
		completionRules = append(completionRules,
			`- Strict mode is active. Do not choose complete until every required director stage has succeeded and the evaluator gate can pass without inference.`,
		)
	}
	base := strings.TrimSpace(fmt.Sprintf(`
TASK: You are a director dispatch component operating under an orchestrator supervisor. The supervisor agent monitors your decisions via MCP tools and may inject [SUPERVISOR] directives as a separate supervisor directive. You coordinate executor and reviewer workers, rely on engine-managed build/test verification, and do not perform implementation yourself.
The job data below is complete. Decide and output the next action now -- do not ask for input.
Output only a JSON object matching the schema. No conversation, no preamble.

Job goal: %s

Pipeline mode: %s

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

For run_worker: set action="run_worker", target=one of "B"/"C"/"D", task_type=one of "implement"/"test"/"search"/"build"/"lint"/"command", task_text=full description of what to do
For run_workers: set action="run_workers", tasks=array of exactly 2 task objects with distinct targets
For run_system: set action="run_system", target="SYS", task_type=one of "build"/"test"/"lint"/"search"/"command", system_action.command=single allowed executable, system_action.args=array
For summarize: set action="summarize", reason=summary text
For complete/fail/blocked: set action, reason=explanation

When dispatching a worker, structure task_text so the worker can parse intent and boundaries quickly:
- Start with the concrete objective.
- Include a dedicated task_why: section that explains why this task matters to the job goal right now.
- Include a dedicated scope_boundary: section that says what the worker must not change, what stays out of scope, or where to stop.
- Reflect the planning invariants in the task_text when they are relevant to the assigned scope.

%s

%s%s%sCurrent job state:
%s

Sprint contract:
%s
If the supervisor directive section is present, follow it with highest priority.
Supervisor directives override previous plans.

`, job.Goal, pipelineMode, strings.Join(completionRules, "\n"), invariantsSection, supervisorSection, schemaRetrySection, payload, contractPayload))
	// Director role: the dispatch loop (leader) also uses the "director" role key
	// so that a single workspace file covers both planner and dispatch phases.
	return applyPromptOverrides(base, "director", job.WorkspaceDir, job.PromptOverrides)
}

// autoContextMode selects a context mode based on step count thresholds.
// The model parameter is accepted for forward compatibility (future per-model
// tuning) but is not used in the threshold logic yet.
func autoContextMode(model string, stepCount int) string {
	switch {
	case stepCount < 10:
		return "full"
	case stepCount <= 20:
		return "summary"
	default:
		return "minimal"
	}
}

// buildLeaderJobPayload serializes the job state for the leader prompt,
// respecting the job's ContextMode setting to control payload size.
func buildLeaderJobPayload(job domain.Job) string {
	job.SupervisorDirective = ""
	mode := strings.TrimSpace(strings.ToLower(job.ContextMode))
	if mode == "" {
		mode = "full"
	}
	// Resolve 'auto' to a concrete mode based on current step count.
	if mode == "auto" {
		mode = autoContextMode(job.RoleProfiles.Leader.Model, len(job.Steps))
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
		Constraints          []string      `json:"constraints,omitempty"`
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
			if len([]rune(summary)) > 80 {
				summary = string([]rune(summary)[:80]) + "..."
			}
			ss.Summary = summary
		}
		steps = append(steps, ss)
	}
	out := summaryJob{
		Goal:                 job.Goal,
		Summary:              job.Summary,
		LeaderContextSummary: job.LeaderContextSummary,
		Constraints:          append([]string(nil), job.Constraints...),
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
	succeeded, failed, blocked, active := 0, 0, 0, 0
	for _, s := range job.Steps {
		switch s.Status {
		case domain.StepStatusSucceeded:
			succeeded++
		case domain.StepStatusFailed:
			failed++
		case domain.StepStatusBlocked:
			blocked++
		case "", domain.StepStatusActive:
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
		Constraints          []string     `json:"constraints,omitempty"`
		StrictnessLevel      string       `json:"strictness_level,omitempty"`
		ContextMode          string       `json:"context_mode"`
		Status               string       `json:"status"`
		CurrentStep          int          `json:"current_step"`
		MaxSteps             int          `json:"max_steps"`
		SucceededSteps       int          `json:"succeeded_steps"`
		FailedSteps          int          `json:"failed_steps"`
		BlockedSteps         int          `json:"blocked_steps"`
		ActiveSteps          int          `json:"active_steps"`
		LastStep             *minimalStep `json:"last_step,omitempty"`
	}
	out := minimalJob{
		Goal:                 job.Goal,
		Summary:              job.Summary,
		LeaderContextSummary: job.LeaderContextSummary,
		Constraints:          append([]string(nil), job.Constraints...),
		StrictnessLevel:      job.StrictnessLevel,
		ContextMode:          "minimal",
		Status:               string(job.Status),
		CurrentStep:          job.CurrentStep,
		MaxSteps:             job.MaxSteps,
		SucceededSteps:       succeeded,
		FailedSteps:          failed,
		BlockedSteps:         blocked,
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

// buildCompactExecutorPayload returns a minimal job context for executor workers.
// Includes workspace info and previous failure reason only -- not the full job
// JSON, all step results, or verification contract details.
func buildCompactExecutorPayload(job domain.Job, task domain.LeaderOutput) string {
	type prevFailure struct {
		StepIndex int    `json:"step_index"`
		TaskType  string `json:"task_type"`
		Reason    string `json:"reason"`
	}
	type compactPayload struct {
		JobID         string       `json:"job_id,omitempty"`
		WorkspaceDir  string       `json:"workspace_dir,omitempty"`
		WorkspaceMode string       `json:"workspace_mode,omitempty"`
		PrevFailure   *prevFailure `json:"previous_failure,omitempty"`
	}
	out := compactPayload{
		JobID:         job.ID,
		WorkspaceDir:  job.WorkspaceDir,
		WorkspaceMode: job.WorkspaceMode,
	}
	// Surface the most recent failed/blocked step so the executor can learn
	// from the previous attempt without receiving the full step history.
	for i := len(job.Steps) - 1; i >= 0; i-- {
		s := job.Steps[i]
		if s.Status == domain.StepStatusFailed || s.Status == domain.StepStatusBlocked {
			reason := firstNonEmpty(s.ErrorReason, s.BlockedReason, s.Summary)
			if reason != "" {
				out.PrevFailure = &prevFailure{
					StepIndex: s.Index,
					TaskType:  s.TaskType,
					Reason:    reason,
				}
			}
			break
		}
	}
	raw, _ := json.MarshalIndent(out, "", "  ")
	return string(raw)
}

// buildCompactReviewerPayload returns a focused job context for reviewer workers.
// Includes goal, step statuses, and diff summaries so the reviewer has the
// evidence it needs without the full job JSON.
func buildCompactReviewerPayload(job domain.Job, task domain.LeaderOutput) string {
	type stepEvidence struct {
		Index    int    `json:"index"`
		TaskType string `json:"task_type"`
		Status   string `json:"status"`
		Summary  string `json:"summary,omitempty"`
		Diff     string `json:"diff_summary,omitempty"`
	}
	type compactPayload struct {
		JobID        string         `json:"job_id,omitempty"`
		Goal         string         `json:"goal"`
		WorkspaceDir string         `json:"workspace_dir,omitempty"`
		Steps        []stepEvidence `json:"steps"`
	}
	steps := make([]stepEvidence, 0, len(job.Steps))
	for _, s := range job.Steps {
		summary := s.Summary
		if len([]rune(summary)) > 120 {
			summary = string([]rune(summary)[:120]) + "..."
		}
		steps = append(steps, stepEvidence{
			Index:    s.Index,
			TaskType: s.TaskType,
			Status:   string(s.Status),
			Summary:  summary,
			Diff:     s.DiffSummary,
		})
	}
	out := compactPayload{
		JobID:        job.ID,
		Goal:         job.Goal,
		WorkspaceDir: job.WorkspaceDir,
		Steps:        steps,
	}
	raw, _ := json.MarshalIndent(out, "", "  ")
	return string(raw)
}

// buildCompactEvaluatorPayload returns a compact job state for the evaluator.
// Includes goal, status, role profiles, step summaries, automated check
// results, and per-step changed file lists. Omits events, planning artifacts,
// and raw step task texts to reduce prompt size.
// Role profiles are included so the evaluator knows which models ran each role.
func buildCompactEvaluatorPayload(job domain.Job) string {
	type stepEvidence struct {
		Index        int                        `json:"index"`
		TaskType     string                     `json:"task_type"`
		Status       string                     `json:"status"`
		Summary      string                     `json:"summary,omitempty"`
		DiffSummary  string                     `json:"diff_summary,omitempty"`
		ErrorReason  string                     `json:"error_reason,omitempty"`
		Artifacts    []string                   `json:"artifacts,omitempty"`
		ChangedFiles []domain.ChangedFile       `json:"changed_files,omitempty"`
	}
	type compactPayload struct {
		JobID                string                       `json:"job_id,omitempty"`
		Goal                 string                       `json:"goal"`
		Status               string                       `json:"status"`
		CurrentStep          int                          `json:"current_step"`
		Summary              string                       `json:"summary,omitempty"`
		RoleProfiles         domain.RoleProfiles          `json:"role_profiles"`
		Steps                []stepEvidence               `json:"steps"`
		AutomatedCheckResults []domain.AutomatedCheckResult `json:"automated_check_results,omitempty"`
		ChangedFiles         []domain.ChangedFile          `json:"changed_files,omitempty"`
	}
	steps := make([]stepEvidence, 0, len(job.Steps))
	// Accumulate all changed files across steps for the top-level summary.
	var allChangedFiles []domain.ChangedFile
	for _, s := range job.Steps {
		summary := s.Summary
		if len([]rune(summary)) > 500 {
			summary = string([]rune(summary)[:500]) + "..."
		}
		steps = append(steps, stepEvidence{
			Index:        s.Index,
			TaskType:     s.TaskType,
			Status:       string(s.Status),
			Summary:      summary,
			DiffSummary:  s.DiffSummary,
			ErrorReason:  s.ErrorReason,
			Artifacts:    s.Artifacts,
			ChangedFiles: s.ChangedFiles,
		})
		allChangedFiles = append(allChangedFiles, s.ChangedFiles...)
	}
	out := compactPayload{
		JobID:                 job.ID,
		Goal:                  job.Goal,
		Status:                string(job.Status),
		CurrentStep:           job.CurrentStep,
		Summary:               job.Summary,
		RoleProfiles:          job.RoleProfiles,
		Steps:                 steps,
		AutomatedCheckResults: job.PreCheckResults,
		ChangedFiles:          allChangedFiles,
	}
	raw, _ := json.MarshalIndent(out, "", "  ")
	return string(raw)
}

type workerTaskContext struct {
	Objective     string
	Why           string
	ScopeBoundary string
}

func formatPromptList(values []string, empty string) string {
	if len(values) == 0 {
		return empty
	}
	var b strings.Builder
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("- ")
		b.WriteString(value)
	}
	if b.Len() == 0 {
		return empty
	}
	return b.String()
}

func parseWorkerTaskContext(taskText, fallbackWhy string) workerTaskContext {
	ctx := workerTaskContext{
		Objective: strings.TrimSpace(taskText),
	}
	sections := map[string][]string{
		"objective":      {},
		"why":            {},
		"scope_boundary": {},
	}
	current := "objective"
	seenStructured := false
	for _, line := range strings.Split(taskText, "\n") {
		if section, value, ok := parseTaskSectionHeader(line); ok {
			current = section
			seenStructured = true
			if value != "" {
				sections[current] = append(sections[current], value)
			}
			continue
		}
		sections[current] = append(sections[current], line)
	}
	if seenStructured {
		if objective := strings.TrimSpace(strings.Join(sections["objective"], "\n")); objective != "" {
			ctx.Objective = objective
		}
		ctx.Why = strings.TrimSpace(strings.Join(sections["why"], "\n"))
		ctx.ScopeBoundary = strings.TrimSpace(strings.Join(sections["scope_boundary"], "\n"))
	}
	if ctx.Objective == "" {
		ctx.Objective = strings.TrimSpace(taskText)
	}
	if ctx.Why == "" {
		ctx.Why = strings.TrimSpace(fallbackWhy)
	}
	if ctx.Why == "" {
		ctx.Why = "Not provided."
	}
	if ctx.ScopeBoundary == "" {
		ctx.ScopeBoundary = "Only perform the assigned task and stay within the stated file, workspace, and contract limits."
	}
	return ctx
}

func parseTaskSectionHeader(line string) (string, string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return "", "", false
	}
	content := strings.TrimLeft(trimmed, "-*# ")
	normalized := strings.ToLower(content)
	for _, candidate := range []struct {
		labels  []string
		section string
	}{
		{labels: []string{"task why", "task_why", "why"}, section: "why"},
		{labels: []string{"scope boundary", "scope_boundary", "scope"}, section: "scope_boundary"},
		{labels: []string{"objective", "task", "assigned task"}, section: "objective"},
	} {
		for _, label := range candidate.labels {
			if normalized == label {
				return candidate.section, "", true
			}
			prefix := label + ":"
			if strings.HasPrefix(normalized, prefix) {
				return candidate.section, strings.TrimSpace(content[len(prefix):]), true
			}
		}
	}
	return "", "", false
}

func buildWorkerPrompt(job domain.Job, task domain.LeaderOutput) string {
	taskPayload, _ := json.MarshalIndent(task, "", "  ")
	taskContext := parseWorkerTaskContext(task.TaskText, job.LeaderContextSummary)
	invariantsSection := formatPromptList(job.Constraints, "- None provided.")
	// schemaRetrySection is injected when a previous attempt produced an
	// invalid response so the model knows exactly what to correct.
	schemaRetrySection := ""
	if strings.TrimSpace(job.SchemaRetryHint) != "" {
		schemaRetrySection = fmt.Sprintf("\nCORRECTION REQUIRED: Your previous response failed schema validation: %s\nRespond with valid JSON matching the required schema.\n", job.SchemaRetryHint)
	}
	// RoleReviewer case removed: review/audit task_types now route to executor.
	// The evaluator performs code verification instead of a separate reviewer worker.
	executorBase := strings.TrimSpace(fmt.Sprintf(`
TASK: You are an executor worker assigned by the director. You perform the task described below. Report results accurately including files changed, commands run, and any errors encountered.
The assigned task below is complete and ready to execute. Do it now -- do not ask for input.
Output only a JSON object matching the schema. No conversation, no preamble.
status MUST be one of: success, failed, blocked.
%s
Overall job goal: %s

Task objective:
%s

Task why:
%s

Invariants to preserve:
%s

Scope boundary:
%s

Autonomy guidance:
%s

Assigned task payload:
%s

File management rules:
- Only create or modify files specified in your task
- Do NOT delete files you did not create
- Do NOT modify files outside the workspace directory
- If working in parallel with another worker, only touch your assigned files
- Use actual shell commands to create files (e.g. echo, touch, write commands)

Job state:
%s
`, schemaRetrySection, job.Goal, taskContext.Objective, taskContext.Why, invariantsSection, taskContext.ScopeBoundary, ambitionInstruction(job.AmbitionLevel, job.AmbitionText), string(taskPayload), buildCompactExecutorPayload(job, task)))
	return applyPromptOverrides(executorBase, "executor", job.WorkspaceDir, job.PromptOverrides)
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
	dir, err := os.MkdirTemp(base, "gorchera-provider-*")
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

// loadPromptOverride reads .gorchera/prompts/<role>.md from workspaceDir.
// Returns (content, isReplace, error).
// isReplace is true when the first line of the file is exactly "# REPLACE"
// (the marker line itself is stripped from content).
// If the file does not exist or workspaceDir is empty, returns ("", false, nil).
// Read errors are treated as "no override" (logged by caller if needed).
func loadPromptOverride(workspaceDir, role string) (string, bool, error) {
	if strings.TrimSpace(workspaceDir) == "" {
		return "", false, nil
	}
	path := filepath.Join(workspaceDir, ".gorchera", "prompts", role+".md")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		// Unexpected error -- surface it so the caller can log it.
		return "", false, err
	}
	content := string(data)
	firstLine, rest, _ := strings.Cut(content, "\n")
	if strings.TrimSpace(firstLine) == "# REPLACE" {
		// Strip the marker line; trim leading newline left by Cut.
		return strings.TrimLeft(rest, "\n"), true, nil
	}
	return content, false, nil
}

// applyPromptOverrides applies workspace-file and job-parameter overrides to
// a base prompt string and returns the final prompt.
//
// Priority (highest to lowest):
//   1. jobOverrides[role] -- always prepended on top of whatever base is used
//   2. workspace file    -- may replace or prepend onto the hardcoded base
//   3. base             -- the hardcoded prompt from the build* functions
//
// The "replace" mode from workspace files replaces the hardcoded base, but job
// overrides are still prepended on top of the replacement.  Job overrides never
// support replace (too dangerous from a remote call).
func applyPromptOverrides(base, role, workspaceDir string, jobOverrides map[string]string) string {
	// Step 1: apply workspace file override.
	wsContent, isReplace, err := loadPromptOverride(workspaceDir, role)
	if err != nil {
		// Log but continue -- do not fail the job over a missing or unreadable file.
		log.Printf("[gorchera] prompt override load error for role %s: %v (using base prompt)", role, err)
	}

	result := base
	if wsContent != "" {
		if isReplace {
			// Workspace file replaces the hardcoded base entirely.
			result = strings.TrimSpace(wsContent)
		} else {
			// Workspace file is prepended before the base prompt.
			result = strings.TrimSpace(wsContent) + "\n\n" + base
		}
	}

	// Step 2: prepend job-level override on top of whatever base we settled on.
	if jobOverrides != nil {
		if fragment, ok := jobOverrides[role]; ok && strings.TrimSpace(fragment) != "" {
			result = strings.TrimSpace(fragment) + "\n\n" + result
		}
	}

	return result
}
