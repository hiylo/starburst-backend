package alerts

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hiylo/starburst-backend/internal/push"
	"github.com/hiylo/starburst-backend/internal/store"
)

// Settings keys persisted in the store's settings KV table.
const (
	SettingEnabled = "alert.enabled"
	SettingCPU     = "alert.cpu_pct"
	SettingMem     = "alert.mem_pct"
	SettingDisk    = "alert.disk_pct"
)

// Defaults applied when no setting has been persisted. Alerts are off by
// default and the thresholds are generous so enabling them does not spam.
const (
	DefaultEnabled = false
	DefaultCPU     = 90.0
	DefaultMem     = 90.0
	DefaultDisk    = 90.0
)

// DefaultInterval is how often the monitor samples host resources.
const DefaultInterval = 60 * time.Second

// Thresholds holds the alerting limits, expressed as percentages (0-100).
type Thresholds struct {
	Enabled bool    `json:"enabled"`
	CPU     float64 `json:"cpuPct"`
	Mem     float64 `json:"memPct"`
	Disk    float64 `json:"diskPct"`
}

// Snapshot is the current alerting config plus the latest samples and per-metric
// alert state, used by the /api/alerts handler.
type Snapshot struct {
	Enabled    bool            `json:"enabled"`
	Thresholds Thresholds      `json:"thresholds"`
	Metrics    Metrics         `json:"metrics"`
	Alerts     map[string]bool `json:"alerts"`
}

// Monitor periodically samples host CPU/memory/disk and broadcasts a push event
// when a metric crosses its configured threshold. It only notifies on state
// transitions (ok -> alert and alert -> ok) to avoid spamming clients.
type Monitor struct {
	st       store.Store
	hub      *push.Hub
	sampler  Sampler
	interval time.Duration

	mu     sync.Mutex
	latest Metrics
	alert  map[string]bool
}

// NewMonitor builds a monitor reading thresholds from st and broadcasting
// through hub at the given sample interval.
func NewMonitor(st store.Store, hub *push.Hub, interval time.Duration) *Monitor {
	return &Monitor{
		st:       st,
		hub:      hub,
		sampler:  NewSampler(),
		interval: interval,
		alert:    make(map[string]bool),
	}
}

// Run samples periodically until ctx is cancelled. The first sample only primes
// the CPU baseline; no alerts are emitted until a real delta is available.
func (m *Monitor) Run(ctx context.Context) {
	if m.interval <= 0 {
		m.interval = DefaultInterval
	}
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	m.sample(ctx, false)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.sample(ctx, true)
		}
	}
}

// Snapshot returns the current thresholds, latest metrics and alert states.
func (m *Monitor) Snapshot(ctx context.Context) Snapshot {
	th := m.loadThresholds(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	states := make(map[string]bool, len(m.alert))
	for k, v := range m.alert {
		states[k] = v
	}
	return Snapshot{
		Enabled:    th.Enabled,
		Thresholds: th,
		Metrics:    m.latest,
		Alerts:     states,
	}
}

func (m *Monitor) sample(ctx context.Context, evaluate bool) {
	th := m.loadThresholds(ctx)
	metrics := m.sampler.Sample()

	m.mu.Lock()
	m.latest = metrics
	if !th.Enabled {
		m.alert = make(map[string]bool)
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	if !evaluate {
		return
	}
	m.evaluate("cpu", metrics.CPU, th.CPU)
	m.evaluate("mem", metrics.Mem, th.Mem)
	m.evaluate("disk", metrics.Disk, th.Disk)
}

func (m *Monitor) evaluate(metric string, value, threshold float64) {
	m.mu.Lock()
	wasAlert := m.alert[metric]
	m.mu.Unlock()

	above := value > threshold
	switch {
	case above && !wasAlert:
		m.mu.Lock()
		m.alert[metric] = true
		m.mu.Unlock()
		m.broadcast(metric, value, threshold, "alert")
	case !above && wasAlert:
		m.mu.Lock()
		m.alert[metric] = false
		m.mu.Unlock()
		m.broadcast(metric, value, threshold, "ok")
	}
}

func (m *Monitor) broadcast(metric string, value, threshold float64, state string) {
	b, err := json.Marshal(map[string]any{
		"metric":    metric,
		"value":     value,
		"threshold": threshold,
		"state":     state,
		"time":      time.Now().UTC(),
	})
	if err != nil {
		return
	}
	sev := push.Info
	if state == "alert" {
		sev = push.Warning
	}
	m.hub.Broadcast(push.Message{Type: "alert.hardware", Payload: b, Severity: sev})
	log.Printf("alerts: %s %.1f%% threshold=%.1f%% state=%s", metric, value, threshold, state)
}

// loadThresholds resolves the effective thresholds from the settings store,
// falling back to the defaults for missing or invalid values.
func (m *Monitor) loadThresholds(ctx context.Context) Thresholds {
	th := Thresholds{
		Enabled: DefaultEnabled,
		CPU:     DefaultCPU,
		Mem:     DefaultMem,
		Disk:    DefaultDisk,
	}
	if v, err := m.st.GetSetting(ctx, SettingEnabled); err == nil {
		t := strings.TrimSpace(v)
		th.Enabled = strings.EqualFold(t, "true") || t == "1"
	}
	if v, err := m.st.GetSetting(ctx, SettingCPU); err == nil {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f > 0 {
			th.CPU = f
		}
	}
	if v, err := m.st.GetSetting(ctx, SettingMem); err == nil {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f > 0 {
			th.Mem = f
		}
	}
	if v, err := m.st.GetSetting(ctx, SettingDisk); err == nil {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f > 0 {
			th.Disk = f
		}
	}
	return th
}
