package auth

import (
	"net/http"
	"os"
	"strings"
)

const disabledProvidersEnv = "EPK_PROXY_DISABLED_PROVIDERS"

type disabledProviderSet map[string]struct{}

// disabledProvidersFromEnv reads the startup gate controlled by the EPK plane.
// Values are comma/space separated provider IDs; "*" disables every provider.
// The environment is intentionally sampled once per Manager so moving the lever
// requires a deliberate service restart rather than silently changing live calls.
func disabledProvidersFromEnv() disabledProviderSet {
	set := make(disabledProviderSet)
	for _, raw := range strings.FieldsFunc(os.Getenv(disabledProvidersEnv), func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\t' || r == ' '
	}) {
		provider := strings.ToLower(strings.TrimSpace(raw))
		if provider != "" {
			set[provider] = struct{}{}
		}
	}
	return set
}

func (m *Manager) providerDisabled(provider string) bool {
	if m == nil {
		return false
	}
	set, _ := m.disabledProviders.Load().(disabledProviderSet)
	if len(set) == 0 {
		return false
	}
	if _, disabled := set["*"]; disabled {
		return true
	}
	_, disabled := set[strings.ToLower(strings.TrimSpace(provider))]
	return disabled
}

func (m *Manager) enabledProviders(providers []string) (enabled []string, disabled bool) {
	enabled = make([]string, 0, len(providers))
	for _, provider := range providers {
		provider = strings.TrimSpace(provider)
		if provider == "" {
			continue
		}
		if m.providerDisabled(provider) {
			disabled = true
			continue
		}
		enabled = append(enabled, provider)
	}
	return enabled, disabled
}

func providerDisabledError(provider string) *Error {
	provider = strings.TrimSpace(provider)
	message := "provider disabled by control-plane gate"
	if provider != "" {
		message += ": " + provider
	}
	return &Error{
		Code:       "provider_disabled",
		Message:    message,
		Retryable:  false,
		HTTPStatus: http.StatusServiceUnavailable,
	}
}
