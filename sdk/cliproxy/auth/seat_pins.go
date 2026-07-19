package auth

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

var seatIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}\.[a-z0-9][a-z0-9_-]{0,62}$`)

type seatPinResolver interface {
	Resolve(context.Context, string) (string, error)
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

func (m *Manager) resolveSeatPin(ctx context.Context, opts cliproxyexecutor.Options) error {
	if pinnedAuthIDFromMetadata(opts.Metadata) != "" {
		return nil
	}
	seatID := seatIDFromMetadata(opts.Metadata)
	if seatID == "" {
		return nil
	}
	if m == nil || m.seatPins == nil {
		return seatPinError("seat_lookup_failed", "seat-pin resolver unavailable")
	}
	authID, err := m.seatPins.Resolve(ctx, seatID)
	if err != nil {
		var proxyErr *Error
		if errors.As(err, &proxyErr) {
			return proxyErr
		}
		return seatPinError("seat_lookup_failed", "seat-pin lookup failed")
	}
	if opts.Metadata == nil {
		return seatPinError("seat_lookup_failed", "request metadata unavailable")
	}
	opts.Metadata[cliproxyexecutor.PinnedAuthMetadataKey] = authID
	return nil
}

func seatPinError(code, message string) *Error {
	return &Error{Code: code, Message: message, Retryable: false, HTTPStatus: 503}
}
