package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type strictPinExecutor struct {
	mu     sync.Mutex
	calls  []string
	errors map[string]error
}

func (e *strictPinExecutor) Identifier() string { return "claude" }

func (e *strictPinExecutor) record(auth *Auth) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, auth.ID)
	return e.errors[auth.ID]
}

func (e *strictPinExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if err := e.record(auth); err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}

func (e *strictPinExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if err := e.record(auth); err != nil {
		return nil, err
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(auth.ID)}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *strictPinExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *strictPinExecutor) CountTokens(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if err := e.record(auth); err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}

func (e *strictPinExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *strictPinExecutor) Calls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

func TestManagerPinned429ReturnsWithoutCrossAccountFallback(t *testing.T) {
	testCases := []struct {
		name   string
		invoke func(*Manager, cliproxyexecutor.Request, cliproxyexecutor.Options) error
	}{
		{
			name: "execute",
			invoke: func(m *Manager, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) error {
				_, err := m.Execute(context.Background(), []string{"claude"}, req, opts)
				return err
			},
		},
		{
			name: "count",
			invoke: func(m *Manager, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) error {
				_, err := m.ExecuteCount(context.Background(), []string{"claude"}, req, opts)
				return err
			},
		},
		{
			name: "stream",
			invoke: func(m *Manager, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) error {
				_, err := m.ExecuteStream(context.Background(), []string{"claude"}, req, opts)
				return err
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			m.SetRetryConfig(3, time.Second, 0)
			pinnedID := "aa-pinned-" + tc.name + ".json"
			otherID := "bb-other-" + tc.name + ".json"
			executor := &strictPinExecutor{errors: map[string]error{
				pinnedID: &Error{Code: "rate_limit", HTTPStatus: http.StatusTooManyRequests, Message: "quota exhausted"},
			}}
			m.RegisterExecutor(executor)

			model := "claude-sonnet-4-6"
			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(pinnedID, "claude", []*registry.ModelInfo{{ID: model}})
			reg.RegisterClient(otherID, "claude", []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() {
				reg.UnregisterClient(pinnedID)
				reg.UnregisterClient(otherID)
			})
			if _, err := m.Register(context.Background(), &Auth{ID: pinnedID, Provider: "claude"}); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Register(context.Background(), &Auth{ID: otherID, Provider: "claude"}); err != nil {
				t.Fatal(err)
			}

			err := tc.invoke(m, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{
				Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: pinnedID},
			})
			var proxyErr *Error
			if !errors.As(err, &proxyErr) || proxyErr.HTTPStatus != http.StatusTooManyRequests {
				t.Fatalf("error = %#v, want pinned account 429", err)
			}
			calls := executor.Calls()
			if len(calls) != 1 || calls[0] != pinnedID {
				t.Fatalf("upstream attempts = %v, want only pinned auth %q", calls, pinnedID)
			}
		})
	}
}
