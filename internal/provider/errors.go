package provider

import (
	"fmt"
	"strings"

	"gorechera/internal/domain"
)

type ProviderErrorKind string
type ErrorKind = ProviderErrorKind

type ProviderErrorAction string

const (
	ProviderErrorActionRetry ProviderErrorAction = "retry"
	ProviderErrorActionBlock ProviderErrorAction = "block"
	ProviderErrorActionFail  ProviderErrorAction = "fail"
)

const (
	ErrorKindMissingExecutable ErrorKind = "missing_executable"
	ErrorKindProbeFailed       ErrorKind = "probe_failed"
	ErrorKindCommandFailed     ErrorKind = "command_failed"
	ErrorKindInvalidResponse   ErrorKind = "invalid_response"
	ErrorKindUnsupportedPhase  ErrorKind = "unsupported_phase"
	ErrorKindAuthFailure       ErrorKind = "auth_failure"
	ErrorKindQuotaExceeded     ErrorKind = "quota_exceeded"
	ErrorKindRateLimited       ErrorKind = "rate_limited"
	ErrorKindBillingRequired   ErrorKind = "billing_required"
	ErrorKindSessionExpired    ErrorKind = "session_expired"
	ErrorKindNetworkError      ErrorKind = "network_error"
	ErrorKindTransportError    ErrorKind = "transport_error"
)

type ProviderError struct {
	Provider          domain.ProviderName
	Kind              ProviderErrorKind
	RecommendedAction ProviderErrorAction
	Executable        string
	Detail            string
	Err               error
}

func (e *ProviderError) Error() string {
	if e == nil {
		return "<nil>"
	}
	switch {
	case e.Executable != "" && e.Detail != "":
		return fmt.Sprintf("%s provider %s (%s): %s", e.Provider, e.Kind, e.Executable, e.Detail)
	case e.Executable != "":
		return fmt.Sprintf("%s provider %s (%s)", e.Provider, e.Kind, e.Executable)
	case e.Detail != "":
		return fmt.Sprintf("%s provider %s: %s", e.Provider, e.Kind, e.Detail)
	default:
		return fmt.Sprintf("%s provider %s", e.Provider, e.Kind)
	}
}

func (e *ProviderError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func recommendedActionForKind(kind ProviderErrorKind) ProviderErrorAction {
	switch kind {
	case ErrorKindRateLimited, ErrorKindNetworkError:
		return ProviderErrorActionRetry
	case ErrorKindAuthFailure, ErrorKindBillingRequired, ErrorKindSessionExpired:
		return ProviderErrorActionBlock
	case ErrorKindQuotaExceeded, ErrorKindTransportError:
		return ProviderErrorActionFail
	default:
		return ProviderErrorActionFail
	}
}

func newProviderError(provider domain.ProviderName, kind ProviderErrorKind, executable, detail string, err error) error {
	return &ProviderError{
		Provider:          provider,
		Kind:              kind,
		RecommendedAction: recommendedActionForKind(kind),
		Executable:        executable,
		Detail:            detail,
		Err:               err,
	}
}

func missingExecutableError(provider domain.ProviderName, executable string, err error) error {
	return newProviderError(provider, ErrorKindMissingExecutable, executable, "CLI executable is not available on PATH", err)
}

func probeFailedError(provider domain.ProviderName, executable string, err error) error {
	return newProviderError(provider, ErrorKindProbeFailed, executable, "CLI probe failed", err)
}

func commandFailedError(provider domain.ProviderName, executable string, err error) error {
	return newProviderError(provider, ErrorKindCommandFailed, executable, "provider command failed", err)
}

func invalidResponseError(provider domain.ProviderName, executable, detail string, err error) error {
	return newProviderError(provider, ErrorKindInvalidResponse, executable, detail, err)
}

func unsupportedPhaseError(provider domain.ProviderName, executable, phase string) error {
	return newProviderError(provider, ErrorKindUnsupportedPhase, executable, fmt.Sprintf("provider does not support %s phase", phase), nil)
}

func authFailureError(provider domain.ProviderName, executable string, err error) error {
	return newProviderError(provider, ErrorKindAuthFailure, executable, "provider authentication failed", err)
}

func quotaExceededError(provider domain.ProviderName, executable string, err error) error {
	return newProviderError(provider, ErrorKindQuotaExceeded, executable, "provider quota exceeded", err)
}

func rateLimitedError(provider domain.ProviderName, executable string, err error) error {
	return newProviderError(provider, ErrorKindRateLimited, executable, "provider rate limited", err)
}

func billingRequiredError(provider domain.ProviderName, executable string, err error) error {
	return newProviderError(provider, ErrorKindBillingRequired, executable, "provider billing is required", err)
}

func sessionExpiredError(provider domain.ProviderName, executable string, err error) error {
	return newProviderError(provider, ErrorKindSessionExpired, executable, "provider session expired", err)
}

func networkError(provider domain.ProviderName, executable string, err error) error {
	return newProviderError(provider, ErrorKindNetworkError, executable, "provider network error", err)
}

func transportError(provider domain.ProviderName, executable string, err error) error {
	return newProviderError(provider, ErrorKindTransportError, executable, "provider transport error", err)
}

func classifyCommandError(provider domain.ProviderName, executable string, result CommandResult, err error) error {
	normalized := strings.ToLower(strings.TrimSpace(strings.Join([]string{result.Stderr, result.Stdout, errorString(err)}, "\n")))
	wrapped := wrapCommandError(err, result)

	switch {
	case containsAny(normalized, "rate limit", "rate-limit", "too many requests", "429"):
		return rateLimitedError(provider, executable, wrapped)
	case containsAny(normalized, "authentication", "unauthorized", "invalid api key", "api key", "401"):
		return authFailureError(provider, executable, wrapped)
	case containsAny(normalized, "billing", "payment required", "402", "payment method", "credit balance"):
		return billingRequiredError(provider, executable, wrapped)
	case containsAny(normalized, "insufficient_quota", "quota exceeded", "quota", "usage limit", "credits exhausted"):
		return quotaExceededError(provider, executable, wrapped)
	case containsAny(normalized, "session expired", "session has expired", "login expired", "reauthenticate", "re-authenticate"):
		return sessionExpiredError(provider, executable, wrapped)
	case containsAny(normalized, "timeout", "timed out", "connection reset", "connection refused", "network is unreachable", "temporary failure in name resolution", "no such host", "econnreset", "econnrefused", "tls handshake timeout"):
		return networkError(provider, executable, wrapped)
	case containsAny(normalized, "transport", "broken pipe", "unexpected eof", "stream closed", "protocol error", "connection closed unexpectedly"):
		return transportError(provider, executable, wrapped)
	default:
		return commandFailedError(provider, executable, wrapped)
	}
}

func wrapCommandError(err error, result CommandResult) error {
	output := strings.TrimSpace(strings.Join([]string{result.Stdout, result.Stderr}, "\n"))
	if output == "" {
		return err
	}
	if err == nil {
		return fmt.Errorf("%s", output)
	}
	return fmt.Errorf("%w: %s", err, output)
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func containsAny(text string, patterns ...string) bool {
	for _, pattern := range patterns {
		if strings.Contains(text, pattern) {
			return true
		}
	}
	return false
}
