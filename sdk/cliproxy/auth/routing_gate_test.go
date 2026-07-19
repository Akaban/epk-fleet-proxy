package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestRoutingGateRejectsRoundRobinWith4xx(t *testing.T) {
	t.Setenv(requireFillFirstEnv, "1")
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: "round-robin"}})

	_, err := manager.SelectAuth(context.Background(), "test", "", cliproxyexecutor.Options{})
	var authErr *Error
	if !errors.As(err, &authErr) {
		t.Fatalf("SelectAuth error = %v, want *Error", err)
	}
	if authErr.Code != "routing_strategy_disabled" || authErr.HTTPStatus != http.StatusConflict || authErr.Retryable {
		t.Fatalf("routing gate error = %+v", authErr)
	}
}

func TestRoutingGateRejectsUnsetStrategy(t *testing.T) {
	t.Setenv(requireFillFirstEnv, "true")
	manager := NewManager(nil, nil, nil)

	err := manager.routingStrategyError()
	var authErr *Error
	if !errors.As(err, &authErr) || authErr.HTTPStatus != http.StatusConflict {
		t.Fatalf("unset strategy error = %v", err)
	}
}

func TestRoutingGateAllowsFillFirst(t *testing.T) {
	t.Setenv(requireFillFirstEnv, "yes")
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: "fill-first"}})
	manager.RegisterExecutor(schedulerTestExecutor{})
	if _, err := manager.Register(context.Background(), &Auth{ID: "auth-test", Provider: "test", Status: StatusActive}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	selected, err := manager.SelectAuth(context.Background(), "test", "", cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("SelectAuth: %v", err)
	}
	if selected == nil || selected.ID != "auth-test" {
		t.Fatalf("selected = %+v", selected)
	}
}

func TestRoutingGateIsOptInForNonEPKLibraryUsers(t *testing.T) {
	t.Setenv(requireFillFirstEnv, "")
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: "round-robin"}})
	if err := manager.routingStrategyError(); err != nil {
		t.Fatalf("unarmed routing gate rejected upstream default: %v", err)
	}
}
