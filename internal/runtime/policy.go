package runtime

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

type Policy struct {
	rules map[Category]map[string]struct{}
}

func NewDefaultPolicy() *Policy {
	return &Policy{
		rules: map[Category]map[string]struct{}{
			CategoryBuild:   newRuleSet("go", "cargo", "dotnet", "make", "cmake", "mvn", "gradle", "npm", "pnpm", "yarn"),
			CategoryTest:    newRuleSet("go", "cargo", "dotnet", "pytest", "npm", "pnpm", "yarn", "python"),
			CategoryLint:    newRuleSet("go", "golangci-lint", "eslint", "prettier", "ruff", "black", "mypy", "flake8", "cargo", "dotnet"),
			CategorySearch:  newRuleSet("rg", "grep", "git", "findstr", "select-string"),
			// rg is intentionally absent from CategoryCommand: it can traverse the
			// full filesystem without a workspace boundary. Use CategorySearch instead.
			CategoryCommand: newRuleSet("go"),
		},
	}
}

func newRuleSet(values ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[normalizeExecutable(value)] = struct{}{}
	}
	return out
}

func (p *Policy) Allows(req Request) error {
	if p == nil {
		return fmt.Errorf("policy is nil")
	}
	if strings.TrimSpace(req.Command) == "" {
		return fmt.Errorf("command is required")
	}
	rules, ok := p.rules[req.Category]
	if !ok {
		return fmt.Errorf("unsupported category: %s", req.Category)
	}
	exe := normalizeExecutable(req.Command)
	if _, ok := rules[exe]; !ok {
		return fmt.Errorf("command %q is not allowlisted for category %s", exe, req.Category)
	}
	return nil
}

func (p *Policy) Allow(category Category, command string) {
	if p.rules == nil {
		p.rules = make(map[Category]map[string]struct{})
	}
	rules, ok := p.rules[category]
	if !ok {
		rules = make(map[string]struct{})
		p.rules[category] = rules
	}
	rules[normalizeExecutable(command)] = struct{}{}
}

func normalizeExecutable(command string) string {
	name := strings.ToLower(filepath.Base(command))
	switch {
	case strings.HasSuffix(name, ".exe"):
		name = strings.TrimSuffix(name, ".exe")
	case strings.HasSuffix(name, ".cmd"):
		name = strings.TrimSuffix(name, ".cmd")
	case strings.HasSuffix(name, ".bat"):
		name = strings.TrimSuffix(name, ".bat")
	}
	if runtime.GOOS == "windows" {
		name = strings.ReplaceAll(name, " ", "")
	}
	return name
}
