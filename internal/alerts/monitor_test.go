package alerts

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hiylo/starburst-backend/internal/push"
	"github.com/hiylo/starburst-backend/internal/store"
)

// pushWait bounds how long a test waits for frames it knows were enqueued.
const pushWait = 5 * time.Second

// flushFrame is a sentinel broadcast through the hub. The hub fans out to each
// connection through a single FIFO queue, so once the recorder has seen the
// sentinel it has also seen every alert pushed before it. That lets tests
// assert "exactly these pushes and no more" without sleeping on a quiet window.
const flushFrame = "test.flush"

// --------------------------------------------------------------------------
// test doubles
// --------------------------------------------------------------------------

// fakeSampler is a scripted Sampler. Each call consumes the next Metrics in the
// script; once the script is drained the last value keeps repeating, so tests
// can also let the real ticker loop spin without running out of data. Every
// returned sample is pinged on notify so a test can wait for a sample instead
// of sleeping.
type fakeSampler struct {
	mu     sync.Mutex
	script []Metrics
	last   Metrics
	calls  int
	notify chan Metrics
}

func newFakeSampler(script ...Metrics) *fakeSampler {
	f := &fakeSampler{notify: make(chan Metrics, 64)}
	if len(script) > 0 {
		f.script = append([]Metrics(nil), script...)
		f.last = script[len(script)-1]
	}
	return f
}

func (f *fakeSampler) Sample() Metrics {
	f.mu.Lock()
	m := f.last
	switch {
	case len(f.script) > 1:
		m, f.script = f.script[0], f.script[1:]
	case len(f.script) == 1:
		m, f.script = f.script[0], nil
	}
	f.calls++
	f.mu.Unlock()

	select {
	case f.notify <- m:
	default:
	}
	return m
}

func (f *fakeSampler) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// alertPush mirrors the alert.hardware payload the monitor broadcasts.
type alertPush struct {
	Metric    string  `json:"metric"`
	Value     float64 `json:"value"`
	Threshold float64 `json:"threshold"`
	State     string  `json:"state"`
	Severity  string  `json:"-"`
}

// pushRecorder drains a hub client connection in the background and collects
// the alert.hardware pushes it receives, in order.
type pushRecorder struct {
	hub      *push.Hub
	mu       sync.Mutex
	pushes   []alertPush
	flushed  chan struct{}
	ready    chan struct{}
	stopOnce sync.Once
	stop     chan struct{}
}

// attachRecorder registers a loopback WS client on hub and starts recording.
func attachRecorder(t *testing.T, hub *push.Hub) *pushRecorder {
	t.Helper()
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		hc := hub.Register(conn)
		defer hub.Unregister(hc)
		hc.Write(push.Message{Type: "subscribed"})
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	dialer := websocket.Dialer{}
	conn, _, err := dialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	rec := &pushRecorder{hub: hub, flushed: make(chan struct{}, 1), ready: make(chan struct{}, 1), stop: make(chan struct{})}
	go rec.record(conn)
	// Hub 只把 Broadcast 扇给**已注册**的客户端：注册发生在服务端的 Upgrade 协程里，
	// dial 返回时可能还没完成，此时立刻 sample 的第一条告警就会没人收到（全套件并发
	// 跑时是真 flake）。服务端注册完会先写一帧 subscribed，与后续广播共用同一个
	// 出站队列，所以收到它就是「已挂上 hub」的确定信号。
	select {
	case <-rec.ready:
	case <-time.After(pushWait):
		t.Fatal("recorder client did not register with the hub")
	}
	return rec
}

func (rec *pushRecorder) record(conn *websocket.Conn) {
	for {
		select {
		case <-rec.stop:
			return
		default:
		}
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var msg push.Message
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		switch msg.Type {
		case "subscribed":
			select {
			case rec.ready <- struct{}{}:
			default:
			}
		case flushFrame:
			select {
			case rec.flushed <- struct{}{}:
			default:
			}
		case "alert.hardware":
			var p alertPush
			if err := json.Unmarshal(msg.Payload, &p); err != nil {
				continue
			}
			p.Severity = msg.Severity
			rec.mu.Lock()
			rec.pushes = append(rec.pushes, p)
			rec.mu.Unlock()
		}
	}
}

// settle flushes the hub queue and returns every alert push recorded so far.
func (rec *pushRecorder) settle(t *testing.T) []alertPush {
	t.Helper()
	rec.hub.Broadcast(push.Message{Type: flushFrame})
	select {
	case <-rec.flushed:
	case <-time.After(pushWait):
		t.Fatal("flush marker did not come back")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]alertPush(nil), rec.pushes...)
}

func (rec *pushRecorder) close() { rec.stopOnce.Do(func() { close(rec.stop) }) }

// expectPushes compares recorded pushes against the wanted metric/state/value
// triples; thresholds are not part of the expectation on purpose.
func expectPushes(t *testing.T, got, want []alertPush) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("pushes = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Metric != want[i].Metric || got[i].State != want[i].State || got[i].Value != want[i].Value {
			t.Fatalf("push %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// --------------------------------------------------------------------------
// helpers
// --------------------------------------------------------------------------

// newTestStore opens a throwaway SQLite store, as the other package tests do.
func newTestStore(t *testing.T) store.Store {
	t.Helper()
	dsn := store.SQLiteDSN(filepath.Join(t.TempDir(), "test.db"))
	st, err := store.OpenFromConfig(context.Background(), "sqlite", dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// setSettings replaces the whole alert setting block, clearing keys that the
// caller leaves out so cases cannot leak into each other.
func setSettings(t *testing.T, st store.Store, kv map[string]string) {
	t.Helper()
	ctx := context.Background()
	for _, key := range []string{SettingEnabled, SettingCPU, SettingMem, SettingDisk} {
		if _, ok := kv[key]; !ok {
			if err := st.SetSetting(ctx, key, ""); err != nil {
				t.Fatalf("reset %s: %v", key, err)
			}
		}
	}
	for k, v := range kv {
		if err := st.SetSetting(ctx, k, v); err != nil {
			t.Fatalf("set %s: %v", k, err)
		}
	}
}

// newMonitorForTest builds a monitor over a real store with a scripted sampler
// and a hub nobody listens on (broadcasts are then dropped, not panicked).
func newMonitorForTest(t *testing.T, st store.Store, interval time.Duration, script ...Metrics) *Monitor {
	t.Helper()
	hub := push.NewHub()
	go hub.Run()
	t.Cleanup(hub.Stop)

	m := NewMonitor(st, hub, interval)
	m.sampler = newFakeSampler(script...)
	return m
}

// newMonitorWithRecorder is newMonitorForTest plus an attached push recorder.
func newMonitorWithRecorder(t *testing.T, st store.Store, interval time.Duration, script ...Metrics) (*Monitor, *fakeSampler, *pushRecorder) {
	t.Helper()
	m := newMonitorForTest(t, st, interval, script...)
	rec := attachRecorder(t, m.hub)
	t.Cleanup(rec.close)
	return m, m.sampler.(*fakeSampler), rec
}

func cpuSeries(values ...float64) []Metrics {
	out := make([]Metrics, 0, len(values))
	for _, v := range values {
		out = append(out, Metrics{CPU: v})
	}
	return out
}

func pushes(spec ...string) []alertPush {
	out := make([]alertPush, 0, len(spec))
	for _, s := range spec {
		// spec entries look like "cpu:alert:60".
		parts := strings.Split(s, ":")
		if len(parts) != 3 {
			panic("bad push spec " + s)
		}
		v, err := strconv.ParseFloat(parts[2], 64)
		if err != nil {
			panic("bad push spec " + s)
		}
		out = append(out, alertPush{Metric: parts[0], State: parts[1], Value: v})
	}
	return out
}

// waitForSample blocks until the sampler has been invoked at least n times.
func waitForSample(t *testing.T, f *fakeSampler, n int) {
	t.Helper()
	deadline := time.After(pushWait)
	for f.callCount() < n {
		select {
		case <-f.notify:
		case <-deadline:
			t.Fatalf("sampler called %d times, want >= %d", f.callCount(), n)
		}
	}
}

// --------------------------------------------------------------------------
// threshold parsing
// --------------------------------------------------------------------------

func TestLoadThresholdsFromSettings(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	m := newMonitorForTest(t, st, DefaultInterval)

	tests := []struct {
		name     string
		settings map[string]string
		want     Thresholds
	}{
		{
			name: "no settings fall back to defaults and stay disabled",
			want: Thresholds{Enabled: false, CPU: 90, Mem: 90, Disk: 90},
		},
		{
			name:     "enabled true",
			settings: map[string]string{SettingEnabled: "true"},
			want:     Thresholds{Enabled: true, CPU: 90, Mem: 90, Disk: 90},
		},
		{
			name:     "enabled is case insensitive and trimmed",
			settings: map[string]string{SettingEnabled: "  TRUE "},
			want:     Thresholds{Enabled: true, CPU: 90, Mem: 90, Disk: 90},
		},
		{
			name:     "enabled one",
			settings: map[string]string{SettingEnabled: "1"},
			want:     Thresholds{Enabled: true, CPU: 90, Mem: 90, Disk: 90},
		},
		{
			name:     "yes is not accepted as enabled",
			settings: map[string]string{SettingEnabled: "yes"},
			want:     Thresholds{Enabled: false, CPU: 90, Mem: 90, Disk: 90},
		},
		{
			name:     "empty enabled value",
			settings: map[string]string{SettingEnabled: ""},
			want:     Thresholds{Enabled: false, CPU: 90, Mem: 90, Disk: 90},
		},
		{
			name:     "valid cpu threshold only",
			settings: map[string]string{SettingEnabled: "true", SettingCPU: "50"},
			want:     Thresholds{Enabled: true, CPU: 50, Mem: 90, Disk: 90},
		},
		{
			name:     "fractional threshold with padding",
			settings: map[string]string{SettingEnabled: "true", SettingMem: " 87.5\t"},
			want:     Thresholds{Enabled: true, CPU: 90, Mem: 87.5, Disk: 90},
		},
		{
			name:     "empty threshold falls back",
			settings: map[string]string{SettingEnabled: "true", SettingCPU: ""},
			want:     Thresholds{Enabled: true, CPU: 90, Mem: 90, Disk: 90},
		},
		{
			name:     "non numeric threshold falls back",
			settings: map[string]string{SettingEnabled: "true", SettingCPU: "ninety"},
			want:     Thresholds{Enabled: true, CPU: 90, Mem: 90, Disk: 90},
		},
		{
			name:     "negative threshold falls back",
			settings: map[string]string{SettingEnabled: "true", SettingMem: "-5"},
			want:     Thresholds{Enabled: true, CPU: 90, Mem: 90, Disk: 90},
		},
		{
			name:     "zero threshold falls back rather than disabling the metric",
			settings: map[string]string{SettingEnabled: "true", SettingDisk: "0"},
			want:     Thresholds{Enabled: true, CPU: 90, Mem: 90, Disk: 90},
		},
		{
			name:     "out of range percentage is taken verbatim",
			settings: map[string]string{SettingEnabled: "true", SettingCPU: "250"},
			want:     Thresholds{Enabled: true, CPU: 250, Mem: 90, Disk: 90},
		},
		{
			name:     "nan falls back",
			settings: map[string]string{SettingEnabled: "true", SettingCPU: "NaN"},
			want:     Thresholds{Enabled: true, CPU: 90, Mem: 90, Disk: 90},
		},
		{
			name:     "infinity is accepted and mutes the metric",
			settings: map[string]string{SettingEnabled: "true", SettingCPU: "+Inf"},
			want:     Thresholds{Enabled: true, CPU: math.Inf(1), Mem: 90, Disk: 90},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setSettings(t, st, tc.settings)

			got := m.loadThresholds(ctx)
			if math.IsNaN(got.CPU) || math.IsNaN(got.Mem) || math.IsNaN(got.Disk) {
				t.Fatalf("thresholds must never be NaN: %+v", got)
			}
			if tc.name == "infinity is accepted and mutes the metric" {
				if !math.IsInf(got.CPU, 1) {
					t.Fatalf("cpu = %v, want +Inf", got.CPU)
				}
				got.CPU, tc.want.CPU = 0, 0
			}
			if got != tc.want {
				t.Fatalf("thresholds = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestThresholdsDriveTheVerdict checks that a parsed value really is the line
// the metric is compared against, including the mutes-by-huge-value case.
func TestThresholdsDriveTheVerdict(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	tests := []struct {
		name       string
		cpuSetting string
		cpuValue   float64
		wantPush   bool
	}{
		{name: "in range breach", cpuSetting: "50", cpuValue: 51, wantPush: true},
		{name: "in range below", cpuSetting: "50", cpuValue: 49, wantPush: false},
		{name: "exact boundary is not a breach", cpuSetting: "50", cpuValue: 50, wantPush: false},
		{name: "over-range setting mutes the metric", cpuSetting: "250", cpuValue: 100, wantPush: false},
		{name: "garbage setting uses the default", cpuSetting: "abc", cpuValue: 95, wantPush: true},
		{name: "zero setting uses the default", cpuSetting: "0", cpuValue: 95, wantPush: true},
		{name: "zero setting with low load stays quiet", cpuSetting: "0", cpuValue: 5, wantPush: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setSettings(t, st, map[string]string{SettingEnabled: "true", SettingCPU: tc.cpuSetting})
			m, _, rec := newMonitorWithRecorder(t, st, DefaultInterval, Metrics{CPU: tc.cpuValue})
			m.sample(ctx, true)

			got := rec.settle(t)
			if !tc.wantPush {
				expectPushes(t, got, nil)
				return
			}
			if len(got) != 1 {
				t.Fatalf("pushes = %+v, want exactly one alert", got)
			}
			want := Thresholds{Enabled: true, Mem: DefaultMem, Disk: DefaultDisk}
			if f, err := strconv.ParseFloat(tc.cpuSetting, 64); err == nil && f > 0 {
				want.CPU = f
			} else {
				want.CPU = DefaultCPU
			}
			if got[0].Threshold != want.CPU {
				t.Fatalf("threshold in payload = %v, want %v", got[0].Threshold, want.CPU)
			}
			if got[0].Severity != push.Warning {
				t.Fatalf("severity = %q, want warning", got[0].Severity)
			}
		})
	}
}

// TestLoadThresholdsStoreErrorIsSafe pins the degradation path: when the store
// cannot answer, the monitor falls back to the disabled defaults instead of
// panicking or alerting.
func TestLoadThresholdsStoreErrorIsSafe(t *testing.T) {
	st := newTestStore(t)
	m, _, rec := newMonitorWithRecorder(t, st, DefaultInterval, Metrics{CPU: 100})

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	got := m.loadThresholds(cancelled)
	if got != (Thresholds{Enabled: false, CPU: DefaultCPU, Mem: DefaultMem, Disk: DefaultDisk}) {
		t.Fatalf("thresholds = %+v, want defaults", got)
	}

	// A full sample cycle on a dead context must be a harmless no-op.
	m.sample(cancelled, true)
	expectPushes(t, rec.settle(t), nil)
	if snap := m.Snapshot(cancelled); len(snap.Alerts) != 0 {
		t.Fatalf("alerts recorded while disabled: %+v", snap.Alerts)
	}
}

// --------------------------------------------------------------------------
// state transitions / de-duplication
// --------------------------------------------------------------------------

// TestAlertTransitionsBroadcastOncePerCrossing is the core de-duplication
// contract, checked after every single sample: one broadcast on the ok->alert
// edge, nothing while the metric stays above the threshold, one broadcast on
// the alert->ok edge, nothing on the exact boundary.
func TestAlertTransitionsBroadcastOncePerCrossing(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	setSettings(t, st, map[string]string{SettingEnabled: "true", SettingCPU: "50"})

	series := cpuSeries(10, 60, 70, 65, 20, 10, 50)
	want := [][]alertPush{
		nil,
		pushes("cpu:alert:60"),
		pushes("cpu:alert:60"), // 70: still breaching, must not re-broadcast
		pushes("cpu:alert:60"), // 65: still breaching
		pushes("cpu:alert:60", "cpu:ok:20"),
		pushes("cpu:alert:60", "cpu:ok:20"),
		pushes("cpu:alert:60", "cpu:ok:20"), // exactly at the threshold
	}
	if len(series) != len(want) {
		t.Fatalf("series and expectations out of sync")
	}

	m, _, rec := newMonitorWithRecorder(t, st, DefaultInterval, series...)
	for i := range series {
		m.sample(ctx, true)
		expectPushes(t, rec.settle(t), want[i])
	}

	snap := m.Snapshot(ctx)
	if snap.Alerts["cpu"] {
		t.Fatalf("cpu still marked alerting after recovery: %+v", snap.Alerts)
	}
	if snap.Metrics.CPU != 50 {
		t.Fatalf("snapshot metrics = %+v, want last sample cpu 50", snap.Metrics)
	}
}

// TestTransitionsOnRepeatedFlapping checks that every edge of a
// breach/recover cycle broadcasts exactly once and severities alternate.
func TestTransitionsOnRepeatedFlapping(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	setSettings(t, st, map[string]string{SettingEnabled: "true", SettingCPU: "80"})

	series := cpuSeries(81, 20, 99, 10, 95)
	m, _, rec := newMonitorWithRecorder(t, st, DefaultInterval, series...)
	for range series {
		m.sample(ctx, true)
	}

	want := pushes("cpu:alert:81", "cpu:ok:20", "cpu:alert:99", "cpu:ok:10", "cpu:alert:95")
	got := rec.settle(t)
	expectPushes(t, got, want)
	for i, p := range got {
		want := push.Warning
		if p.State == "ok" {
			want = push.Info
		}
		if p.Severity != want {
			t.Fatalf("push %d severity = %q, want %q", i, p.Severity, want)
		}
	}
	if len(got) != 5 {
		t.Fatalf("got %d pushes, want 5", len(got))
	}
}

// TestMetricsAreTrackedIndependently checks that each metric keeps its own
// state and that a breach on one does not suppress the others.
func TestMetricsAreTrackedIndependently(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	setSettings(t, st, map[string]string{
		SettingEnabled: "true", SettingCPU: "50", SettingMem: "50", SettingDisk: "50",
	})

	series := []Metrics{
		{CPU: 60, Mem: 10, Disk: 70},
		{CPU: 65, Mem: 55, Disk: 71},
		{CPU: 5, Mem: 5, Disk: 5},
	}
	m, _, rec := newMonitorWithRecorder(t, st, DefaultInterval, series...)
	for range series {
		m.sample(ctx, true)
	}

	want := pushes("cpu:alert:60", "disk:alert:70", "mem:alert:55",
		"cpu:ok:5", "mem:ok:5", "disk:ok:5")
	expectPushes(t, rec.settle(t), want)

	alerts := m.Snapshot(ctx).Alerts
	if len(alerts) != 3 {
		t.Fatalf("snapshot alerts = %+v, want one entry per metric", alerts)
	}
	for _, k := range []string{"cpu", "mem", "disk"} {
		if alerts[k] {
			t.Fatalf("%s still alerting after recovery: %+v", k, alerts)
		}
	}
}

// TestDisabledSilencesBroadcastsAndClearsState covers alert.enabled=false:
// metrics keep being sampled, the alert map is wiped, no push goes out.
func TestDisabledSilencesBroadcastsAndClearsState(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	setSettings(t, st, map[string]string{SettingEnabled: "true", SettingCPU: "50"})

	m, _, rec := newMonitorWithRecorder(t, st, DefaultInterval, Metrics{CPU: 99})
	m.sample(ctx, true)
	expectPushes(t, rec.settle(t), pushes("cpu:alert:99"))

	setSettings(t, st, map[string]string{SettingEnabled: "false", SettingCPU: "50"})
	for i := 0; i < 3; i++ {
		m.sample(ctx, true)
	}
	expectPushes(t, rec.settle(t), pushes("cpu:alert:99")) // nothing new

	snap := m.Snapshot(ctx)
	if snap.Enabled || len(snap.Alerts) != 0 {
		t.Fatalf("snapshot after disable = %+v, want disabled with empty alerts", snap)
	}
	if snap.Metrics.CPU != 99 {
		t.Fatalf("metrics stopped updating while disabled: %+v", snap.Metrics)
	}
}

// TestReEnableReAnnouncesBreach pins that clearing the state on disable makes
// a still-breaching metric announce itself again once alerts are turned back
// on, rather than staying muted forever.
func TestReEnableReAnnouncesBreach(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	setSettings(t, st, map[string]string{SettingEnabled: "true", SettingCPU: "50"})

	m, _, rec := newMonitorWithRecorder(t, st, DefaultInterval, Metrics{CPU: 99})
	m.sample(ctx, true)
	expectPushes(t, rec.settle(t), pushes("cpu:alert:99"))

	setSettings(t, st, map[string]string{SettingEnabled: "false", SettingCPU: "50"})
	m.sample(ctx, true)
	setSettings(t, st, map[string]string{SettingEnabled: "true", SettingCPU: "50"})
	m.sample(ctx, true)

	expectPushes(t, rec.settle(t), pushes("cpu:alert:99", "cpu:alert:99"))
}

// TestDefaultConfigIsSilent pins the shipped default: with nothing usable
// persisted, even a fully loaded host must not produce a single push.
func TestDefaultConfigIsSilent(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	setSettings(t, st, map[string]string{})

	series := cpuSeries(0, 100, 100, 91, 0)
	m, fs, rec := newMonitorWithRecorder(t, st, DefaultInterval, series...)
	for range series {
		m.sample(ctx, true)
	}
	expectPushes(t, rec.settle(t), nil)

	snap := m.Snapshot(ctx)
	if snap.Enabled || len(snap.Alerts) != 0 {
		t.Fatalf("default snapshot = %+v", snap)
	}
	if fs.callCount() != len(series) {
		t.Fatalf("sampler calls = %d, want %d", fs.callCount(), len(series))
	}
}

// --------------------------------------------------------------------------
// Run loop
// --------------------------------------------------------------------------

// TestRunFirstSampleOnlyPrimesBaseline pins the documented contract that the
// first sample never evaluates alerts.
func TestRunFirstSampleOnlyPrimesBaseline(t *testing.T) {
	st := newTestStore(t)
	setSettings(t, st, map[string]string{SettingEnabled: "true", SettingCPU: "10"})

	// One hour interval: the ticker cannot fire, so only the priming sample
	// runs before the context is cancelled.
	m, fs, rec := newMonitorWithRecorder(t, st, time.Hour, Metrics{CPU: 99})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()
	waitForSample(t, fs, 1)

	// Reading under m.mu proves the priming sample finished its state update,
	// without guessing at wall-clock timing.
	m.mu.Lock()
	latest, alertCount := m.latest, len(m.alert)
	m.mu.Unlock()
	if latest.CPU != 99 {
		t.Fatalf("priming sample did not store metrics: %+v", latest)
	}
	if alertCount != 0 {
		t.Fatalf("first sample evaluated alert state: %d entries", alertCount)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(pushWait):
		t.Fatal("Run did not return after ctx cancel")
	}
	expectPushes(t, rec.settle(t), nil)
}

// TestRunLoopsUntilCancelled drives the ticker loop with a tiny interval: it
// must sample repeatedly, broadcast a breach exactly once and stop cleanly.
func TestRunLoopsUntilCancelled(t *testing.T) {
	st := newTestStore(t)
	setSettings(t, st, map[string]string{SettingEnabled: "true", SettingCPU: "50"})

	m, fs, rec := newMonitorWithRecorder(t, st, 2*time.Millisecond, Metrics{CPU: 90})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	waitForSample(t, fs, 3)
	expectPushes(t, rec.settle(t), pushes("cpu:alert:90")) // one push for many ticks

	cancel()
	select {
	case <-done:
	case <-time.After(pushWait):
		t.Fatal("Run did not return after ctx cancel")
	}
	calls := fs.callCount()
	if calls < 3 {
		t.Fatalf("ticker loop sampled only %d times", calls)
	}
	// Once Run has returned nothing may sample any more.
	expectPushes(t, rec.settle(t), pushes("cpu:alert:90"))
	if after := fs.callCount(); after < calls {
		t.Fatalf("sampler call count went backwards (%d -> %d)", calls, after)
	}
}

// TestRunAppliesDefaultInterval covers the non-positive interval guard.
func TestRunAppliesDefaultInterval(t *testing.T) {
	st := newTestStore(t)
	setSettings(t, st, map[string]string{SettingEnabled: "true", SettingCPU: "50"})
	m, fs, _ := newMonitorWithRecorder(t, st, 0)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()
	waitForSample(t, fs, 1)
	cancel()
	select {
	case <-done:
	case <-time.After(pushWait):
		t.Fatal("Run did not return after ctx cancel")
	}
	if m.interval != DefaultInterval {
		t.Fatalf("interval = %v, want default %v", m.interval, DefaultInterval)
	}
}

// --------------------------------------------------------------------------
// snapshot / wiring
// --------------------------------------------------------------------------

func TestSnapshotShape(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	setSettings(t, st, map[string]string{SettingEnabled: "true", SettingCPU: "40"})

	m, _, rec := newMonitorWithRecorder(t, st, DefaultInterval, Metrics{CPU: 55, Mem: 12, Disk: 88})
	m.sample(ctx, true)
	expectPushes(t, rec.settle(t), pushes("cpu:alert:55"))

	snap := m.Snapshot(ctx)
	if !snap.Enabled || !snap.Thresholds.Enabled {
		t.Fatalf("snapshot not enabled: %+v", snap)
	}
	if snap.Thresholds.CPU != 40 || snap.Thresholds.Mem != DefaultMem {
		t.Fatalf("snapshot thresholds = %+v", snap.Thresholds)
	}
	if snap.Metrics != (Metrics{CPU: 55, Mem: 12, Disk: 88}) {
		t.Fatalf("snapshot metrics = %+v", snap.Metrics)
	}
	if !snap.Alerts["cpu"] {
		t.Fatalf("snapshot alerts = %+v, want cpu alerting", snap.Alerts)
	}
	// The returned map is a copy: mutating it must not corrupt monitor state.
	snap.Alerts["cpu"] = false
	if !m.Snapshot(ctx).Alerts["cpu"] {
		t.Fatalf("snapshot alerts alias the monitor state")
	}
	// JSON shape consumed by /api/alerts must stay stable.
	data, err := json.Marshal(m.Snapshot(ctx))
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	for _, key := range []string{"enabled", "thresholds", "metrics", "alerts"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("snapshot json missing %q: %s", key, data)
		}
	}
}

func TestNewMonitorWiresDefaults(t *testing.T) {
	st := newTestStore(t)
	hub := push.NewHub()
	m := NewMonitor(st, hub, DefaultInterval)
	if m.st != st || m.hub != hub {
		t.Fatalf("monitor dependencies not wired")
	}
	if m.sampler == nil {
		t.Fatalf("monitor has no sampler")
	}
	if m.alert == nil {
		t.Fatalf("alert state map is nil")
	}
	if m.interval != DefaultInterval {
		t.Fatalf("interval = %v", m.interval)
	}
	if want := (Thresholds{Enabled: DefaultEnabled, CPU: DefaultCPU, Mem: DefaultMem, Disk: DefaultDisk}); !reflect.DeepEqual(m.loadThresholds(context.Background()), want) {
		t.Fatalf("fresh monitor thresholds = %+v, want defaults", m.loadThresholds(context.Background()))
	}
}

// TestBroadcastWithNoClientsDoesNotPanic mirrors the hub test: the monitor must
// survive firing into an empty hub.
func TestBroadcastWithNoClientsDoesNotPanic(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	setSettings(t, st, map[string]string{SettingEnabled: "true", SettingCPU: "10"})

	m := newMonitorForTest(t, st, DefaultInterval, Metrics{CPU: 99})
	for i := 0; i < 3; i++ {
		m.sample(ctx, true)
	}
	if !m.Snapshot(ctx).Alerts["cpu"] {
		t.Fatalf("expected cpu to stay alerting without clients attached")
	}
}
