package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gorchera/internal/domain"
)

// runAutomatedChecks executes all automated checks defined in the verification
// contract and returns their results. steps is used by checks that inspect
// the per-step ChangedFiles list (file_unchanged, no_new_deps).
func runAutomatedChecks(workspaceDir string, checks []domain.AutomatedCheck, steps []domain.Step) []domain.AutomatedCheckResult {
	results := make([]domain.AutomatedCheckResult, 0, len(checks))
	for _, check := range checks {
		results = append(results, executeCheck(workspaceDir, check, steps))
	}
	return results
}

func executeCheck(workspaceDir string, check domain.AutomatedCheck, steps []domain.Step) domain.AutomatedCheckResult {
	switch check.Type {
	case "grep":
		return runGrepCheck(workspaceDir, check)
	case "file_exists":
		return runFileExistsCheck(workspaceDir, check)
	case "file_unchanged":
		return runFileUnchangedCheck(check, steps)
	case "no_new_deps":
		return runNoNewDepsCheck(check, steps)
	default:
		return domain.AutomatedCheckResult{
			Description: check.Description,
			Status:      "skipped",
			Detail:      "unknown check type: " + check.Type,
		}
	}
}

// runGrepCheck verifies that check.Pattern matches at least one line in the
// files matched by check.File (a glob relative to workspaceDir). Passes when
// any match is found; fails when no files match the glob or no line matches
// the pattern.
func runGrepCheck(workspaceDir string, check domain.AutomatedCheck) domain.AutomatedCheckResult {
	if check.Pattern == "" {
		return domain.AutomatedCheckResult{
			Description: check.Description,
			Status:      "skipped",
			Detail:      "grep check missing pattern",
		}
	}
	re, err := regexp.Compile(check.Pattern)
	if err != nil {
		return domain.AutomatedCheckResult{
			Description: check.Description,
			Status:      "skipped",
			Detail:      fmt.Sprintf("invalid pattern: %v", err),
		}
	}

	globPattern := check.File
	if globPattern == "" {
		globPattern = "*"
	}
	// Resolve the glob relative to workspaceDir.
	matches, err := filepath.Glob(filepath.Join(workspaceDir, globPattern))
	if err != nil || len(matches) == 0 {
		return domain.AutomatedCheckResult{
			Description: check.Description,
			Status:      "failed",
			Detail:      fmt.Sprintf("no files matched glob %q", globPattern),
		}
	}

	for _, path := range matches {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				rel, _ := filepath.Rel(workspaceDir, path)
				return domain.AutomatedCheckResult{
					Description: check.Description,
					Status:      "passed",
					Detail:      fmt.Sprintf("pattern matched in %s", filepath.ToSlash(rel)),
				}
			}
		}
	}
	return domain.AutomatedCheckResult{
		Description: check.Description,
		Status:      "failed",
		Detail:      fmt.Sprintf("pattern %q not found in %d file(s)", check.Pattern, len(matches)),
	}
}

// runFileExistsCheck verifies that check.Path exists in the workspace.
func runFileExistsCheck(workspaceDir string, check domain.AutomatedCheck) domain.AutomatedCheckResult {
	if check.Path == "" {
		return domain.AutomatedCheckResult{
			Description: check.Description,
			Status:      "skipped",
			Detail:      "file_exists check missing path",
		}
	}
	full := filepath.Join(workspaceDir, check.Path)
	if _, err := os.Stat(full); err == nil {
		return domain.AutomatedCheckResult{
			Description: check.Description,
			Status:      "passed",
			Detail:      fmt.Sprintf("%s exists", check.Path),
		}
	}
	return domain.AutomatedCheckResult{
		Description: check.Description,
		Status:      "failed",
		Detail:      fmt.Sprintf("%s does not exist", check.Path),
	}
}

// runFileUnchangedCheck verifies that check.Path does not appear in any
// step's ChangedFiles list. A file that was not touched passes this check.
func runFileUnchangedCheck(check domain.AutomatedCheck, steps []domain.Step) domain.AutomatedCheckResult {
	if check.Path == "" {
		return domain.AutomatedCheckResult{
			Description: check.Description,
			Status:      "skipped",
			Detail:      "file_unchanged check missing path",
		}
	}
	target := filepath.ToSlash(check.Path)
	for _, s := range steps {
		for _, cf := range s.ChangedFiles {
			if filepath.ToSlash(cf.Path) == target {
				return domain.AutomatedCheckResult{
					Description: check.Description,
					Status:      "failed",
					Detail:      fmt.Sprintf("%s was %s in step %d", check.Path, cf.Action, s.Index),
				}
			}
		}
	}
	return domain.AutomatedCheckResult{
		Description: check.Description,
		Status:      "passed",
		Detail:      fmt.Sprintf("%s was not modified", check.Path),
	}
}

// runNoNewDepsCheck verifies that go.mod was not modified in any step.
// A modified go.mod indicates new external dependencies were added.
func runNoNewDepsCheck(check domain.AutomatedCheck, steps []domain.Step) domain.AutomatedCheckResult {
	for _, s := range steps {
		for _, cf := range s.ChangedFiles {
			if filepath.Base(filepath.ToSlash(cf.Path)) == "go.mod" {
				return domain.AutomatedCheckResult{
					Description: check.Description,
					Status:      "failed",
					Detail:      fmt.Sprintf("go.mod was %s in step %d", cf.Action, s.Index),
				}
			}
		}
	}
	return domain.AutomatedCheckResult{
		Description: check.Description,
		Status:      "passed",
		Detail:      "go.mod was not modified",
	}
}
