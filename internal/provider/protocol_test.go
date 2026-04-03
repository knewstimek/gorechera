package provider

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gorchera/internal/domain"
)

// writePromptFile creates the .gorchera/prompts/ directory and writes content
// to <role>.md inside workspaceDir. Returns the file path written.
func writePromptFile(t *testing.T, workspaceDir, role, content string) string {
	t.Helper()
	dir := filepath.Join(workspaceDir, ".gorchera", "prompts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, role+".md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// --- loadPromptOverride unit tests ---

func TestLoadPromptOverride_NotExist(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	content, isReplace, err := loadPromptOverride(dir, "executor")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if content != "" || isReplace {
		t.Fatalf("expected empty content and isReplace=false, got %q %v", content, isReplace)
	}
}

func TestLoadPromptOverride_EmptyWorkspaceDir(t *testing.T) {
	t.Parallel()
	content, isReplace, err := loadPromptOverride("", "executor")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if content != "" || isReplace {
		t.Fatalf("expected empty, got %q %v", content, isReplace)
	}
}

func TestLoadPromptOverride_PrependMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePromptFile(t, dir, "executor", "Custom executor instruction.\nSecond line.")

	content, isReplace, err := loadPromptOverride(dir, "executor")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if isReplace {
		t.Fatal("expected isReplace=false for file without # REPLACE marker")
	}
	if !strings.Contains(content, "Custom executor instruction.") {
		t.Fatalf("content missing expected text, got: %q", content)
	}
}

func TestLoadPromptOverride_ReplaceMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePromptFile(t, dir, "evaluator", "# REPLACE\nReplacement evaluator body.")

	content, isReplace, err := loadPromptOverride(dir, "evaluator")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isReplace {
		t.Fatal("expected isReplace=true for file starting with # REPLACE")
	}
	// Marker line must be stripped.
	if strings.Contains(content, "# REPLACE") {
		t.Fatalf("# REPLACE marker must be stripped, got: %q", content)
	}
	if !strings.Contains(content, "Replacement evaluator body.") {
		t.Fatalf("expected body text, got: %q", content)
	}
}

// --- applyPromptOverrides unit tests ---

func TestApplyPromptOverrides_NoOverride(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base := "Base prompt."
	result := applyPromptOverrides(base, "executor", dir, nil)
	if result != base {
		t.Fatalf("expected base unchanged, got %q", result)
	}
}

func TestApplyPromptOverrides_WorkspacePrepend(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePromptFile(t, dir, "executor", "Workspace prepend.")

	base := "Base prompt."
	result := applyPromptOverrides(base, "executor", dir, nil)

	if !strings.HasPrefix(result, "Workspace prepend.") {
		t.Fatalf("workspace content should come first, got: %q", result)
	}
	if !strings.Contains(result, "Base prompt.") {
		t.Fatalf("base prompt must be retained, got: %q", result)
	}
}

func TestApplyPromptOverrides_WorkspaceReplace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePromptFile(t, dir, "evaluator", "# REPLACE\nReplaced body.")

	base := "Original base."
	result := applyPromptOverrides(base, "evaluator", dir, nil)

	if strings.Contains(result, "Original base.") {
		t.Fatalf("base should be replaced, got: %q", result)
	}
	if !strings.Contains(result, "Replaced body.") {
		t.Fatalf("replacement body missing, got: %q", result)
	}
}

func TestApplyPromptOverrides_JobParamPrepend(t *testing.T) {
	t.Parallel()
	dir := t.TempDir() // no workspace file
	base := "Base prompt."
	jobOverrides := map[string]string{"executor": "Job override fragment."}

	result := applyPromptOverrides(base, "executor", dir, jobOverrides)

	if !strings.HasPrefix(result, "Job override fragment.") {
		t.Fatalf("job override should come first, got: %q", result)
	}
	if !strings.Contains(result, "Base prompt.") {
		t.Fatalf("base prompt must be retained, got: %q", result)
	}
}

func TestApplyPromptOverrides_JobParamAndWorkspaceFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePromptFile(t, dir, "executor", "Workspace fragment.")

	base := "Base prompt."
	jobOverrides := map[string]string{"executor": "Job fragment."}

	result := applyPromptOverrides(base, "executor", dir, jobOverrides)

	// Order must be: job override > workspace file > base
	jobIdx := strings.Index(result, "Job fragment.")
	wsIdx := strings.Index(result, "Workspace fragment.")
	baseIdx := strings.Index(result, "Base prompt.")
	if jobIdx < 0 || wsIdx < 0 || baseIdx < 0 {
		t.Fatalf("expected all three sections in result, got: %q", result)
	}
	if !(jobIdx < wsIdx && wsIdx < baseIdx) {
		t.Fatalf("order wrong: job=%d ws=%d base=%d in %q", jobIdx, wsIdx, baseIdx, result)
	}
}

func TestApplyPromptOverrides_JobParamOnTopOfReplace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePromptFile(t, dir, "evaluator", "# REPLACE\nReplaced body.")

	base := "Original base."
	jobOverrides := map[string]string{"evaluator": "Job override."}

	result := applyPromptOverrides(base, "evaluator", dir, jobOverrides)

	// Workspace replaces base; job override still prepends on top.
	if strings.Contains(result, "Original base.") {
		t.Fatalf("original base should be gone after replace, got: %q", result)
	}
	if !strings.HasPrefix(result, "Job override.") {
		t.Fatalf("job override should be at front, got: %q", result)
	}
	if !strings.Contains(result, "Replaced body.") {
		t.Fatalf("replaced body should be present, got: %q", result)
	}
}

func TestApplyPromptOverrides_UnrelatedRoleIgnored(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePromptFile(t, dir, "reviewer", "Reviewer workspace fragment.")

	base := "Base prompt."
	// executor override should not affect the reviewer file
	jobOverrides := map[string]string{"executor": "Executor job fragment."}

	result := applyPromptOverrides(base, "reviewer", dir, jobOverrides)

	// reviewer workspace file should be prepended
	if !strings.Contains(result, "Reviewer workspace fragment.") {
		t.Fatalf("reviewer workspace file should apply, got: %q", result)
	}
	// executor job override must NOT appear in reviewer result
	if strings.Contains(result, "Executor job fragment.") {
		t.Fatalf("executor override must not bleed into reviewer, got: %q", result)
	}
}

// --- Integration: prompt build functions respect overrides ---

func minimalJob(workspaceDir string, overrides map[string]string) domain.Job {
	return domain.Job{
		Goal:            "test goal",
		WorkspaceDir:    workspaceDir,
		PromptOverrides: overrides,
	}
}

func TestBuildPlannerPrompt_AppliesDirectorOverride(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePromptFile(t, dir, "director", "Custom director instruction.")

	job := minimalJob(dir, nil)
	result := buildPlannerPrompt(job)

	if !strings.Contains(result, "Custom director instruction.") {
		t.Fatalf("director override not applied to planner prompt, got: %q", result[:min(200, len(result))])
	}
}

func TestBuildLeaderPrompt_AppliesDirectorOverride(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	jobOverrides := map[string]string{"director": "Leader job directive."}

	job := minimalJob(dir, jobOverrides)
	result := buildLeaderPrompt(job)

	if !strings.HasPrefix(result, "Leader job directive.") {
		t.Fatalf("director job override not at front of leader prompt, got: %q", result[:min(200, len(result))])
	}
}

func TestBuildEvaluatorPrompt_AppliesEvaluatorOverride(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	jobOverrides := map[string]string{"evaluator": "Reject if coverage below 80%."}

	job := minimalJob(dir, jobOverrides)
	result := buildEvaluatorPrompt(job)

	if !strings.HasPrefix(result, "Reject if coverage below 80%.") {
		t.Fatalf("evaluator job override not at front, got: %q", result[:min(200, len(result))])
	}
}

func TestBuildWorkerPrompt_AppliesExecutorOverride(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	jobOverrides := map[string]string{"executor": "Always write tests first."}

	job := minimalJob(dir, jobOverrides)
	task := domain.LeaderOutput{
		Action:   "run_worker",
		Target:   "B",
		TaskType: "implement",
		TaskText: "Implement feature X.",
	}
	result := buildWorkerPrompt(job, task)

	if !strings.HasPrefix(result, "Always write tests first.") {
		t.Fatalf("executor job override not at front, got: %q", result[:min(200, len(result))])
	}
}

func TestBuildWorkerPrompt_AppliesReviewerOverride(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	jobOverrides := map[string]string{"reviewer": "Focus on concurrency bugs."}

	job := minimalJob(dir, jobOverrides)
	task := domain.LeaderOutput{
		Action:   "run_worker",
		Target:   "B",
		TaskType: "review",
		TaskText: "Review the change.",
	}
	result := buildWorkerPrompt(job, task)

	if !strings.HasPrefix(result, "Focus on concurrency bugs.") {
		t.Fatalf("reviewer job override not at front, got: %q", result[:min(200, len(result))])
	}
}

// min is a trivial helper for safe substring preview in test error messages.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
