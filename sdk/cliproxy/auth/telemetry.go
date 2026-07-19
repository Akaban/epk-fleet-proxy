package auth

// telemetry.go — per-call fleet telemetry (contract v1.2, LAW 2026-07-18):
// one llm.call / llm.call.error event per upstream attempt, published to
// pgmq proxy.events via the fail-open publisher in internal/logging.
//
// NULL-HONESTY LAW (fleetgame amendment, non-negotiable): when tokens or
// cost are unknown they are emitted as explicit JSON null (never 0), with
// unknown_reason set ("no_usage" / "no_price"). A false $0.00 lies to the
// founder; unknown renders dark.

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

const telemetryContractVersion = 1

// llmTokens carries token counts; nil pointers marshal as JSON null.
type llmTokens struct {
	In         *int64 `json:"in"`
	Out        *int64 `json:"out"`
	CacheRead  *int64 `json:"cache_read"`
	CacheWrite *int64 `json:"cache_write"`
}

type llmCaller struct {
	Agent      string `json:"agent,omitempty"`
	Session    string `json:"session,omitempty"`
	Task       string `json:"task,omitempty"`
	APIKeyName string `json:"api_key_name,omitempty"`
}

type llmCallEvent struct {
	V                  int       `json:"v"`
	Kind               string    `json:"kind"`
	CallID             string    `json:"call_id"`
	TSStart            time.Time `json:"ts_start"`
	TSEnd              time.Time `json:"ts_end"`
	LatencyMS          int64     `json:"latency_ms"`
	TTFBMS             *int64    `json:"ttfb_ms"`
	QueueMS            int64     `json:"queue_ms"`
	InflightAtDispatch int       `json:"inflight_at_dispatch"`
	Provider           string    `json:"provider"`
	Account            string    `json:"account"`
	Model              string    `json:"model"`
	ModelRequested     string    `json:"model_requested,omitempty"`
	Route              string    `json:"route"`
	Stream             bool      `json:"stream"`
	Status             int       `json:"status"`
	Outcome            string    `json:"outcome"`
	Tokens             llmTokens `json:"tokens"`
	CostUSD            *float64  `json:"cost_usd"`
	UnknownReason      string    `json:"unknown_reason,omitempty"`
	Caller             llmCaller `json:"caller"`
	Error              string    `json:"error,omitempty"`
}

// callTelemetry accumulates one upstream attempt's telemetry from acquire to
// terminal emit. Not goroutine-safe except firstChunk/usage, which the stream
// forwarder goroutine owns exclusively after handoff.
type callTelemetry struct {
	enabled  bool
	cfg      *internalconfig.Config
	tsStart  time.Time
	dispatch time.Time
	queueMS  int64
	inflight int
	provider string
	account  string
	model    string
	modelReq string
	route    string
	stream   bool
	caller   llmCaller

	firstChunkAt atomic.Int64 // epoch nanos of first stream chunk; 0 = none
	lastUsage    atomic.Value // string: last usage-bearing JSON payload seen
	emitted      atomic.Bool
	gQueued      bool // gauge: queued++ recorded
	gDispatched  bool // gauge: inflight++ recorded
}

// markQueued registers the attempt on the gauge before the limiter wait.
func (t *callTelemetry) markQueued() {
	if t == nil || !t.enabled || t.gQueued {
		return
	}
	t.gQueued = true
	fleetGaugeOnQueued(t.account, t.caller.Agent)
}

// accountSlug derives a human identifier for the credential — never a secret.
func accountSlug(a *Auth) string {
	if a == nil {
		return ""
	}
	if s := strings.TrimSpace(a.Label); s != "" {
		return s
	}
	if s := strings.TrimSpace(a.FileName); s != "" {
		parts := strings.Split(s, "/")
		base := parts[len(parts)-1]
		return strings.TrimSuffix(base, ".json")
	}
	return a.ID
}

// callerFromContext extracts X-EPK-* attribution from the inbound request.
// Precedence: explicit opts.Headers, then the gin request riding ctx.
func callerFromContext(ctx context.Context, opts cliproxyexecutor.Options, cfg *internalconfig.Config) llmCaller {
	var h http.Header
	if opts.Headers != nil {
		h = opts.Headers
	} else if ctx != nil {
		if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
			h = ginCtx.Request.Header
		}
	}
	if h == nil {
		return llmCaller{}
	}
	c := llmCaller{
		Agent:   strings.TrimSpace(h.Get("X-EPK-Agent")),
		Session: strings.TrimSpace(h.Get("X-EPK-Session")),
		Task:    strings.TrimSpace(h.Get("X-EPK-Task")),
	}
	c.APIKeyName = apiKeyName(h, cfg)
	return c
}

// apiKeyName maps the presented inbound key to a stable index name
// ("key-1"...). The key value itself never enters an event.
func apiKeyName(h http.Header, cfg *internalconfig.Config) string {
	if cfg == nil {
		return ""
	}
	presented := strings.TrimSpace(h.Get("X-Api-Key"))
	if presented == "" {
		if v := strings.TrimSpace(h.Get("Authorization")); v != "" {
			presented = strings.TrimSpace(strings.TrimPrefix(v, "Bearer "))
		}
	}
	if presented == "" {
		return ""
	}
	for i, k := range cfg.APIKeys {
		if k == presented {
			return "key-" + strconv.Itoa(i+1)
		}
	}
	return "key-unknown"
}

// beginCallTelemetry captures pre-dispatch facts. Call markDispatched after
// the limiter slot is acquired.
func (m *Manager) beginCallTelemetry(ctx context.Context, provider string, a *Auth, execModel, requestedModel string, opts cliproxyexecutor.Options) *callTelemetry {
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	route := "round-robin"
	if cfg != nil {
		if configured := strings.TrimSpace(cfg.Routing.Strategy); configured != "" {
			route = configured
		}
	}
	t := &callTelemetry{
		enabled:  cfg != nil && cfg.TelemetryEnabled,
		cfg:      cfg,
		tsStart:  time.Now(),
		provider: provider,
		account:  accountSlug(a),
		model:    execModel,
		modelReq: requestedModel,
		route:    route,
		stream:   opts.Stream,
	}
	if !t.enabled {
		return t
	}
	t.caller = callerFromContext(ctx, opts, cfg)
	return t
}

// markDispatched records queue wait + concurrency gauge at dispatch.
func (t *callTelemetry) markDispatched() {
	if t == nil {
		return
	}
	t.dispatch = time.Now()
	t.queueMS = t.dispatch.Sub(t.tsStart).Milliseconds()
	if t.enabled && t.gQueued && !t.gDispatched {
		t.gDispatched = true
		t.inflight = fleetGaugeOnDispatch(t.account, t.caller.Agent)
	}
}

// noteChunk lets the stream forwarder record first-chunk time and remember
// the last usage-bearing payload (terminal usage wins).
func (t *callTelemetry) noteChunk(payload []byte) {
	if t == nil || !t.enabled || len(payload) == 0 {
		return
	}
	if t.firstChunkAt.Load() == 0 {
		t.firstChunkAt.Store(time.Now().UnixNano())
	}
	// Cheap pre-filter before parsing: most chunks carry no usage object.
	if !strings.Contains(string(payload), "usage") {
		return
	}
	if u := extractUsageJSON(payload); u != "" {
		t.lastUsage.Store(u)
	}
}

// extractUsageJSON returns the raw usage object from a provider payload
// (searching the common shapes), or "" when absent.
func extractUsageJSON(payload []byte) string {
	for _, path := range []string{"usage", "message.usage", "response.usage"} {
		if u := gjson.GetBytes(payload, path); u.Exists() && u.IsObject() {
			return u.Raw
		}
	}
	// SSE frames: strip "data: " lines and retry.
	s := string(payload)
	if idx := strings.Index(s, "data:"); idx >= 0 {
		for _, line := range strings.Split(s, "\n") {
			line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
			if line == "" || line == "[DONE]" {
				continue
			}
			for _, path := range []string{"usage", "message.usage", "response.usage"} {
				if u := gjson.Get(line, path); u.Exists() && u.IsObject() {
					return u.Raw
				}
			}
		}
	}
	return ""
}

// sniffTokens parses a usage JSON object into token counts, tolerating both
// Anthropic (input_tokens/output_tokens/cache_*) and OpenAI
// (prompt_tokens/completion_tokens[+details]) shapes. nil = that field unknown.
func sniffTokens(usageRaw string) llmTokens {
	var tk llmTokens
	if usageRaw == "" {
		return tk
	}
	get := func(paths ...string) *int64 {
		for _, p := range paths {
			if v := gjson.Get(usageRaw, p); v.Exists() {
				n := v.Int()
				return &n
			}
		}
		return nil
	}
	tk.In = get("input_tokens", "prompt_tokens")
	tk.Out = get("output_tokens", "completion_tokens")
	tk.CacheRead = get("cache_read_input_tokens", "prompt_tokens_details.cached_tokens", "input_tokens_details.cached_tokens")
	tk.CacheWrite = get("cache_creation_input_tokens")
	return tk
}

// computeCost prices the call from the config table. Missing model price or
// missing usage → (nil, reason) per the null-honesty law.
func computeCost(cfg *internalconfig.Config, provider, model string, tk llmTokens) (*float64, string) {
	if tk.In == nil && tk.Out == nil {
		return nil, "no_usage"
	}
	if cfg == nil || len(cfg.TelemetryPricing) == 0 {
		return nil, "no_price"
	}
	price, ok := cfg.TelemetryPricing[provider+"/"+model]
	if !ok {
		price, ok = cfg.TelemetryPricing[model]
	}
	if !ok {
		return nil, "no_price"
	}
	var cost float64
	val := func(p *int64) float64 {
		if p == nil {
			return 0
		}
		return float64(*p)
	}
	cost = val(tk.In)/1e6*price.Input +
		val(tk.Out)/1e6*price.Output +
		val(tk.CacheRead)/1e6*price.CacheRead +
		val(tk.CacheWrite)/1e6*price.CacheWrite
	return &cost, ""
}

// finish classifies the outcome and emits the terminal event. Idempotent —
// retry wrappers may call it defensively; only the first call emits.
func (t *callTelemetry) finish(nonStreamPayload []byte, status int, callErr error) {
	if t == nil || !t.enabled || !t.emitted.CompareAndSwap(false, true) {
		return
	}
	now := time.Now()
	usageRaw := ""
	if len(nonStreamPayload) > 0 {
		usageRaw = extractUsageJSON(nonStreamPayload)
	}
	if usageRaw == "" {
		if s, ok := t.lastUsage.Load().(string); ok {
			usageRaw = s
		}
	}
	tk := sniffTokens(usageRaw)
	cost, unknownReason := computeCost(t.cfg, t.provider, t.model, tk)

	outcome := "ok"
	kind := "llm.call"
	errText := ""
	if callErr != nil {
		kind = "llm.call.error"
		errText = callErr.Error()
		outcome = "error"
		if status == 0 {
			if se, ok := callErr.(cliproxyexecutor.StatusError); ok {
				status = se.StatusCode()
			}
		}
		switch {
		case status == 429:
			outcome = "rate_limited"
		case callErr == context.Canceled || strings.Contains(errText, "context canceled"):
			outcome = "aborted"
		}
	} else if status == 0 {
		status = 200
	}

	var ttfb *int64
	if fc := t.firstChunkAt.Load(); fc > 0 {
		v := (fc - t.tsStart.UnixNano()) / int64(time.Millisecond)
		ttfb = &v
	}

	logging.PublishFleetEvent(llmCallEvent{
		V:                  telemetryContractVersion,
		Kind:               kind,
		CallID:             uuid.NewString(),
		TSStart:            t.tsStart.UTC(),
		TSEnd:              now.UTC(),
		LatencyMS:          now.Sub(t.tsStart).Milliseconds(),
		TTFBMS:             ttfb,
		QueueMS:            t.queueMS,
		InflightAtDispatch: t.inflight,
		Provider:           t.provider,
		Account:            t.account,
		Model:              t.model,
		ModelRequested:     t.modelReq,
		Route:              t.route,
		Stream:             t.stream,
		Status:             status,
		Outcome:            outcome,
		Tokens:             tk,
		CostUSD:            cost,
		UnknownReason:      unknownReason,
		Caller:             t.caller,
		Error:              errText,
	})
	switch {
	case t.gDispatched:
		fleetGaugeOnFinish(t.account, t.caller.Agent)
	case t.gQueued:
		fleetGaugeOnAbort(t.account, t.caller.Agent) // never dispatched
	}
}
