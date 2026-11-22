package controller

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	profilingv1alpha1 "github.com/a-kash-singh/bolometer/api/v1alpha1"
	"github.com/a-kash-singh/bolometer/internal/metrics"
)

// ---------------------------------------------------------------------------
// Fake clock
// ---------------------------------------------------------------------------

// fakeClock is a manually advanced clock. Step fires every timer whose
// deadline has been reached. It tracks pending timers so tests can tell when
// every schedule loop has finished its work and is waiting again.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	seq    int
	timers map[*fakeTimer]struct{}
}

type fakeTimer struct {
	clk      *fakeClock
	c        chan time.Time
	deadline time.Time
	seq      int
}

func newFakeClock() *fakeClock {
	return &fakeClock{
		now:    time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		timers: make(map[*fakeTimer]struct{}),
	}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTimer(d time.Duration) timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	t := &fakeTimer{clk: c, c: make(chan time.Time, 1), deadline: c.now.Add(d), seq: c.seq}
	if d <= 0 {
		t.c <- c.now
		return t
	}
	c.timers[t] = struct{}{}
	return t
}

func (t *fakeTimer) C() <-chan time.Time { return t.c }

func (t *fakeTimer) Stop() bool {
	t.clk.mu.Lock()
	defer t.clk.mu.Unlock()
	_, pending := t.clk.timers[t]
	delete(t.clk.timers, t)
	return pending
}

// Step advances the clock and fires every timer that is due.
func (c *fakeClock) Step(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for t := range c.timers {
		if !t.deadline.After(c.now) {
			t.c <- c.now
			delete(c.timers, t)
		}
	}
}

// settled reports whether exactly want timers are pending, all created after
// the timer with sequence number since.
func (c *fakeClock) settled(want, since int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.timers) != want {
		return false
	}
	for t := range c.timers {
		if t.seq <= since {
			return false
		}
	}
	return true
}

func (c *fakeClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

func (c *fakeClock) lastSeq() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seq
}

// ---------------------------------------------------------------------------
// Fake metrics and capture
// ---------------------------------------------------------------------------

type fakeMetrics struct {
	clk   *fakeClock // optional: record check times
	start time.Time

	mu         sync.Mutex
	cpuPercent float64
	cpuByPod   map[string]float64 // pod name -> CPU %, overrides cpuPercent
	calls      int
	checkTimes map[string][]time.Duration // pod name -> times since start
}

func (f *fakeMetrics) GetPodMetrics(_ context.Context, _, podName string, _ *corev1.Pod) (*metrics.PodMetrics, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.clk != nil {
		if f.checkTimes == nil {
			f.checkTimes = map[string][]time.Duration{}
		}
		f.checkTimes[podName] = append(f.checkTimes[podName], f.clk.Now().Sub(f.start))
	}
	cpu := f.cpuPercent
	if p, ok := f.cpuByPod[podName]; ok {
		cpu = p
	}
	return &metrics.PodMetrics{CPUUsagePercent: cpu}, nil
}

func (f *fakeMetrics) setPodCPU(podName string, p float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cpuByPod == nil {
		f.cpuByPod = map[string]float64{}
	}
	f.cpuByPod[podName] = p
}

func (f *fakeMetrics) podCheckTimes(podName string) []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.checkTimes[podName]...)
}

func (f *fakeMetrics) setCPU(p float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cpuPercent = p
}

func (f *fakeMetrics) checks() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type captureCall struct {
	at     time.Duration // time since the harness started
	pod    string
	reason string
	bucket string
	ctx    context.Context
}

type fakeCapturer struct {
	clk   *fakeClock
	start time.Time

	mu          sync.Mutex
	calls       []captureCall
	inFlight    int
	maxInFlight int
	// block, when non-nil, makes captures wait until it is closed or their
	// context is cancelled. If blockPod is set, only that pod's captures wait.
	block    chan struct{}
	blockPod string
	// failPods makes captures of these pods fail.
	failPods map[string]bool
	started  chan captureCall
	// result is what a successful capture reports; zero means 1 profile
	// captured and uploaded.
	result captureResult
}

func (f *fakeCapturer) capture(ctx context.Context, pod *corev1.Pod, config *profilingv1alpha1.ProfilingConfig, reason string) (captureResult, error) {
	call := captureCall{
		at:     f.clk.Now().Sub(f.start),
		pod:    pod.Name,
		reason: reason,
		bucket: config.Spec.S3Config.Bucket,
		ctx:    ctx,
	}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.inFlight++
	if f.inFlight > f.maxInFlight {
		f.maxInFlight = f.inFlight
	}
	block, started := f.block, f.started
	if f.blockPod != "" && f.blockPod != pod.Name {
		block = nil
	}
	fail := f.failPods[pod.Name]
	res := f.result
	if res == (captureResult{}) {
		res = captureResult{Profiles: 1, Uploaded: 1}
	}
	f.mu.Unlock()

	if started != nil {
		started <- call
	}
	var err error
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			err = ctx.Err()
		}
	}

	if err == nil && fail {
		err = fmt.Errorf("port-forward to %s failed", pod.Name)
	}
	if err != nil {
		res = captureResult{}
	}

	f.mu.Lock()
	f.inFlight--
	f.mu.Unlock()
	return res, err
}

// podTimes returns when captures of the pod started.
func (f *fakeCapturer) podTimes(podName string) []time.Duration {
	var out []time.Duration
	for _, c := range f.snapshot() {
		if c.pod == podName {
			out = append(out, c.at)
		}
	}
	return out
}

func (f *fakeCapturer) snapshot() []captureCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]captureCall(nil), f.calls...)
}

// onDemandTimes returns when on-demand captures started.
func (f *fakeCapturer) onDemandTimes() []time.Duration { return f.times(true) }

// thresholdTimes returns when threshold-triggered captures started.
func (f *fakeCapturer) thresholdTimes() []time.Duration { return f.times(false) }

func (f *fakeCapturer) times(onDemand bool) []time.Duration {
	var out []time.Duration
	for _, c := range f.snapshot() {
		if (c.reason == "on-demand") == onDemand {
			out = append(out, c.at)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// monitorHarness drives a reconciler with a fake clock the way
// controller-runtime would: Reconcile runs again when its RequeueAfter
// expires, and right after every capture (the status update that follows a
// capture produces a watch event).
type monitorHarness struct {
	t        *testing.T
	r        *ProfilingConfigReconciler
	clk      *fakeClock
	metrics  *fakeMetrics
	capturer *fakeCapturer
	req      ctrl.Request
	start    time.Time

	// wantTimers is the number of schedule loops that arm a timer when idle:
	// 1 for thresholds, plus 1 while on-demand is enabled.
	wantTimers    int
	nextReconcile time.Time
	reconciles    int
}

func newMonitorHarness(t *testing.T, config *profilingv1alpha1.ProfilingConfig) *monitorHarness {
	t.Helper()
	r := setupTestReconciler(config)
	t.Cleanup(r.baseCancel)

	pod := createTestPod("app-0", config.Namespace, true)
	if _, err := r.Clientset.CoreV1().Pods(config.Namespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	clk := newFakeClock()
	fm := &fakeMetrics{clk: clk, start: clk.Now()}
	fc := &fakeCapturer{clk: clk, start: clk.Now()}
	r.clock = clk
	r.podWatcher.clock = clk
	r.metricsCollector = fm
	r.captureFn = fc.capture

	wantTimers := 1
	if config.Spec.OnDemand != nil && config.Spec.OnDemand.Enabled {
		wantTimers = 2
	}
	return &monitorHarness{
		t:          t,
		r:          r,
		clk:        clk,
		metrics:    fm,
		capturer:   fc,
		req:        ctrl.Request{NamespacedName: types.NamespacedName{Name: config.Name, Namespace: config.Namespace}},
		start:      clk.Now(),
		wantTimers: wantTimers,
	}
}

func (h *monitorHarness) elapsed() time.Duration { return h.clk.Now().Sub(h.start) }

func (h *monitorHarness) reconcile() {
	h.t.Helper()
	res, err := h.r.Reconcile(context.Background(), h.req)
	if err != nil {
		h.t.Fatalf("Reconcile at %v: %v", h.elapsed(), err)
	}
	h.reconciles++
	if res.RequeueAfter > 0 {
		h.nextReconcile = h.clk.Now().Add(res.RequeueAfter)
	}
}

// begin performs the first reconcile and waits for the monitor to be idle.
func (h *monitorHarness) begin() {
	h.t.Helper()
	mark := h.clk.lastSeq()
	h.reconcile()
	h.waitFor("monitor to start", func() bool { return h.clk.settled(h.wantTimers, mark) })
}

// advance moves the clock forward in steps, letting due schedules run and
// reconciling whenever controller-runtime would.
func (h *monitorHarness) advance(total, step time.Duration) {
	h.t.Helper()
	for moved := time.Duration(0); moved < total; moved += step {
		captures := len(h.capturer.snapshot())
		h.clk.Step(step)
		h.waitIdle()
		if len(h.capturer.snapshot()) > captures || !h.clk.Now().Before(h.nextReconcile) {
			h.reconcile()
		}
	}
}

// editSpec updates the ProfilingConfig spec through the API, reconciles, and
// waits for every schedule loop to re-arm against the new spec.
func (h *monitorHarness) editSpec(wantTimers int, mutate func(*profilingv1alpha1.ProfilingConfigSpec)) {
	h.t.Helper()
	cfg := &profilingv1alpha1.ProfilingConfig{}
	if err := h.r.Get(context.Background(), h.req.NamespacedName, cfg); err != nil {
		h.t.Fatalf("get config: %v", err)
	}
	mutate(&cfg.Spec)
	if err := h.r.Update(context.Background(), cfg); err != nil {
		h.t.Fatalf("update config: %v", err)
	}
	h.wantTimers = wantTimers
	mark := h.clk.lastSeq()
	h.reconcile()
	h.waitFor("monitor to pick up spec change", func() bool {
		return h.clk.settled(h.wantTimers, mark) && h.capturesInFlight() == 0
	})
}

// waitIdle waits until every schedule loop has re-armed its timer and no
// capture is running (threshold captures run in the background).
func (h *monitorHarness) waitIdle() {
	h.t.Helper()
	h.waitFor("schedule loops to go idle", func() bool {
		return h.clk.pending() == h.wantTimers && h.capturesInFlight() == 0
	})
}

func (h *monitorHarness) capturesInFlight() int {
	pw := h.r.podWatcher
	pw.mu.RLock()
	defer pw.mu.RUnlock()
	return len(pw.capturing)
}

func (h *monitorHarness) addPod(name string) {
	h.t.Helper()
	pod := createTestPod(name, h.req.Namespace, true)
	if _, err := h.r.Clientset.CoreV1().Pods(h.req.Namespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		h.t.Fatalf("create pod: %v", err)
	}
}

func (h *monitorHarness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out at %v waiting for %s", h.elapsed(), what)
		}
		time.Sleep(time.Millisecond)
	}
}

func (h *monitorHarness) monitor() *configMonitor {
	h.r.monitorsMu.Lock()
	defer h.r.monitorsMu.Unlock()
	return h.r.activeMonitors[h.req.NamespacedName.String()]
}

func seconds(s ...int) []time.Duration {
	out := make([]time.Duration, len(s))
	for i, v := range s {
		out[i] = time.Duration(v) * time.Second
	}
	return out
}

func assertTimes(t *testing.T, what string, got, want []time.Duration) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: got %v, want %v", what, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Regression tests: schedules must not depend on reconcile frequency
// ---------------------------------------------------------------------------

// A checkIntervalSeconds longer than the 30s requeue used to be reset by every
// reconcile, so threshold checks never ran.
func TestMonitor_ThresholdCheckIntervalLongerThanRequeue(t *testing.T) {
	config := createTestProfilingConfig("cfg", "default")
	config.Spec.Thresholds.CheckIntervalSeconds = 60
	h := newMonitorHarness(t, config)
	h.metrics.setCPU(10) // below threshold: count checks only

	h.begin()
	for _, want := range []int{0, 1, 1, 2, 2, 3} { // t = 30s, 60s, ... 180s
		h.advance(30*time.Second, 30*time.Second)
		if got := h.metrics.checks(); got != want {
			t.Fatalf("after %v (%d reconciles): %d threshold checks, want %d", h.elapsed(), h.reconciles, got, want)
		}
	}
	if h.reconciles < 6 {
		t.Fatalf("harness reconciled only %d times; test would not exercise the requeue", h.reconciles)
	}
}

// The same applies to on-demand mode at its default interval of 35s.
func TestMonitor_OnDemandDefaultIntervalWithRequeue(t *testing.T) {
	config := createTestProfilingConfig("cfg", "default")
	config.Spec.OnDemand = &profilingv1alpha1.OnDemandConfig{Enabled: true, IntervalSeconds: 35}
	h := newMonitorHarness(t, config)
	h.metrics.setCPU(10)

	h.begin()
	h.advance(110*time.Second, 5*time.Second)

	assertTimes(t, "on-demand capture times", h.capturer.onDemandTimes(), seconds(35, 70, 105))
}

// Frequent reconciles (requeue plus one per status update) must not make
// intervals drift or stall.
func TestMonitor_IntervalsUnaffectedByReconcileFrequency(t *testing.T) {
	config := createTestProfilingConfig("cfg", "default")
	config.Spec.Thresholds.CheckIntervalSeconds = 45
	config.Spec.OnDemand = &profilingv1alpha1.OnDemandConfig{Enabled: true, IntervalSeconds: 40}
	h := newMonitorHarness(t, config)
	h.metrics.setCPU(10)

	h.begin()
	for h.elapsed() < 180*time.Second {
		h.advance(5*time.Second, 5*time.Second)
		h.reconcile() // an extra reconcile every 5s on top of the normal ones
	}

	assertTimes(t, "on-demand capture times", h.capturer.onDemandTimes(), seconds(40, 80, 120, 160))
	if got := h.metrics.checks(); got != 4 { // 45, 90, 135, 180
		t.Fatalf("threshold checks: got %d, want 4", got)
	}
}

// The cooldown used to be wiped whenever a reconcile re-tracked the pod, so a
// pod above threshold was profiled again long before cooldownSeconds.
func TestMonitor_CooldownSurvivesReconcile(t *testing.T) {
	config := createTestProfilingConfig("cfg", "default")
	config.Spec.Thresholds.CheckIntervalSeconds = 10
	config.Spec.Thresholds.CooldownSeconds = 60
	h := newMonitorHarness(t, config)
	h.metrics.setCPU(95) // permanently above the 80% threshold

	h.begin()
	h.advance(150*time.Second, 10*time.Second)

	// Captured at 10s; checks at 20..70s are within the 60s cooldown; the
	// check at 80s is the first one past it; and so on.
	assertTimes(t, "threshold capture times", h.capturer.thresholdTimes(), seconds(10, 80, 150))
	if h.reconciles < 6 {
		t.Fatalf("harness reconciled only %d times; test would not exercise the requeue", h.reconciles)
	}
}

// ---------------------------------------------------------------------------
// Capture lifecycle
// ---------------------------------------------------------------------------

// Reconciles, including ones for spec edits, used to cancel the monitor's
// context and with it any capture in progress (CPU profiles take 30s).
func TestMonitor_ReconcileDoesNotCancelInFlightCapture(t *testing.T) {
	config := createTestProfilingConfig("cfg", "default")
	config.Spec.OnDemand = &profilingv1alpha1.OnDemandConfig{Enabled: true, IntervalSeconds: 30}
	h := newMonitorHarness(t, config)
	h.metrics.setCPU(10)
	h.capturer.block = make(chan struct{})
	h.capturer.started = make(chan captureCall, 10)

	h.begin()
	h.clk.Step(30 * time.Second)
	call := <-h.capturer.started

	h.reconcile()
	cfg := &profilingv1alpha1.ProfilingConfig{}
	if err := h.r.Get(context.Background(), h.req.NamespacedName, cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Spec.Thresholds.CPUThresholdPercent = 70
	if err := h.r.Update(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	h.reconcile()

	if err := call.ctx.Err(); err != nil {
		t.Fatalf("in-flight capture was cancelled by a reconcile: %v", err)
	}
	close(h.capturer.block)
	h.waitIdle()
}

// Deleting the ProfilingConfig must still stop its monitor and abort any
// capture in progress.
func TestMonitor_DeletionCancelsInFlightCapture(t *testing.T) {
	config := createTestProfilingConfig("cfg", "default")
	config.Spec.OnDemand = &profilingv1alpha1.OnDemandConfig{Enabled: true, IntervalSeconds: 30}
	h := newMonitorHarness(t, config)
	h.capturer.block = make(chan struct{})
	h.capturer.started = make(chan captureCall, 10)

	h.begin()
	h.clk.Step(30 * time.Second)
	call := <-h.capturer.started

	if err := h.r.Delete(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	h.reconcile()

	select {
	case <-call.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("capture was not cancelled after the ProfilingConfig was deleted")
	}
	if h.monitor() != nil {
		t.Fatal("monitor still registered after deletion")
	}
	h.waitFor("schedule loops to exit", func() bool { return h.clk.pending() == 0 })
}

// Threshold and on-demand captures of the same pod must not overlap: pprof
// rejects a second concurrent CPU profile of the same process.
func TestMonitor_NoConcurrentCapturesOfSamePod(t *testing.T) {
	config := createTestProfilingConfig("cfg", "default")
	config.Spec.OnDemand = &profilingv1alpha1.OnDemandConfig{Enabled: true, IntervalSeconds: 30}
	h := newMonitorHarness(t, config) // threshold checks every 30s as well
	h.metrics.setCPU(95)
	h.capturer.block = make(chan struct{})
	h.capturer.started = make(chan captureCall, 10)

	h.begin()
	h.clk.Step(30 * time.Second) // both schedules are due
	<-h.capturer.started

	// Whichever schedule won, the other must skip the busy pod. Captures
	// run in the background, so both loops re-arm.
	h.waitFor("the other schedule to skip the busy pod", func() bool { return h.clk.pending() == 2 })

	if got := len(h.capturer.snapshot()); got != 1 {
		t.Fatalf("%d captures started for one pod at once, want 1", got)
	}
	close(h.capturer.block)
	h.waitIdle()
	if got := len(h.capturer.snapshot()); got != 1 {
		t.Fatalf("%d captures after the first finished, want 1 (the other schedule should have skipped)", got)
	}
	if h.capturer.maxInFlight != 1 {
		t.Fatalf("max concurrent captures of one pod = %d, want 1", h.capturer.maxInFlight)
	}
}

// A capture that hangs (an unresponsive pod's port-forward takes up to 10s
// to time out, a CPU profile 30s) must not delay the threshold checks of the
// config's other pods. The hung pod is skipped while its capture runs and
// checked again on the first pass after it failed.
func TestMonitor_HungCaptureDoesNotDelayOtherPodsChecks(t *testing.T) {
	config := createTestProfilingConfig("cfg", "default")
	config.Spec.Thresholds.CheckIntervalSeconds = 10
	h := newMonitorHarness(t, config) // creates the healthy pod app-0
	h.addPod("stuck")
	h.metrics.setCPU(10)             // app-0: below threshold
	h.metrics.setPodCPU("stuck", 95) // over threshold: captured
	h.capturer.block = make(chan struct{})
	h.capturer.blockPod = "stuck"
	h.capturer.failPods = map[string]bool{"stuck": true}
	h.capturer.started = make(chan captureCall, 10)

	h.begin()
	for i := 0; i < 4; i++ { // t = 10s .. 40s, while stuck's capture hangs
		h.clk.Step(10 * time.Second)
		if i == 0 {
			// The capture runs in its own goroutine; let it start (and record
			// its start time) before moving the clock on.
			<-h.capturer.started
		}
		h.waitFor("threshold pass to finish", func() bool { return h.clk.pending() == 1 })
	}
	assertTimes(t, "app-0 check times while stuck's capture hangs", h.metrics.podCheckTimes("app-0"), seconds(10, 20, 30, 40))
	assertTimes(t, "stuck capture times while it hangs", h.capturer.podTimes("stuck"), seconds(10))
	assertTimes(t, "stuck check times while its capture hangs", h.metrics.podCheckTimes("stuck"), seconds(10))

	// The capture fails; the next pass picks the pod up again.
	close(h.capturer.block)
	h.waitFor("hung capture to fail", func() bool { return h.capturesInFlight() == 0 })
	h.clk.Step(10 * time.Second) // t = 50s
	h.waitIdle()
	assertTimes(t, "app-0 check times", h.metrics.podCheckTimes("app-0"), seconds(10, 20, 30, 40, 50))
	assertTimes(t, "stuck capture times", h.capturer.podTimes("stuck"), seconds(10, 50))
}

// Unset intervals (objects that bypassed CRD defaulting) used to panic in
// time.NewTicker and crash the controller. They now fall back to the CRD
// defaults.
func TestMonitor_ZeroIntervalsUseCRDDefaults(t *testing.T) {
	config := createTestProfilingConfig("cfg", "default")
	config.Spec.Thresholds.CheckIntervalSeconds = 0
	config.Spec.Thresholds.CooldownSeconds = 0
	config.Spec.OnDemand = &profilingv1alpha1.OnDemandConfig{Enabled: true}
	h := newMonitorHarness(t, config)
	h.metrics.setCPU(95)

	h.begin()
	h.advance(360*time.Second, 5*time.Second)

	// Checks every 30s (default); after the capture at 30s the pod is in its
	// 300s cooldown (default) until the check at 360s.
	assertTimes(t, "threshold capture times", h.capturer.thresholdTimes(), seconds(30, 360))
	// On-demand every 35s (default).
	assertTimes(t, "on-demand capture times", h.capturer.onDemandTimes(), seconds(35, 70, 105, 140, 175, 210, 245, 280, 315, 350))
}

// ---------------------------------------------------------------------------
// Spec edits reach the running monitor
// ---------------------------------------------------------------------------

func TestMonitor_SpecEditChangesCheckIntervalWithoutRestart(t *testing.T) {
	config := createTestProfilingConfig("cfg", "default")
	config.Spec.Thresholds.CheckIntervalSeconds = 60
	h := newMonitorHarness(t, config)
	h.metrics.setCPU(10)

	h.begin()
	m := h.monitor()
	h.advance(40*time.Second, 10*time.Second)
	if got := h.metrics.checks(); got != 0 {
		t.Fatalf("checks before edit: got %d, want 0", got)
	}

	// Shortening the interval to 20s makes the check that was due at 20s
	// overdue, so it runs right away; later checks follow every 20s.
	h.editSpec(1, func(s *profilingv1alpha1.ProfilingConfigSpec) { s.Thresholds.CheckIntervalSeconds = 20 })
	if got := h.metrics.checks(); got != 1 {
		t.Fatalf("checks right after shortening interval: got %d, want 1", got)
	}
	h.advance(40*time.Second, 10*time.Second) // t = 80s
	if got := h.metrics.checks(); got != 3 {
		t.Fatalf("checks at 80s: got %d, want 3 (40s, 60s, 80s)", got)
	}

	// Lengthening it pushes the next check out from the last one (80s).
	h.advance(10*time.Second, 10*time.Second) // t = 90s
	h.editSpec(1, func(s *profilingv1alpha1.ProfilingConfigSpec) { s.Thresholds.CheckIntervalSeconds = 60 })
	h.advance(40*time.Second, 10*time.Second) // t = 130s
	if got := h.metrics.checks(); got != 3 {
		t.Fatalf("checks at 130s: got %d, want 3 (next is due at 140s)", got)
	}
	h.advance(10*time.Second, 10*time.Second) // t = 140s
	if got := h.metrics.checks(); got != 4 {
		t.Fatalf("checks at 140s: got %d, want 4", got)
	}

	if h.monitor() != m {
		t.Fatal("monitor was restarted; spec edits should be applied to the running monitor")
	}
}

func TestMonitor_SpecEditChangesThresholdsAndS3Config(t *testing.T) {
	config := createTestProfilingConfig("cfg", "default")
	h := newMonitorHarness(t, config)
	h.metrics.setCPU(60) // below the 80% threshold

	h.begin()
	h.advance(30*time.Second, 10*time.Second)
	if n := len(h.capturer.snapshot()); n != 0 {
		t.Fatalf("captures below threshold: got %d, want 0", n)
	}

	h.editSpec(1, func(s *profilingv1alpha1.ProfilingConfigSpec) {
		s.Thresholds.CPUThresholdPercent = 50
		s.S3Config.Bucket = "new-bucket"
	})
	h.advance(30*time.Second, 10*time.Second)

	calls := h.capturer.snapshot()
	if len(calls) != 1 {
		t.Fatalf("captures after lowering threshold: got %d, want 1", len(calls))
	}
	if calls[0].bucket != "new-bucket" {
		t.Fatalf("capture used bucket %q, want the edited %q", calls[0].bucket, "new-bucket")
	}
}

func TestMonitor_SpecEditTogglesOnDemand(t *testing.T) {
	config := createTestProfilingConfig("cfg", "default")
	h := newMonitorHarness(t, config)
	h.metrics.setCPU(10)

	h.begin()
	h.advance(50*time.Second, 10*time.Second)

	h.editSpec(2, func(s *profilingv1alpha1.ProfilingConfigSpec) {
		s.OnDemand = &profilingv1alpha1.OnDemandConfig{Enabled: true, IntervalSeconds: 30}
	})
	h.advance(70*time.Second, 10*time.Second) // t = 120s
	assertTimes(t, "on-demand capture times after enabling", h.capturer.onDemandTimes(), seconds(80, 110))

	h.editSpec(1, func(s *profilingv1alpha1.ProfilingConfigSpec) { s.OnDemand.Enabled = false })
	h.advance(90*time.Second, 10*time.Second)
	assertTimes(t, "on-demand capture times after disabling", h.capturer.onDemandTimes(), seconds(80, 110))
}

// ---------------------------------------------------------------------------
// Capture limit
// ---------------------------------------------------------------------------

func (f *fakeCapturer) setBlock(ch chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.block = ch
}

func podNamesOf(calls []captureCall) []string {
	var names []string
	for _, c := range calls {
		names = append(names, c.pod)
	}
	sort.Strings(names)
	return names
}

// With more pods than the capture limit, a run starts exactly the limit, and
// the next run starts the pods that were left out before revisiting the
// others.
func TestMonitor_CaptureLimitRotatesThroughPods(t *testing.T) {
	config := createTestProfilingConfig("cfg", "default")
	config.Spec.Thresholds.CheckIntervalSeconds = 3600
	config.Spec.OnDemand = &profilingv1alpha1.OnDemandConfig{Enabled: true, IntervalSeconds: 10}
	h := newMonitorHarness(t, config) // app-0
	for i := 1; i <= 5; i++ {
		h.addPod(fmt.Sprintf("app-%d", i))
	}
	first := make(chan struct{})
	h.capturer.setBlock(first)

	h.begin()
	h.clk.Step(10 * time.Second)
	h.waitFor("run 1 to start its captures", func() bool { return h.clk.pending() == 2 && h.capturesInFlight() == 4 })
	h.waitFor("capture calls", func() bool { return len(h.capturer.snapshot()) == 4 })
	if got := podNamesOf(h.capturer.snapshot()); fmt.Sprint(got) != "[app-0 app-1 app-2 app-3]" {
		t.Fatalf("run 1 captured %v, want the first 4 pods", got)
	}

	close(first)
	h.waitFor("run 1 captures to finish", func() bool { return h.capturesInFlight() == 0 })
	second := make(chan struct{})
	h.capturer.setBlock(second)

	h.clk.Step(10 * time.Second)
	h.waitFor("run 2 to start its captures", func() bool { return h.clk.pending() == 2 && h.capturesInFlight() == 4 })
	h.waitFor("capture calls", func() bool { return len(h.capturer.snapshot()) == 8 })
	if got := podNamesOf(h.capturer.snapshot()[4:]); fmt.Sprint(got) != "[app-0 app-1 app-4 app-5]" {
		t.Fatalf("run 2 captured %v, want app-4 and app-5 (never captured) first, then app-0 and app-1", got)
	}
	close(second)
	h.waitIdle()
	if h.capturer.maxInFlight != maxConcurrentCapturesPerConfig {
		t.Fatalf("max concurrent captures = %d, want %d", h.capturer.maxInFlight, maxConcurrentCapturesPerConfig)
	}
}

// ---------------------------------------------------------------------------
// Status counters
// ---------------------------------------------------------------------------

// failStatusWrites makes status writes fail with a conflict while fail
// returns true, as they do when another writer got there first.
func failStatusWrites(h *monitorHarness, fail func() bool) *atomic.Int32 {
	var conflicts atomic.Int32
	h.r.Client = interceptor.NewClient(h.r.Client.(client.WithWatch), interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if fail() {
				conflicts.Add(1)
				return apierrors.NewConflict(profilingv1alpha1.GroupVersion.WithResource("profilingconfigs").GroupResource(), obj.GetName(), fmt.Errorf("simulated concurrent write"))
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	return &conflicts
}

func (h *monitorHarness) status() profilingv1alpha1.ProfilingConfigStatus {
	h.t.Helper()
	cfg := &profilingv1alpha1.ProfilingConfig{}
	if err := h.r.Get(context.Background(), h.req.NamespacedName, cfg); err != nil {
		h.t.Fatalf("get config: %v", err)
	}
	return cfg.Status
}

// Counts from captures finishing together must all be written, even when
// status writes conflict and have to be retried.
func TestMonitor_StatusCountersSurviveConflicts(t *testing.T) {
	config := createTestProfilingConfig("cfg", "default")
	config.Spec.Thresholds.CheckIntervalSeconds = 3600
	config.Spec.OnDemand = &profilingv1alpha1.OnDemandConfig{Enabled: true, IntervalSeconds: 10}
	h := newMonitorHarness(t, config)
	h.addPod("app-1")
	h.addPod("app-2")
	h.capturer.result = captureResult{Profiles: 2, Uploaded: 1} // one upload of two failed

	var writes atomic.Int32
	conflicts := failStatusWrites(h, func() bool { return writes.Add(1) <= 3 })

	h.begin()
	h.advance(20*time.Second, 10*time.Second) // 2 runs x 3 pods

	h.waitFor("counters to be written", func() bool {
		st := h.status()
		return st.TotalProfiles == 12 && st.TotalUploads == 6
	})
	if conflicts.Load() != 3 {
		t.Fatalf("expected 3 conflicting writes to be retried, got %d", conflicts.Load())
	}
	st := h.status()
	if st.LastProfileTime == nil || !st.LastProfileTime.Time.Equal(h.start.Add(20*time.Second)) {
		t.Fatalf("lastProfileTime = %v, want %v", st.LastProfileTime, h.start.Add(20*time.Second))
	}
}

// Counts that could not be written yet are written when the controller shuts
// down, before shutdown returns.
func TestMonitor_ShutdownWritesPendingCounters(t *testing.T) {
	config := createTestProfilingConfig("cfg", "default")
	config.Spec.Thresholds.CheckIntervalSeconds = 3600
	config.Spec.OnDemand = &profilingv1alpha1.OnDemandConfig{Enabled: true, IntervalSeconds: 10}
	h := newMonitorHarness(t, config)

	var blocked atomic.Bool
	blocked.Store(true)
	conflicts := failStatusWrites(h, blocked.Load)

	h.begin()
	h.advance(10*time.Second, 10*time.Second)
	h.waitFor("a status write to be attempted", func() bool { return conflicts.Load() > 0 })
	if st := h.status(); st.TotalProfiles != 0 {
		t.Fatalf("counters written while writes were failing: %+v", st)
	}

	blocked.Store(false)
	h.r.shutdown()

	if st := h.status(); st.TotalProfiles != 1 || st.TotalUploads != 1 {
		t.Fatalf("after shutdown: totalProfiles=%d totalUploads=%d, want 1 and 1", st.TotalProfiles, st.TotalUploads)
	}
}
