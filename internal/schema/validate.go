package schema

import (
	"fmt"
	"strings"

	"gorchera/internal/domain"
)

var leaderActions = map[string]struct{}{
	"run_worker":  {},
	"run_workers": {},
	"run_system":  {},
	"summarize":   {},
	"complete":    {},
	"fail":        {},
	"blocked":     {},
}

var leaderTargets = map[string]struct{}{
	"B":    {},
	"C":    {},
	"D":    {},
	"SYS":  {},
	"none": {},
}

var leaderTaskTypes = map[string]struct{}{
	"implement": {},
	"test":      {},
	"search":    {},
	"build":     {},
	"lint":      {},
	"command":   {},
	"summarize": {},
	"none":      {},
}

var workerTargets = map[string]struct{}{
	"B": {},
	"C": {},
	"D": {},
}

var workerTaskTypes = map[string]struct{}{
	"implement": {},
	"review":    {},
	"audit":     {},
	"test":      {},
	"search":    {},
	"build":     {},
	"lint":      {},
	"command":   {},
}

var workerStatuses = map[string]struct{}{
	"success": {},
	"failed":  {},
	"blocked": {},
}

var verificationStatuses = map[string]struct{}{
	"passed":  {},
	"blocked": {},
	"failed":  {},
}

func ValidateLeaderOutput(msg *domain.LeaderOutput) error {
	if _, ok := leaderActions[msg.Action]; !ok {
		return fmt.Errorf("invalid action: %q", msg.Action)
	}
	if msg.Action == "run_worker" {
		if _, ok := leaderTargets[msg.Target]; !ok || msg.Target == "none" {
			return fmt.Errorf("run_worker requires a valid target")
		}
		if _, ok := leaderTaskTypes[msg.TaskType]; !ok || msg.TaskType == "none" {
			return fmt.Errorf("run_worker requires a valid task_type")
		}
		if strings.TrimSpace(msg.TaskText) == "" {
			return fmt.Errorf("run_worker requires task_text")
		}
	}
	if msg.Action == "run_workers" {
		if len(msg.Tasks) != 2 {
			return fmt.Errorf("run_workers requires exactly 2 tasks")
		}
		seenTargets := make(map[string]struct{}, len(msg.Tasks))
		for _, task := range msg.Tasks {
			if _, ok := workerTargets[task.Target]; !ok {
				return fmt.Errorf("run_workers requires valid worker targets")
			}
			if _, ok := seenTargets[task.Target]; ok {
				return fmt.Errorf("run_workers requires distinct worker targets")
			}
			seenTargets[task.Target] = struct{}{}
			if _, ok := workerTaskTypes[task.TaskType]; !ok {
				return fmt.Errorf("run_workers requires valid worker task_type")
			}
			if strings.TrimSpace(task.TaskText) == "" {
				return fmt.Errorf("run_workers requires task_text")
			}
		}
	}
	if msg.Action == "run_system" {
		if msg.Target != "SYS" {
			return fmt.Errorf("run_system requires SYS target")
		}
		if _, ok := leaderTaskTypes[msg.TaskType]; !ok || msg.TaskType == "none" || msg.TaskType == "summarize" || msg.TaskType == "implement" || msg.TaskType == "review" || msg.TaskType == "test" {
			return fmt.Errorf("run_system requires a system task_type")
		}
		if msg.SystemAction == nil {
			return fmt.Errorf("run_system requires system_action")
		}
		if strings.TrimSpace(string(msg.SystemAction.Type)) == "" {
			return fmt.Errorf("run_system requires system_action.type")
		}
		if strings.TrimSpace(msg.SystemAction.Command) == "" {
			return fmt.Errorf("run_system requires system_action.command")
		}
	}
	// Non-run_system actions: clear stale system_action the model may have sent
	// even though it was not requested (anyOf null schema allows null, but models
	// sometimes echo a populated object anyway).
	if msg.Action != "run_system" && msg.SystemAction != nil {
		msg.SystemAction = nil
	}
	if msg.Action == "complete" || msg.Action == "fail" || msg.Action == "blocked" {
		if strings.TrimSpace(msg.Reason) == "" {
			return fmt.Errorf("%s requires reason", msg.Action)
		}
	}
	return nil
}

func ValidateWorkerOutput(msg domain.WorkerOutput) error {
	if _, ok := workerStatuses[msg.Status]; !ok {
		return fmt.Errorf("invalid status: %q", msg.Status)
	}
	if strings.TrimSpace(msg.Summary) == "" {
		return fmt.Errorf("summary is required")
	}
	if msg.Status == "blocked" && strings.TrimSpace(msg.BlockedReason) == "" {
		return fmt.Errorf("blocked requires blocked_reason")
	}
	if msg.Status == "failed" && strings.TrimSpace(msg.ErrorReason) == "" {
		return fmt.Errorf("failed requires error_reason")
	}
	return nil
}

func ValidateVerificationContract(msg domain.VerificationContract) error {
	if msg.Version <= 0 {
		return fmt.Errorf("version must be positive")
	}
	if strings.TrimSpace(msg.Goal) == "" {
		return fmt.Errorf("goal is required")
	}
	if len(msg.Scope) == 0 {
		return fmt.Errorf("scope is required")
	}
	if len(msg.RequiredChecks) == 0 {
		return fmt.Errorf("required_checks is required")
	}
	if len(msg.RequiredCommands) == 0 {
		return fmt.Errorf("required_commands is required")
	}
	return nil
}

func ValidateVerificationReport(msg domain.VerificationReport) error {
	if _, ok := verificationStatuses[msg.Status]; !ok {
		return fmt.Errorf("invalid verification status: %q", msg.Status)
	}
	if msg.Status == "passed" && !msg.Passed {
		return fmt.Errorf("passed status requires passed=true")
	}
	if msg.Status != "passed" && msg.Passed {
		return fmt.Errorf("non-passed status requires passed=false")
	}
	if strings.TrimSpace(msg.Reason) == "" {
		return fmt.Errorf("reason is required")
	}
	if len(msg.Evidence) == 0 {
		return fmt.Errorf("evidence is required")
	}
	return nil
}
