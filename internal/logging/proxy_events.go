package logging

// proxy_events.go — fleet observability: publish one event per AI-API request
// into the pgmq topic proxy.events on the EPK control plane.
//
// LAW (founder, 2026-07-17): the proxy NEVER stalls on PG. Publish is
// non-blocking (bounded channel, drop + count on overflow); the writer batches
// with a hard timeout and fails open (drop + count) on any PG error. The DSN
// comes from a 0600 env file (~/.epkv2/gpt-proxy/pg-env, key EPK_PROXY_DSN)
// and never enters argv. No file, no DSN -> publisher stays disabled.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

type proxyEvent struct {
	ID        string `json:"id"`
	TS        int64  `json:"ts"` // epoch millis
	Slug      string `json:"slug"`
	Status    int    `json:"status"`
	LatencyMS int64  `json:"latency_ms"`
	InFlight  int32  `json:"in_flight"`
	Client    string `json:"client"`
	// Per-account identity of the credential SELECTED for this request (founder
	// msg 971 / order 1005). Empty when no auth was selected (e.g. 401s) — that
	// null is legitimate, not a gap. Stamped by the gin middleware from the
	// selected-auth holder after c.Next().
	AuthID  string `json:"auth_id,omitempty"`
	Account string `json:"account,omitempty"`
	Email   string `json:"email,omitempty"`
}

var (
	peOnce            sync.Once
	peCh              chan string
	peEnabled         atomic.Bool
	peDrops           atomic.Int64
	peSent            atomic.Int64
	peProjectionDrops atomic.Int64
	inFlight          atomic.Int32
)

// FleetEventsDSN returns the protected control-plane DSN for in-process
// consumers such as seat-pin resolution. Callers must never log or serialize it.
func FleetEventsDSN() string {
	return proxyEventsDSN()
}

func proxyEventsDSN() string {
	path := os.Getenv("EPK_PROXY_PG_ENV")
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".epkv2", "gpt-proxy", "pg-env")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "EPK_PROXY_DSN="); ok {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

func initProxyEvents() {
	dsn := proxyEventsDSN()
	if dsn == "" {
		return // fail-open: observability absent, proxy unaffected
	}
	peCh = make(chan string, 4096)
	peEnabled.Store(true)
	go proxyEventsWriter(dsn)
}

// publishProxyEvent is non-blocking: full channel or disabled publisher = drop.
func publishProxyEvent(evt proxyEvent) {
	if b, err := json.Marshal(evt); err == nil {
		enqueueFleetEvent(string(b))
	}
}

// PublishFleetEvent publishes any JSON-marshalable event (e.g. the llm.call
// telemetry contract) onto proxy.events. Same law as publishProxyEvent:
// non-blocking, fail-open, the proxy never stalls on PG.
func PublishFleetEvent(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	enqueueFleetEvent(string(b))
}

func enqueueFleetEvent(msg string) {
	peOnce.Do(initProxyEvents)
	if !peEnabled.Load() {
		return
	}
	select {
	case peCh <- msg:
	default:
		peDrops.Add(1)
	}
}

func proxyEventsWriter(dsn string) {
	var conn *pgx.Conn
	defer func() {
		if conn != nil {
			_ = conn.Close(context.Background())
		}
	}()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	queue := os.Getenv("EPK_PROXY_EVENTS_QUEUE")
	if queue == "" {
		queue = "proxy.events"
	}
	// The queue name is inlined into the SQL (bind params don't infer cleanly
	// in the function-call position); restrict it to safe characters.
	for _, r := range queue {
		if !(r == '.' || r == '_' || r == '-' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			queue = "proxy.events"
			break
		}
	}
	sendSQL := `select pgmq.send_batch('` + queue + `', $1::jsonb[])`
	observeSQL := `select epk.epk_proxy_observe_events($1::jsonb[])`
	batch := make([]string, 0, 64)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if conn == nil || conn.IsClosed() {
			c, err := pgx.Connect(ctx, dsn)
			if err != nil {
				peDrops.Add(int64(len(batch)))
				batch = batch[:0]
				return
			}
			conn = c
		}
		msgs := make([]string, len(batch))
		copy(msgs, batch)
		if _, err := conn.Exec(ctx, sendSQL, msgs); err != nil {
			peDrops.Add(int64(len(batch)))
			_ = conn.Close(context.Background())
			conn = nil
		} else {
			peSent.Add(int64(len(batch)))
			if _, errObserve := conn.Exec(ctx, observeSQL, msgs); errObserve != nil {
				peProjectionDrops.Add(int64(len(batch)))
			}
		}
		batch = batch[:0]
	}
	for {
		select {
		case evt := <-peCh:
			batch = append(batch, evt)
			if len(batch) >= 64 {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// ProxyEventStats exposes raw telemetry sent/dropped counters.
func ProxyEventStats() (sent, dropped int64) { return peSent.Load(), peDrops.Load() }

// ProxyProjectionDrops counts events whose raw PGMQ write succeeded but whose
// immediate control-plane projection failed. Raw telemetry remains replayable.
func ProxyProjectionDrops() int64 { return peProjectionDrops.Load() }
