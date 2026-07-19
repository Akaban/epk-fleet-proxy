package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestProviderGateBlocksSingleProvider(t *testing.T) {
	t.Setenv(disabledProvidersEnv, " test, claude ")
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(schedulerTestExecutor{})
	if _, err := manager.Register(context.Background(), &Auth{ID: "auth-test", Provider: "test", Status: StatusActive}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	_, err := manager.SelectAuth(context.Background(), "test", "", cliproxyexecutor.Options{})
	var authErr *Error
	if !errors.As(err, &authErr) {
		t.Fatalf("SelectAuth error = %v, want *Error", err)
	}
	if authErr.Code != "provider_disabled" || authErr.HTTPStatus != http.StatusServiceUnavailable || authErr.Retryable {
		t.Fatalf("provider gate error = %+v", authErr)
	}
}

func TestProviderGateWildcardDisablesEveryProvider(t *testing.T) {
	t.Setenv(disabledProvidersEnv, "*")
	manager := NewManager(nil, nil, nil)
	for _, provider := range []string{"test", "claude", "codex"} {
		if !manager.providerDisabled(provider) {
			t.Fatalf("provider %q was not disabled by wildcard", provider)
		}
	}
}

func TestProviderGateIsSampledAtManagerStartup(t *testing.T) {
	t.Setenv(disabledProvidersEnv, "")
	manager := NewManager(nil, nil, nil)
	t.Setenv(disabledProvidersEnv, "test")
	if manager.providerDisabled("test") {
		t.Fatal("live environment mutation changed a running manager without restart")
	}

	restarted := NewManager(nil, nil, nil)
	if !restarted.providerDisabled("test") {
		t.Fatal("restarted manager did not load the provider gate")
	}
}

func TestEnabledProvidersFiltersOnlyGatedProviders(t *testing.T) {
	t.Setenv(disabledProvidersEnv, "claude")
	manager := NewManager(nil, nil, nil)

	enabled, disabled := manager.enabledProviders([]string{"claude", "codex"})
	if !disabled {
		t.Fatal("disabled provider was not reported")
	}
	if len(enabled) != 1 || enabled[0] != "codex" {
		t.Fatalf("enabled providers = %v, want [codex]", enabled)
	}
}
