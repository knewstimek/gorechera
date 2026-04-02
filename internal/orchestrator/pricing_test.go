package orchestrator

import (
	"math"
	"testing"

	"gorchera/internal/domain"
)

func TestEstimateProviderUsageUsesModelAwarePricing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		job      domain.Job
		role     domain.RoleName
		input    string
		output   string
		wantCost float64
	}{
		{
			name: "claude alias opus maps to latest opus pricing",
			job: domain.Job{
				Provider: domain.ProviderClaude,
				RoleProfiles: domain.RoleProfiles{
					Leader: domain.ExecutionProfile{Provider: domain.ProviderClaude, Model: "opus"},
				},
			},
			role:     domain.RoleLeader,
			input:    "abcdabcd", // 2 tokens
			output:   "abcdefgh", // 2 tokens
			wantCost: (2*5.0 + 2*25.0) / 1_000_000,
		},
		{
			name: "codex alias sonnet maps to gpt-5.4-mini pricing",
			job: domain.Job{
				Provider: domain.ProviderCodex,
				RoleProfiles: domain.RoleProfiles{
					Executor: domain.ExecutionProfile{Provider: domain.ProviderCodex, Model: "sonnet"},
				},
			},
			role:     domain.RoleExecutor,
			input:    "abcdabcd", // 2 tokens
			output:   "abcdefgh", // 2 tokens
			wantCost: (2*0.75 + 2*4.5) / 1_000_000,
		},
		{
			name: "explicit gpt-5.3-codex uses official pricing",
			job: domain.Job{
				Provider: domain.ProviderCodex,
				RoleProfiles: domain.RoleProfiles{
					Leader: domain.ExecutionProfile{Provider: domain.ProviderCodex, Model: "gpt-5.3-codex"},
				},
			},
			role:     domain.RoleLeader,
			input:    "abcdefghijkl", // 3 tokens
			output:   "abcdefghijkl", // 3 tokens
			wantCost: (3*1.75 + 3*14.0) / 1_000_000,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			usage := estimateProviderUsage(tc.job, tc.role, tc.output, tc.input)
			if math.Abs(usage.EstimatedCostUSD-tc.wantCost) > 1e-12 {
				t.Fatalf("estimated cost = %.12f, want %.12f", usage.EstimatedCostUSD, tc.wantCost)
			}
		})
	}
}

func TestNormalizePricingModelFallsBackByRoleTier(t *testing.T) {
	t.Parallel()

	if got := normalizePricingModel(domain.ProviderCodex, "", domain.RoleLeader); got != "gpt-5.4" {
		t.Fatalf("leader codex fallback = %q, want gpt-5.4", got)
	}
	if got := normalizePricingModel(domain.ProviderCodex, "", domain.RoleExecutor); got != "gpt-5.4-mini" {
		t.Fatalf("executor codex fallback = %q, want gpt-5.4-mini", got)
	}
	if got := normalizePricingModel(domain.ProviderClaude, "", domain.RoleReviewer); got != "claude-sonnet-4.6" {
		t.Fatalf("reviewer claude fallback = %q, want claude-sonnet-4.6", got)
	}
}
