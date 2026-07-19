package auth

import (
	"encoding/json"
	"strings"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestSniffTokensAnthropicShape(t *testing.T) {
	u := extractUsageJSON([]byte(`{"id":"m1","usage":{"input_tokens":100,"output_tokens":25,"cache_read_input_tokens":900,"cache_creation_input_tokens":10}}`))
	tk := sniffTokens(u)
	if tk.In == nil || *tk.In != 100 || tk.Out == nil || *tk.Out != 25 {
		t.Fatalf("in/out wrong: %+v", tk)
	}
	if tk.CacheRead == nil || *tk.CacheRead != 900 || tk.CacheWrite == nil || *tk.CacheWrite != 10 {
		t.Fatalf("cache wrong: %+v", tk)
	}
}

func TestSniffTokensOpenAIShape(t *testing.T) {
	u := extractUsageJSON([]byte(`{"usage":{"prompt_tokens":40,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":30}}}`))
	tk := sniffTokens(u)
	if tk.In == nil || *tk.In != 40 || tk.Out == nil || *tk.Out != 7 {
		t.Fatalf("in/out wrong: %+v", tk)
	}
	if tk.CacheRead == nil || *tk.CacheRead != 30 {
		t.Fatalf("cached wrong: %+v", tk)
	}
	if tk.CacheWrite != nil {
		t.Fatalf("cache write should be unknown (nil), got %v", *tk.CacheWrite)
	}
}

func TestExtractUsageFromSSEFrame(t *testing.T) {
	sse := []byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":5,\"output_tokens\":9}}\n\n")
	u := extractUsageJSON(sse)
	tk := sniffTokens(u)
	if tk.Out == nil || *tk.Out != 9 {
		t.Fatalf("SSE usage not extracted: %q -> %+v", u, tk)
	}
}

func TestComputeCostNullHonesty(t *testing.T) {
	cfg := &internalconfig.Config{TelemetryPricing: map[string]internalconfig.ModelPrice{
		"claude/claude-fable-5": {Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75},
	}}
	// no usage at all -> nil cost, no_usage (even though price exists)
	cost, reason := computeCost(cfg, "claude", "claude-fable-5", llmTokens{})
	if cost != nil || reason != "no_usage" {
		t.Fatalf("want nil/no_usage, got %v/%q", cost, reason)
	}
	// usage but unknown model -> nil cost, no_price — NEVER 0
	in, out := int64(1000), int64(1000)
	cost, reason = computeCost(cfg, "openai", "gpt-x-unknown", llmTokens{In: &in, Out: &out})
	if cost != nil || reason != "no_price" {
		t.Fatalf("want nil/no_price, got %v/%q", cost, reason)
	}
	// known model -> priced correctly
	cr := int64(1_000_000)
	cost, reason = computeCost(cfg, "claude", "claude-fable-5", llmTokens{In: &in, Out: &out, CacheRead: &cr})
	if cost == nil || reason != "" {
		t.Fatalf("want priced, got %v/%q", cost, reason)
	}
	want := 1000.0/1e6*3 + 1000.0/1e6*15 + 1.0*0.3
	if *cost < want-1e-9 || *cost > want+1e-9 {
		t.Fatalf("cost=%v want=%v", *cost, want)
	}
}

// TestEventJSONNullHonesty is the wire-level proof of the null-honesty law:
// unknown tokens/cost marshal as literal null, never 0.
func TestEventJSONNullHonesty(t *testing.T) {
	b, err := json.Marshal(llmCallEvent{V: 1, Kind: "llm.call", Outcome: "ok", UnknownReason: "no_usage"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{`"in":null`, `"out":null`, `"cache_read":null`, `"cache_write":null`, `"cost_usd":null`} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %s in %s", want, s)
		}
	}
	if strings.Contains(s, `"cost_usd":0`) {
		t.Fatalf("cost rendered as 0: %s", s)
	}
}

func TestFleetGaugeBalance(t *testing.T) {
	// simulate queued -> dispatched -> finished
	fleetGaugeOnQueued("acct-t", "agent-t")
	n := fleetGaugeOnDispatch("acct-t", "agent-t")
	if n != 1 {
		t.Fatalf("inflight at dispatch = %d, want 1", n)
	}
	snap := FleetGaugeSnapshot()
	found := false
	for _, c := range snap {
		if c.Account == "acct-t" && c.Agent == "agent-t" {
			found = true
			if c.Inflight != 1 || c.Queued != 0 {
				t.Fatalf("cell wrong: %+v", c)
			}
		}
	}
	if !found {
		t.Fatal("cell missing from snapshot")
	}
	fleetGaugeOnFinish("acct-t", "agent-t")
	// abort path: queued entry that never dispatches
	fleetGaugeOnQueued("acct-t2", "agent-t")
	fleetGaugeOnAbort("acct-t2", "agent-t")
	for _, c := range FleetGaugeSnapshot() {
		if (c.Account == "acct-t" || c.Account == "acct-t2") && (c.Inflight != 0 || c.Queued != 0) {
			t.Fatalf("unbalanced cell after finish/abort: %+v", c)
		}
	}
}

func TestCallTelemetryDisabledIsInert(t *testing.T) {
	m := &Manager{}
	m.runtimeConfig.Store(&internalconfig.Config{}) // TelemetryEnabled=false
	tel := m.beginCallTelemetry(nil, "claude", &Auth{ID: "x", Label: "acct"}, "model-a", "alias-a", cliproxyexecutorOptions())
	tel.markQueued()
	tel.markDispatched()
	tel.finish([]byte(`{"usage":{"input_tokens":1}}`), 0, nil) // must not publish or panic
	if tel.enabled {
		t.Fatal("telemetry should be disabled")
	}
}

func TestCallTelemetryReportsConfiguredRoutingStrategy(t *testing.T) {
	m := &Manager{}
	m.runtimeConfig.Store(&internalconfig.Config{
		TelemetryEnabled: true,
		Routing:          internalconfig.RoutingConfig{Strategy: "fill-first"},
	})

	tel := m.beginCallTelemetry(nil, "claude", &Auth{ID: "x", Label: "acct"}, "model-a", "alias-a", cliproxyexecutorOptions())
	if tel.route != "fill-first" {
		t.Fatalf("route = %q, want fill-first", tel.route)
	}
}

func TestCallTelemetryMarksMissingSeatUnattributed(t *testing.T) {
	m := &Manager{}
	m.runtimeConfig.Store(&internalconfig.Config{TelemetryEnabled: true})

	tel := m.beginCallTelemetry(nil, "claude", &Auth{ID: "auth-a"}, "model-a", "model-a", cliproxyexecutor.Options{})
	if tel.seatID != "unattributed" {
		t.Fatalf("seatID = %q, want unattributed", tel.seatID)
	}
	b, err := json.Marshal(llmCallEvent{SeatID: tel.seatID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"seat_id":"unattributed"`) {
		t.Fatalf("unattributed seat_id missing from event: %s", b)
	}
}

func TestCallTelemetryCarriesExactAuthIDSeparatelyFromAccount(t *testing.T) {
	m := &Manager{}
	m.runtimeConfig.Store(&internalconfig.Config{TelemetryEnabled: true})
	auth := &Auth{
		ID:       "codex-bryce@pyramind.ai-prolite.json",
		Label:    "bryce@pyramind.ai",
		FileName: "/runtime/auths/codex-bryce@pyramind.ai-prolite.json",
	}
	opts := cliproxyexecutor.Options{Metadata: map[string]any{
		cliproxyexecutor.SeatIDMetadataKey: "epk9s.chief",
	}}
	tel := m.beginCallTelemetry(nil, "codex", auth, "gpt-5.6-sol", "claude-fable-5", opts)
	if tel.authID != auth.ID {
		t.Fatalf("authID = %q, want exact manager ID %q", tel.authID, auth.ID)
	}
	if tel.account != auth.Label {
		t.Fatalf("account = %q, want human label %q", tel.account, auth.Label)
	}
	if tel.seatID != "epk9s.chief" {
		t.Fatalf("seatID = %q, want epk9s.chief", tel.seatID)
	}
	b, err := json.Marshal(llmCallEvent{SeatID: tel.seatID, AuthID: tel.authID, Account: tel.account})
	if err != nil {
		t.Fatal(err)
	}
	wire := string(b)
	if !strings.Contains(wire, `"seat_id":"epk9s.chief"`) {
		t.Fatalf("seat_id missing from event: %s", wire)
	}
	if !strings.Contains(wire, `"auth_id":"codex-bryce@pyramind.ai-prolite.json"`) {
		t.Fatalf("exact auth_id missing from event: %s", wire)
	}
	if !strings.Contains(wire, `"account":"bryce@pyramind.ai"`) {
		t.Fatalf("account label missing from event: %s", wire)
	}
}

func TestAccountSlugNeverSecret(t *testing.T) {
	a := &Auth{ID: "id-1", FileName: "/home/x/.cli-proxy-api/codex-bryce@godfather.dev-pro.json"}
	if got := accountSlug(a); got != "codex-bryce@godfather.dev-pro" {
		t.Fatalf("slug=%q", got)
	}
	a.Label = "cred2"
	if got := accountSlug(a); got != "cred2" {
		t.Fatalf("label should win: %q", got)
	}
}

func cliproxyexecutorOptions() cliproxyexecutor.Options {
	return cliproxyexecutor.Options{}
}
