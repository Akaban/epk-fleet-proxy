package auth

import (
	"context"
	"errors"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type fakeSeatPinResolver struct {
	authID        string
	err           error
	validateErr   error
	calls         int
	validateCalls int
}

func (r *fakeSeatPinResolver) Resolve(_ context.Context, _ string) (string, error) {
	r.calls++
	return r.authID, r.err
}

func (r *fakeSeatPinResolver) ValidateAuth(_ context.Context, _ string) error {
	r.validateCalls++
	return r.validateErr
}

func TestResolveSeatPinSeesMapFlipOnNextRequest(t *testing.T) {
	resolver := &fakeSeatPinResolver{authID: "auth-a.json"}
	m := &Manager{seatPins: resolver}
	first := cliproxyexecutor.Options{Metadata: map[string]any{
		cliproxyexecutor.SeatIDMetadataKey: "epk9s.chief",
	}}
	if err := m.resolveSeatPin(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if got := pinnedAuthIDFromMetadata(first.Metadata); got != "auth-a.json" {
		t.Fatalf("first pin = %q, want auth-a.json", got)
	}

	resolver.authID = "auth-b.json"
	second := cliproxyexecutor.Options{Metadata: map[string]any{
		cliproxyexecutor.SeatIDMetadataKey: "epk9s.chief",
	}}
	if err := m.resolveSeatPin(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if got := pinnedAuthIDFromMetadata(second.Metadata); got != "auth-b.json" {
		t.Fatalf("second pin = %q, want auth-b.json", got)
	}
	if resolver.calls != 2 {
		t.Fatalf("resolver calls = %d, want one lookup per request", resolver.calls)
	}
}

func TestResolveSeatPinUnmappedIsHonest503(t *testing.T) {
	t.Setenv("EPK_PROXY_SEAT_GATE", "enforce")
	resolver := &fakeSeatPinResolver{err: seatPinError("seat_unmapped", "seat has no active auth mapping")}
	m := &Manager{seatPins: resolver}
	opts := cliproxyexecutor.Options{Metadata: map[string]any{
		cliproxyexecutor.SeatIDMetadataKey: "epk9s.unknown",
	}}
	err := m.resolveSeatPin(context.Background(), opts)
	var proxyErr *Error
	if !errors.As(err, &proxyErr) || proxyErr.Code != "seat_unmapped" || proxyErr.HTTPStatus != 503 {
		t.Fatalf("error = %#v, want seat_unmapped 503", err)
	}
	if pinnedAuthIDFromMetadata(opts.Metadata) != "" {
		t.Fatal("unmapped seat invented a pin")
	}
}

// Observe mode (the default) NEVER 503s: an unmapped seat falls through to
// permissive selection with no pin, and the block is telemetered elsewhere.
func TestResolveSeatPinObserveModeUnmappedFallsThrough(t *testing.T) {
	t.Setenv("EPK_PROXY_SEAT_GATE", "observe")
	resolver := &fakeSeatPinResolver{err: seatPinError("seat_unmapped", "seat has no active auth mapping")}
	m := &Manager{seatPins: resolver}
	opts := cliproxyexecutor.Options{Metadata: map[string]any{
		cliproxyexecutor.SeatIDMetadataKey: "epk9s.unknown",
	}}
	if err := m.resolveSeatPin(context.Background(), opts); err != nil {
		t.Fatalf("observe mode returned error %#v, want nil fallthrough", err)
	}
	if pinnedAuthIDFromMetadata(opts.Metadata) != "" {
		t.Fatal("observe fallthrough invented a pin")
	}
}

// Observe mode drops an ineligible explicit pin so the fallthrough is not bound
// to the rejected credential.
func TestResolveSeatPinObserveModeDropsQuarantinedPin(t *testing.T) {
	t.Setenv("EPK_PROXY_SEAT_GATE", "observe")
	resolver := &fakeSeatPinResolver{validateErr: seatPinError("auth_quarantined", "explicit account is quarantined")}
	m := &Manager{seatPins: resolver}
	opts := cliproxyexecutor.Options{Metadata: map[string]any{
		cliproxyexecutor.RequestPathMetadataKey: "/v1/messages",
		cliproxyexecutor.PinnedAuthMetadataKey:  "quarantined-auth.json",
	}}
	if err := m.resolveSeatPin(context.Background(), opts); err != nil {
		t.Fatalf("observe mode returned error %#v, want nil fallthrough", err)
	}
	if got := pinnedAuthIDFromMetadata(opts.Metadata); got != "" {
		t.Fatalf("observe mode kept ineligible pin %q, want dropped", got)
	}
	if resolver.validateCalls != 1 {
		t.Fatalf("validate calls = %d, want 1", resolver.validateCalls)
	}
}

func TestResolveSeatPinPinnedAuthEscapeHatchWins(t *testing.T) {
	resolver := &fakeSeatPinResolver{authID: "seat-auth.json"}
	m := &Manager{seatPins: resolver}
	opts := cliproxyexecutor.Options{Metadata: map[string]any{
		cliproxyexecutor.SeatIDMetadataKey:     "epk9s.chief",
		cliproxyexecutor.PinnedAuthMetadataKey: "escape-auth.json",
	}}
	if err := m.resolveSeatPin(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if got := pinnedAuthIDFromMetadata(opts.Metadata); got != "escape-auth.json" {
		t.Fatalf("escape pin = %q", got)
	}
	if resolver.calls != 0 {
		t.Fatalf("resolver called %d times despite explicit pin", resolver.calls)
	}
}

func TestResolveSeatPinGatewayRequiresExplicitAccount(t *testing.T) {
	t.Setenv("EPK_PROXY_SEAT_GATE", "enforce")
	resolver := &fakeSeatPinResolver{}
	m := &Manager{seatPins: resolver}
	opts := cliproxyexecutor.Options{Metadata: map[string]any{
		cliproxyexecutor.RequestPathMetadataKey: "/v1/messages",
	}}
	err := m.resolveSeatPin(context.Background(), opts)
	var proxyErr *Error
	if !errors.As(err, &proxyErr) || proxyErr.Code != "auth_required" || proxyErr.HTTPStatus != 503 {
		t.Fatalf("error = %#v, want auth_required 503", err)
	}
	if resolver.calls != 0 || resolver.validateCalls != 0 {
		t.Fatalf("resolver calls = %d validate = %d, want zero", resolver.calls, resolver.validateCalls)
	}
}

// In observe mode a gateway request with no seat and no pin falls through
// instead of 503ing an anonymous caller.
func TestResolveSeatPinObserveModeGatewayNoAccountFallsThrough(t *testing.T) {
	t.Setenv("EPK_PROXY_SEAT_GATE", "observe")
	resolver := &fakeSeatPinResolver{}
	m := &Manager{seatPins: resolver}
	opts := cliproxyexecutor.Options{Metadata: map[string]any{
		cliproxyexecutor.RequestPathMetadataKey: "/v1/messages",
	}}
	if err := m.resolveSeatPin(context.Background(), opts); err != nil {
		t.Fatalf("observe mode returned error %#v, want nil fallthrough", err)
	}
	if resolver.calls != 0 || resolver.validateCalls != 0 {
		t.Fatalf("resolver calls = %d validate = %d, want zero", resolver.calls, resolver.validateCalls)
	}
}

func TestResolveSeatPinGatewayValidatesDirectPin(t *testing.T) {
	resolver := &fakeSeatPinResolver{}
	m := &Manager{seatPins: resolver}
	opts := cliproxyexecutor.Options{Metadata: map[string]any{
		cliproxyexecutor.RequestPathMetadataKey: "/v1/messages",
		cliproxyexecutor.PinnedAuthMetadataKey:  "explicit-auth.json",
	}}
	if err := m.resolveSeatPin(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if resolver.validateCalls != 1 || resolver.calls != 0 {
		t.Fatalf("resolver calls = %d validate = %d, want 0/1", resolver.calls, resolver.validateCalls)
	}
}

func TestResolveSeatPinGatewayRejectsQuarantinedPin(t *testing.T) {
	t.Setenv("EPK_PROXY_SEAT_GATE", "enforce")
	resolver := &fakeSeatPinResolver{validateErr: seatPinError("auth_quarantined", "explicit account is quarantined")}
	m := &Manager{seatPins: resolver}
	opts := cliproxyexecutor.Options{Metadata: map[string]any{
		cliproxyexecutor.RequestPathMetadataKey: "/v1/messages",
		cliproxyexecutor.PinnedAuthMetadataKey:  "quarantined-auth.json",
	}}
	err := m.resolveSeatPin(context.Background(), opts)
	var proxyErr *Error
	if !errors.As(err, &proxyErr) || proxyErr.Code != "auth_quarantined" || proxyErr.HTTPStatus != 503 {
		t.Fatalf("error = %#v, want auth_quarantined 503", err)
	}
	if resolver.validateCalls != 1 {
		t.Fatalf("validate calls = %d, want 1", resolver.validateCalls)
	}
}
