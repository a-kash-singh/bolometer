package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	profilingv1alpha1 "github.com/a-kash-singh/bolometer/api/v1alpha1"
)

type validityHarness struct {
	t        *testing.T
	r        *ProfilingConfigReconciler
	recorder *record.FakeRecorder
	req      ctrl.Request
}

func newValidityHarness(t *testing.T, config *profilingv1alpha1.ProfilingConfig) *validityHarness {
	r := setupTestReconciler(config)
	t.Cleanup(r.baseCancel)
	recorder := record.NewFakeRecorder(100)
	r.recorder = recorder
	return &validityHarness{t: t, r: r, recorder: recorder,
		req: ctrl.Request{NamespacedName: types.NamespacedName{Namespace: config.Namespace, Name: config.Name}}}
}

func (h *validityHarness) edit(mutate func(*profilingv1alpha1.ProfilingConfigSpec)) {
	h.t.Helper()
	cfg := &profilingv1alpha1.ProfilingConfig{}
	if err := h.r.Get(context.Background(), h.req.NamespacedName, cfg); err != nil {
		h.t.Fatal(err)
	}
	mutate(&cfg.Spec)
	if err := h.r.Update(context.Background(), cfg); err != nil {
		h.t.Fatal(err)
	}
}

func (h *validityHarness) reconcile() {
	h.t.Helper()
	if _, err := h.r.Reconcile(context.Background(), h.req); err != nil {
		h.t.Fatalf("Reconcile: %v", err)
	}
}

// events drains the events recorded so far ("Type Reason Message").
func (h *validityHarness) events() []string {
	var out []string
	for {
		select {
		case e := <-h.recorder.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func (h *validityHarness) ready() *metav1.Condition {
	cfg := &profilingv1alpha1.ProfilingConfig{}
	if err := h.r.Get(context.Background(), h.req.NamespacedName, cfg); err != nil {
		h.t.Fatal(err)
	}
	return meta.FindStatusCondition(cfg.Status.Conditions, ConditionReady)
}

func expectEvents(t *testing.T, step string, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: events %q, want %d event(s) starting with %q", step, got, len(want), want)
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			t.Fatalf("%s: event %q, want it to start with %q", step, got[i], want[i])
		}
	}
}

// Events are emitted on transitions: once when the config becomes invalid,
// again if it becomes invalid in a different way, and once when fixed. The
// Ready condition follows every step.
func TestValidity_EventsOnTransitionsOnly(t *testing.T) {
	h := newValidityHarness(t, createTestProfilingConfig("cfg", "default"))

	h.reconcile()
	expectEvents(t, "valid from the start", h.events())
	if c := h.ready(); c == nil || c.Status != metav1.ConditionTrue || c.Reason != ReasonMonitoring {
		t.Fatalf("Ready after creation = %+v, want True/%s", c, ReasonMonitoring)
	}

	h.edit(func(s *profilingv1alpha1.ProfilingConfigSpec) { s.S3Config.Bucket = "" })
	h.reconcile()
	h.reconcile()
	expectEvents(t, "bucket removed, reconciled twice", h.events(),
		"Warning InvalidConfig Invalid configuration, monitoring stopped: s3 bucket is required")

	h.edit(func(s *profilingv1alpha1.ProfilingConfigSpec) { s.S3Config.Bucket = "b"; s.S3Config.Region = "" })
	h.reconcile()
	expectEvents(t, "bucket fixed, region removed", h.events(),
		"Warning InvalidConfig Invalid configuration, monitoring stopped: s3 region is required")
	if c := h.ready(); c == nil || c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, "s3 region is required") {
		t.Fatalf("Ready = %+v, want False naming the missing region", c)
	}

	h.edit(func(s *profilingv1alpha1.ProfilingConfigSpec) { s.S3Config.Region = "us-east-1" })
	h.reconcile()
	h.reconcile()
	expectEvents(t, "fixed, reconciled twice", h.events(), "Normal ConfigValid")
	if c := h.ready(); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("Ready after the fix = %+v, want True", c)
	}
	if _, running := h.r.activeMonitors[h.req.NamespacedName.String()]; !running {
		t.Fatal("monitoring did not resume after the fix")
	}
}

// After a controller restart the in-memory state is gone; the Ready
// condition tells the new reconciler that the config was already reported
// invalid, so the warning is not repeated, and the fix is still announced.
func TestValidity_RestartDoesNotRepeatWarning(t *testing.T) {
	config := createTestProfilingConfig("cfg", "default")
	config.Spec.S3Config.Bucket = ""
	h := newValidityHarness(t, config)
	h.reconcile()
	expectEvents(t, "first controller", h.events(), "Warning InvalidConfig")

	// A new reconciler on the same API state, as after a restart.
	restarted := &validityHarness{t: t, r: &ProfilingConfigReconciler{
		Client: h.r.Client, Scheme: h.r.Scheme, Clientset: h.r.Clientset,
		podWatcher: NewPodWatcher(h.r.Clientset), metricsCollector: &fakeMetrics{},
		captureFn: h.r.captureFn, baseCtx: h.r.baseCtx, baseCancel: h.r.baseCancel,
		activeMonitors: map[string]*configMonitor{},
	}, recorder: record.NewFakeRecorder(100), req: h.req}
	restarted.r.recorder = restarted.recorder

	restarted.reconcile()
	expectEvents(t, "restarted, still invalid", restarted.events())

	restarted.edit(func(s *profilingv1alpha1.ProfilingConfigSpec) { s.S3Config.Bucket = "b" })
	restarted.reconcile()
	expectEvents(t, "restarted, fixed", restarted.events(), "Normal ConfigValid")
}

// If the invalid state cannot be recorded in the status, Reconcile returns
// the error so that controller-runtime retries, and no event is emitted yet.
func TestValidity_StatusWriteFailureIsRetried(t *testing.T) {
	config := createTestProfilingConfig("cfg", "default")
	config.Spec.S3Config.Bucket = ""
	h := newValidityHarness(t, config)
	failing := true
	h.r.Client = interceptor.NewClient(h.r.Client.(client.WithWatch), interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if failing {
				return apierrors.NewServiceUnavailable("simulated API outage")
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})

	if _, err := h.r.Reconcile(context.Background(), h.req); err == nil {
		t.Fatal("Reconcile succeeded although the status could not be written; want an error so it is retried")
	}
	expectEvents(t, "status write failed", h.events())

	failing = false
	h.reconcile()
	expectEvents(t, "retried", h.events(), "Warning InvalidConfig")
}

// An edit that makes the config invalid cancels captures in flight: they
// would upload with the settings the user replaced.
func TestMonitor_InvalidEditCancelsInFlightCapture(t *testing.T) {
	config := createTestProfilingConfig("cfg", "default")
	config.Spec.OnDemand = &profilingv1alpha1.OnDemandConfig{Enabled: true, IntervalSeconds: 30}
	h := newMonitorHarness(t, config)
	h.capturer.block = make(chan struct{})
	h.capturer.started = make(chan captureCall, 10)

	h.begin()
	h.clk.Step(30 * time.Second)
	call := <-h.capturer.started

	cfg := &profilingv1alpha1.ProfilingConfig{}
	if err := h.r.Get(context.Background(), h.req.NamespacedName, cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Spec.S3Config.Bucket = ""
	if err := h.r.Update(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	h.reconcile()

	select {
	case <-call.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("capture in flight was not cancelled by the invalid edit")
	}
	if h.monitor() != nil {
		t.Fatal("monitor still registered for the invalid config")
	}
	if n := len(h.r.podWatcher.GetTrackedPodsForConfig(h.req.NamespacedName.String())); n != 0 {
		t.Fatalf("%d pods still tracked for the invalid config", n)
	}
	h.waitFor("schedule loops to exit", func() bool { return h.clk.pending() == 0 })
}
