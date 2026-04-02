package orchestrator

import (
	"strings"

	"gorchera/internal/domain"
)

// Pricing table last updated from official sources on 2026-04-02:
// - OpenAI API pricing: https://openai.com/api/pricing/
// - OpenAI model pages on developers.openai.com for GPT-5, GPT-5.1, GPT-5.2,
//   GPT-5.3-Codex, GPT-5-Codex, GPT-5 mini, and GPT-5 nano
// - Anthropic pricing: https://platform.claude.com/docs/en/about-claude/pricing
//
// Costs are stored per 1M tokens and applied separately to estimated input and
// estimated output token counts. Prompt-caching, batch, regional uplifts, and
// tool-specific surcharges are intentionally excluded from this estimate.

type modelPricing struct {
	inputUSDPerMTok  float64
	outputUSDPerMTok float64
}

var (
	openAIModelPricing = map[string]modelPricing{
		"gpt-5.4":             {inputUSDPerMTok: 2.50, outputUSDPerMTok: 15.00},
		"gpt-5.4-mini":        {inputUSDPerMTok: 0.75, outputUSDPerMTok: 4.50},
		"gpt-5.4-nano":        {inputUSDPerMTok: 0.20, outputUSDPerMTok: 1.25},
		"gpt-5.4-pro":         {inputUSDPerMTok: 30.00, outputUSDPerMTok: 180.00},
		"gpt-5.3-codex":       {inputUSDPerMTok: 1.75, outputUSDPerMTok: 14.00},
		"gpt-5.3-chat":        {inputUSDPerMTok: 1.75, outputUSDPerMTok: 14.00},
		"gpt-5.3-chat-latest": {inputUSDPerMTok: 1.75, outputUSDPerMTok: 14.00},
		"gpt-5.2":             {inputUSDPerMTok: 1.75, outputUSDPerMTok: 14.00},
		"gpt-5.1":             {inputUSDPerMTok: 1.25, outputUSDPerMTok: 10.00},
		"gpt-5":               {inputUSDPerMTok: 1.25, outputUSDPerMTok: 10.00},
		"gpt-5-codex":         {inputUSDPerMTok: 1.25, outputUSDPerMTok: 10.00},
		"gpt-5-chat":          {inputUSDPerMTok: 1.25, outputUSDPerMTok: 10.00},
		"gpt-5-chat-latest":   {inputUSDPerMTok: 1.25, outputUSDPerMTok: 10.00},
		"gpt-5-mini":          {inputUSDPerMTok: 0.25, outputUSDPerMTok: 2.00},
		"gpt-5-nano":          {inputUSDPerMTok: 0.05, outputUSDPerMTok: 0.40},
	}

	anthropicModelPricing = map[string]modelPricing{
		"claude-opus-4.6":   {inputUSDPerMTok: 5.00, outputUSDPerMTok: 25.00},
		"claude-opus-4.5":   {inputUSDPerMTok: 5.00, outputUSDPerMTok: 25.00},
		"claude-opus-4.1":   {inputUSDPerMTok: 15.00, outputUSDPerMTok: 75.00},
		"claude-opus-4":     {inputUSDPerMTok: 15.00, outputUSDPerMTok: 75.00},
		"claude-sonnet-4.6": {inputUSDPerMTok: 3.00, outputUSDPerMTok: 15.00},
		"claude-sonnet-4.5": {inputUSDPerMTok: 3.00, outputUSDPerMTok: 15.00},
		"claude-sonnet-4":   {inputUSDPerMTok: 3.00, outputUSDPerMTok: 15.00},
		"claude-sonnet-3.7": {inputUSDPerMTok: 3.00, outputUSDPerMTok: 15.00},
		"claude-haiku-4.5":  {inputUSDPerMTok: 1.00, outputUSDPerMTok: 5.00},
		"claude-haiku-3.5":  {inputUSDPerMTok: 0.80, outputUSDPerMTok: 4.00},
		"claude-haiku-3":    {inputUSDPerMTok: 0.25, outputUSDPerMTok: 1.25},
	}
)

func estimateProviderUsage(job domain.Job, role domain.RoleName, output string, inputs ...any) domain.TokenUsage {
	input := buildTokenUsageInput(inputs...)
	inputTokens := estimateTokenCount(input)
	outputTokens := estimateTokenCount(output)
	pricing := resolvePricing(job, role)
	return domain.TokenUsage{
		InputTokens:      inputTokens,
		OutputTokens:     outputTokens,
		TotalTokens:      inputTokens + outputTokens,
		EstimatedCostUSD: estimateTokenCost(inputTokens, outputTokens, pricing),
	}
}

func resolvePricing(job domain.Job, role domain.RoleName) modelPricing {
	profile := job.RoleProfiles.ProfileFor(role, job.Provider)
	model := normalizePricingModel(job.Provider, profile.Model, role)
	switch job.Provider {
	case domain.ProviderClaude:
		if pricing, ok := anthropicModelPricing[model]; ok {
			return pricing
		}
	case domain.ProviderCodex:
		if pricing, ok := openAIModelPricing[model]; ok {
			return pricing
		}
	}
	return modelPricing{}
}

func normalizePricingModel(providerName domain.ProviderName, model string, role domain.RoleName) string {
	normalized := strings.ToLower(strings.TrimSpace(model))
	switch providerName {
	case domain.ProviderClaude:
		switch normalized {
		case "opus":
			return "claude-opus-4.6"
		case "sonnet":
			return "claude-sonnet-4.6"
		case "haiku":
			return "claude-haiku-4.5"
		}
		if strings.HasPrefix(normalized, "claude-") {
			return normalizeClaudeModelName(normalized)
		}
	case domain.ProviderCodex:
		switch normalized {
		case "opus":
			return "gpt-5.4"
		case "sonnet":
			return "gpt-5.4-mini"
		case "haiku":
			return "gpt-5.4-nano"
		}
		if strings.HasPrefix(normalized, "gpt-") {
			return normalizeOpenAIModelName(normalized)
		}
	}

	// If the provider is explicit but the model is missing or unrecognized,
	// fall back to the role tier instead of the previous near-zero flat guess.
	switch role {
	case domain.RolePlanner, domain.RoleLeader, domain.RoleEvaluator:
		if providerName == domain.ProviderClaude {
			return "claude-opus-4.6"
		}
		if providerName == domain.ProviderCodex {
			return "gpt-5.4"
		}
	default:
		if providerName == domain.ProviderClaude {
			return "claude-sonnet-4.6"
		}
		if providerName == domain.ProviderCodex {
			return "gpt-5.4-mini"
		}
	}
	return ""
}

func normalizeClaudeModelName(model string) string {
	switch {
	case strings.Contains(model, "opus-4.6"):
		return "claude-opus-4.6"
	case strings.Contains(model, "opus-4.5"):
		return "claude-opus-4.5"
	case strings.Contains(model, "opus-4.1"):
		return "claude-opus-4.1"
	case strings.Contains(model, "opus-4"):
		return "claude-opus-4"
	case strings.Contains(model, "sonnet-4.6"):
		return "claude-sonnet-4.6"
	case strings.Contains(model, "sonnet-4.5"):
		return "claude-sonnet-4.5"
	case strings.Contains(model, "sonnet-4"):
		return "claude-sonnet-4"
	case strings.Contains(model, "sonnet-3.7"):
		return "claude-sonnet-3.7"
	case strings.Contains(model, "haiku-4.5"):
		return "claude-haiku-4.5"
	case strings.Contains(model, "haiku-3.5"):
		return "claude-haiku-3.5"
	case strings.Contains(model, "haiku-3"):
		return "claude-haiku-3"
	default:
		return model
	}
}

func normalizeOpenAIModelName(model string) string {
	switch {
	case strings.HasPrefix(model, "gpt-5.4-pro"):
		return "gpt-5.4-pro"
	case strings.HasPrefix(model, "gpt-5.4-mini"):
		return "gpt-5.4-mini"
	case strings.HasPrefix(model, "gpt-5.4-nano"):
		return "gpt-5.4-nano"
	case strings.HasPrefix(model, "gpt-5.4"):
		return "gpt-5.4"
	case strings.HasPrefix(model, "gpt-5.3-codex"):
		return "gpt-5.3-codex"
	case strings.HasPrefix(model, "gpt-5.3-chat"):
		return "gpt-5.3-chat"
	case strings.HasPrefix(model, "gpt-5.2"):
		return "gpt-5.2"
	case strings.HasPrefix(model, "gpt-5.1"):
		return "gpt-5.1"
	case strings.HasPrefix(model, "gpt-5-codex"):
		return "gpt-5-codex"
	case strings.HasPrefix(model, "gpt-5-chat"):
		return "gpt-5-chat"
	case strings.HasPrefix(model, "gpt-5-mini"):
		return "gpt-5-mini"
	case strings.HasPrefix(model, "gpt-5-nano"):
		return "gpt-5-nano"
	case strings.HasPrefix(model, "gpt-5"):
		return "gpt-5"
	default:
		return model
	}
}

func estimateTokenCost(inputTokens, outputTokens int, pricing modelPricing) float64 {
	if inputTokens <= 0 && outputTokens <= 0 {
		return 0
	}
	return (float64(inputTokens) * pricing.inputUSDPerMTok / 1_000_000) +
		(float64(outputTokens) * pricing.outputUSDPerMTok / 1_000_000)
}
