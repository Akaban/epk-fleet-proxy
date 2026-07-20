package auth

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

var seatIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}\.[a-z0-9][a-z0-9_-]{0,62}$`)

type seatPinResolver interface {
	Resolve(context.Context, string) (string, error)
	ValidateAuth(context.Context, string) error
}

type pgSeatPinResolver struct {
	mu   sync.Mutex
	pool *pgxpool.Pool
}

func (r *pgSeatPinResolver) connectionPool(ctx context.Context) (*pgxpool.Pool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pool != nil {
		return r.pool, nil
	}
	dsn := logging.FleetEventsDSN()
	if dsn == "" {
		return nil, errors.New("control-plane DSN unavailable")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	config.MaxConns = 2
	config.MinConns = 0
	config.MaxConnIdleTime = 2 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	r.pool = pool
	return pool, nil
}

func (r *pgSeatPinResolver) Resolve(ctx context.Context, seatID string) (string, error) {
	if r == nil {
		return "", errors.New("seat-pin resolver unavailable")
	}
	seatID = normalizeSeatID(seatID)
	if !seatIDPattern.MatchString(seatID) {
		return "", seatPinError("seat_invalid", "invalid X-Seat-Id")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	lookupCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	pool, err := r.connectionPool(lookupCtx)
	if err != nil {
		return "", err
	}
	var authID string
	err = pool.QueryRow(lookupCtx,
		"select auth_id from epk.seat_pin_lookup where seat_id=$1", seatID).Scan(&authID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", seatPinError("seat_unmapped", "seat has no active auth mapping")
	}
	if err != nil {
		return "", err
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return "", seatPinError("seat_unmapped", "seat has no active auth mapping")
	}
	return authID, nil
}

func (r *pgSeatPinResolver) ValidateAuth(ctx context.Context, authID string) error {
	if r == nil {
		return errors.New("seat-pin resolver unavailable")
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return seatPinError("auth_required", "an explicit account is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	lookupCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	pool, err := r.connectionPool(lookupCtx)
	if err != nil {
		return err
	}
	var status string
	var excluded bool
	err = pool.QueryRow(lookupCtx,
		"select status,excluded from epk.accounts where auth_id=$1", authID).Scan(&status, &excluded)
	if errors.Is(err, pgx.ErrNoRows) {
		return seatPinError("auth_unknown", "explicit account is not registered")
	}
	if err != nil {
		return err
	}
	if status != "active" {
		return seatPinError("auth_disabled", "explicit account is not active")
	}
	if excluded {
		return seatPinError("auth_quarantined", "explicit account is quarantined")
	}
	return nil
}

func normalizeSeatID(seatID string) string {
	return strings.ToLower(strings.TrimSpace(seatID))
}

func seatIDFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	if value, ok := meta[cliproxyexecutor.SeatIDMetadataKey].(string); ok {
		return normalizeSeatID(value)
	}
	return ""
}

type gatewayAuthEvent struct {
	V         int       `json:"v"`
	Kind      string    `json:"kind"`
	EventID   string    `json:"event_id"`
	Timestamp time.Time `json:"timestamp"`
	Code      string    `json:"code"`
	Mode      string    `json:"mode"`
	Status    int       `json:"status"`
	SeatID    string    `json:"seat_id,omitempty"`
	AuthID    string    `json:"auth_id,omitempty"`
	Account   string    `json:"account,omitempty"`
	Email     string    `json:"email,omitempty"`
}

// seatGateEnforce reports whether the seat gate is in ENFORCE mode. The default
// (unset / anything but "enforce") is OBSERVE: gate conditions telemeter but
// never 503 — the request falls through to permissive selection. Flipping to
// enforce is a gateway-squad + founder decision (post-IGNITE), not this lane's.
func seatGateEnforce() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("EPK_PROXY_SEAT_GATE")), "enforce")
}

// publishGatewayAuthEvent emits one gateway auth-gate event. In enforce mode it
// is a "gateway.auth.rejected" (status 503, the request was blocked); in observe
// mode it is a "gateway.auth.would_block" (status 0, the request fell through).
// Both are stamped with the resolved-or-null seat/auth/account/email so the
// owning squad gets real gate data either way.
func publishGatewayAuthEvent(code, seatID, authID string, enforce bool) {
	kind := "gateway.auth.would_block"
	mode := "observe"
	status := 0
	if enforce {
		kind = "gateway.auth.rejected"
		mode = "enforce"
		status = 503
	}
	authID = strings.TrimSpace(authID)
	logging.PublishFleetEvent(gatewayAuthEvent{
		V:         1,
		Kind:      kind,
		EventID:   uuid.NewString(),
		Timestamp: time.Now().UTC(),
		Code:      strings.TrimSpace(code),
		Mode:      mode,
		Status:    status,
		SeatID:    normalizeSeatID(seatID),
		AuthID:    authID,
		Account:   credSlug(authID),
		Email:     credEmail(authID),
	})
}

// seatGate telemeters a gate condition and enforces it ONLY when
// EPK_PROXY_SEAT_GATE=enforce. The default (observe) emits a would-block event
// and returns nil so the request falls through to permissive fill-first
// selection with ZERO 503 blast radius; any ineligible explicit pin is dropped
// so the fallthrough is not itself bound to the rejected credential.
func (m *Manager) seatGate(code, seatID, authID string, honest *Error, opts cliproxyexecutor.Options) error {
	enforce := seatGateEnforce()
	publishGatewayAuthEvent(code, seatID, authID, enforce)
	if enforce {
		if honest != nil {
			return honest
		}
		return seatPinError(code, code)
	}
	if authID != "" && opts.Metadata != nil {
		if cur, _ := opts.Metadata[cliproxyexecutor.PinnedAuthMetadataKey].(string); cur == authID {
			delete(opts.Metadata, cliproxyexecutor.PinnedAuthMetadataKey)
		}
	}
	return nil
}

func (m *Manager) resolveSeatPin(ctx context.Context, opts cliproxyexecutor.Options) error {
	seatID := seatIDFromMetadata(opts.Metadata)
	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	_, gatewayRequest := opts.Metadata[cliproxyexecutor.RequestPathMetadataKey]
	if pinnedAuthID == "" && seatID == "" && !gatewayRequest {
		return nil
	}
	if m == nil || m.seatPins == nil {
		return m.seatGate("seat_lookup_failed", seatID, pinnedAuthID,
			seatPinError("seat_lookup_failed", "seat-pin resolver unavailable"), opts)
	}
	if pinnedAuthID != "" {
		if !gatewayRequest {
			return nil
		}
		if err := m.seatPins.ValidateAuth(ctx, pinnedAuthID); err != nil {
			code := "seat_lookup_failed"
			var proxyErr *Error
			if errors.As(err, &proxyErr) {
				code = proxyErr.Code
			} else {
				proxyErr = seatPinError("seat_lookup_failed", "account lookup failed")
			}
			return m.seatGate(code, seatID, pinnedAuthID, proxyErr, opts)
		}
		return nil
	}
	if seatID == "" {
		return m.seatGate("auth_required", "", "",
			seatPinError("auth_required", "an explicit account is required"), opts)
	}
	authID, err := m.seatPins.Resolve(ctx, seatID)
	if err != nil {
		code := "seat_lookup_failed"
		var proxyErr *Error
		if errors.As(err, &proxyErr) {
			code = proxyErr.Code
		} else {
			proxyErr = seatPinError("seat_lookup_failed", "seat-pin lookup failed")
		}
		return m.seatGate(code, seatID, "", proxyErr, opts)
	}
	if opts.Metadata == nil {
		return m.seatGate("seat_lookup_failed", seatID, authID,
			seatPinError("seat_lookup_failed", "request metadata unavailable"), opts)
	}
	opts.Metadata[cliproxyexecutor.PinnedAuthMetadataKey] = authID
	return nil
}

func seatPinError(code, message string) *Error {
	return &Error{Code: code, Message: message, Retryable: false, HTTPStatus: 503}
}
