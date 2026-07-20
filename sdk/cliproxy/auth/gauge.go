package auth

// gauge.go — per-(account, agent) live concurrency gauge (telemetry contract
// v1.2, A1): the fleetgame renders "calls firing NOW" from this, terminal
// llm.call events carry the history. Exposed two ways: FleetGaugeSnapshot()
// for the /v0/gauge poll endpoint, and debounced gauge.inflight events on
// proxy.events (published only when the state changed, min 1s apart).

import (
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
)

type FleetGaugeCell struct {
	Account  string `json:"account"`
	Email    string `json:"email"`
	Agent    string `json:"agent,omitempty"`
	Inflight int    `json:"inflight"`
	Queued   int    `json:"queued"`
}

type fleetGaugeKey struct{ account, agent string }

var fleetGauge = struct {
	mu    sync.Mutex
	cells map[fleetGaugeKey]*FleetGaugeCell
	dirty bool
	once  sync.Once
}{cells: make(map[fleetGaugeKey]*FleetGaugeCell)}

func fleetGaugeCell(account, agent string) *FleetGaugeCell {
	k := fleetGaugeKey{account, agent}
	c, ok := fleetGauge.cells[k]
	if !ok {
		c = &FleetGaugeCell{Account: account, Email: credEmail(account), Agent: agent}
		fleetGauge.cells[k] = c
	}
	return c
}

func fleetGaugeOnQueued(account, agent string) {
	fleetGauge.once.Do(startFleetGaugePublisher)
	fleetGauge.mu.Lock()
	fleetGaugeCell(account, agent).Queued++
	fleetGauge.dirty = true
	fleetGauge.mu.Unlock()
}

// fleetGaugeOnDispatch moves the attempt queued→inflight and returns the
// cell's new in-flight count (the event's inflight_at_dispatch).
func fleetGaugeOnDispatch(account, agent string) int {
	fleetGauge.mu.Lock()
	c := fleetGaugeCell(account, agent)
	if c.Queued > 0 {
		c.Queued--
	}
	c.Inflight++
	n := c.Inflight
	fleetGauge.dirty = true
	fleetGauge.mu.Unlock()
	return n
}

// fleetGaugeOnAbort undoes a queued entry whose acquire never completed.
func fleetGaugeOnAbort(account, agent string) {
	fleetGauge.mu.Lock()
	c := fleetGaugeCell(account, agent)
	if c.Queued > 0 {
		c.Queued--
	}
	fleetGauge.dirty = true
	fleetGauge.mu.Unlock()
}

func fleetGaugeOnFinish(account, agent string) {
	fleetGauge.mu.Lock()
	c := fleetGaugeCell(account, agent)
	if c.Inflight > 0 {
		c.Inflight--
	}
	fleetGauge.dirty = true
	fleetGauge.mu.Unlock()
}

// FleetGaugeSnapshot returns non-zero cells for the /v0/gauge endpoint.
func FleetGaugeSnapshot() []FleetGaugeCell {
	fleetGauge.mu.Lock()
	defer fleetGauge.mu.Unlock()
	out := make([]FleetGaugeCell, 0, len(fleetGauge.cells))
	for k, c := range fleetGauge.cells {
		if c.Inflight == 0 && c.Queued == 0 {
			delete(fleetGauge.cells, k) // idle cells age out of the map
			continue
		}
		out = append(out, *c)
	}
	return out
}

type fleetGaugeEvent struct {
	V     int              `json:"v"`
	Kind  string           `json:"kind"`
	TS    time.Time        `json:"ts"`
	Cells []FleetGaugeCell `json:"cells"`
}

// startFleetGaugePublisher emits a gauge.inflight snapshot at most once per
// second and only when something changed since the last emit.
func startFleetGaugePublisher() {
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			fleetGauge.mu.Lock()
			if !fleetGauge.dirty {
				fleetGauge.mu.Unlock()
				continue
			}
			fleetGauge.dirty = false
			cells := make([]FleetGaugeCell, 0, len(fleetGauge.cells))
			for _, c := range fleetGauge.cells {
				cells = append(cells, *c)
			}
			fleetGauge.mu.Unlock()
			logging.PublishFleetEvent(fleetGaugeEvent{
				V: telemetryContractVersion, Kind: "gauge.inflight",
				TS: time.Now().UTC(), Cells: cells,
			})
		}
	}()
}
