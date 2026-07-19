package auth

import (
	"context"
	"sync"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// perAuthInFlightLimiter caps concurrent upstream requests per credential
// (auth ID). A zero or negative limit disables the cap. Excess acquirers
// BLOCK (queue locally) until a slot frees or their context ends, so bursts
// are absorbed here instead of being forwarded upstream.
type perAuthInFlightLimiter struct {
	mu    sync.Mutex
	limit int
	slots map[string]chan struct{}
}

// setLimit installs a new per-credential cap. Existing holders keep and
// release their old slots; a limit change only governs new acquisitions, so
// the cap is briefly approximate across a runtime config reload.
func (l *perAuthInFlightLimiter) setLimit(n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.limit == n {
		return
	}
	l.limit = n
	l.slots = nil
}

// acquire blocks until the credential has a free slot or ctx ends. The
// returned release is idempotent and must be called exactly when the upstream
// request stops being in flight (response returned, or stream fully closed).
func (l *perAuthInFlightLimiter) acquire(ctx context.Context, authID string) (func(), error) {
	l.mu.Lock()
	limit := l.limit
	if limit <= 0 || authID == "" {
		l.mu.Unlock()
		return func() {}, nil
	}
	if l.slots == nil {
		l.slots = make(map[string]chan struct{})
	}
	ch, ok := l.slots[authID]
	if !ok {
		ch = make(chan struct{}, limit)
		l.slots[authID] = ch
	}
	l.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case ch <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	var once sync.Once
	return func() { once.Do(func() { <-ch }) }, nil
}

// holdSlotThroughStream keeps an in-flight slot held until the upstream chunk
// channel closes — the true end of a streaming request — forwarding chunks
// unchanged. Consumers always drain the channel fully (readStreamBootstrap,
// wrapStreamResult, discardStreamChunks), so the forwarder terminates. The
// forwarder doubles as the stream's telemetry tap: it notes first-chunk time
// and usage-bearing payloads, and emits the terminal event on channel close.
func holdSlotThroughStream(sr *cliproxyexecutor.StreamResult, release func(), tel *callTelemetry) *cliproxyexecutor.StreamResult {
	if release == nil && tel == nil {
		return sr
	}
	if release == nil {
		release = func() {}
	}
	if sr == nil || sr.Chunks == nil {
		release()
		tel.finish(nil, 0, nil)
		return sr
	}
	src := sr.Chunks
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		var termErr error
		defer func() {
			tel.finish(nil, 0, termErr)
			release()
		}()
		defer close(out)
		for chunk := range src {
			tel.noteChunk(chunk.Payload)
			if chunk.Err != nil {
				termErr = chunk.Err
			}
			out <- chunk
		}
	}()
	wrapped := *sr
	wrapped.Chunks = out
	return &wrapped
}
