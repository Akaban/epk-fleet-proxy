package logging

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

type endpointKey struct{}
type responseStatusKey struct{}
type responseHeadersKey struct{}
type selectedAuthKey struct{}

type responseStatusHolder struct {
	status atomic.Int32
}

type responseHeadersHolder struct {
	mu      sync.RWMutex
	headers http.Header
}

func WithEndpoint(ctx context.Context, endpoint string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, endpointKey{}, endpoint)
}

func GetEndpoint(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if endpoint, ok := ctx.Value(endpointKey{}).(string); ok {
		return endpoint
	}
	return ""
}

func WithResponseStatusHolder(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if holder, ok := ctx.Value(responseStatusKey{}).(*responseStatusHolder); ok && holder != nil {
		return ctx
	}
	return context.WithValue(ctx, responseStatusKey{}, &responseStatusHolder{})
}

func WithResponseHeadersHolder(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if holder, ok := ctx.Value(responseHeadersKey{}).(*responseHeadersHolder); ok && holder != nil {
		return ctx
	}
	return context.WithValue(ctx, responseHeadersKey{}, &responseHeadersHolder{})
}

// selectedAuthHolder receives the auth ID selected during execution so the
// request-logging middleware can stamp per-account identity (auth_id/account/
// email) on the minimal per-request proxy.event AFTER c.Next(). This is the
// founder-named gap (msg 971 / order 1005): the minimal gauge event carried no
// account. Mirrors responseStatusHolder — a mutable holder threaded through the
// execution context; the auth selection writes it, the middleware reads it.
type selectedAuthHolder struct {
	mu     sync.Mutex
	authID string
}

// WithSelectedAuthHolder installs (or reuses) a selected-auth holder on ctx.
func WithSelectedAuthHolder(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if holder, ok := ctx.Value(selectedAuthKey{}).(*selectedAuthHolder); ok && holder != nil {
		return ctx
	}
	return context.WithValue(ctx, selectedAuthKey{}, &selectedAuthHolder{})
}

// SelectedAuthHolderInto shares src's holder pointer onto dst (when src has one
// and dst does not). It bridges the request context's holder into the separate
// execution context built by GetContextWithCancel, so a write during execution
// is visible to the middleware reading the request context after c.Next().
func SelectedAuthHolderInto(src, dst context.Context) context.Context {
	if src == nil || dst == nil {
		return dst
	}
	holder, ok := src.Value(selectedAuthKey{}).(*selectedAuthHolder)
	if !ok || holder == nil {
		return dst
	}
	if existing, ok := dst.Value(selectedAuthKey{}).(*selectedAuthHolder); ok && existing != nil {
		return dst
	}
	return context.WithValue(dst, selectedAuthKey{}, holder)
}

// HasSelectedAuthHolder reports whether ctx carries a selected-auth holder.
func HasSelectedAuthHolder(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	holder, ok := ctx.Value(selectedAuthKey{}).(*selectedAuthHolder)
	return ok && holder != nil
}

// SetSelectedAuth records the auth ID selected during execution on the holder,
// if one is present. Safe under concurrency and a no-op without a holder.
func SetSelectedAuth(ctx context.Context, authID string) {
	if ctx == nil {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	if holder, ok := ctx.Value(selectedAuthKey{}).(*selectedAuthHolder); ok && holder != nil {
		holder.mu.Lock()
		holder.authID = authID
		holder.mu.Unlock()
	}
}

// GetSelectedAuth returns the auth ID selected during execution, or "".
func GetSelectedAuth(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if holder, ok := ctx.Value(selectedAuthKey{}).(*selectedAuthHolder); ok && holder != nil {
		holder.mu.Lock()
		defer holder.mu.Unlock()
		return holder.authID
	}
	return ""
}

func SetResponseStatus(ctx context.Context, status int) {
	if ctx == nil || status <= 0 {
		return
	}
	holder, ok := ctx.Value(responseStatusKey{}).(*responseStatusHolder)
	if !ok || holder == nil {
		return
	}
	holder.status.Store(int32(status))
}

func SetResponseHeaders(ctx context.Context, headers http.Header) {
	if ctx == nil {
		return
	}
	holder, ok := ctx.Value(responseHeadersKey{}).(*responseHeadersHolder)
	if !ok || holder == nil {
		return
	}
	holder.mu.Lock()
	defer holder.mu.Unlock()
	holder.headers = cloneHTTPHeader(headers)
}

func GetResponseStatus(ctx context.Context) int {
	if ctx == nil {
		return 0
	}
	holder, ok := ctx.Value(responseStatusKey{}).(*responseStatusHolder)
	if !ok || holder == nil {
		return 0
	}
	return int(holder.status.Load())
}

func GetResponseHeaders(ctx context.Context) http.Header {
	if ctx == nil {
		return nil
	}
	holder, ok := ctx.Value(responseHeadersKey{}).(*responseHeadersHolder)
	if !ok || holder == nil {
		return nil
	}
	holder.mu.RLock()
	defer holder.mu.RUnlock()
	return cloneHTTPHeader(holder.headers)
}

func cloneHTTPHeader(src http.Header) http.Header {
	if len(src) == 0 {
		return nil
	}
	dst := make(http.Header, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}
