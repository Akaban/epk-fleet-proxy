package auth

import (
	"context"
	"errors"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type fakeSeatPinResolver struct {
	authID string
	err    error
	calls  int
}

func (r *fakeSeatPinResolver) Resolve(_ context.Context, _ string) (string, error) {
	r.calls++
	return r.authID, r.err
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
