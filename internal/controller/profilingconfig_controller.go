package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	profilingv1alpha1 "github.com/a-kash-singh/bolometer/api/v1alpha1"
	"github.com/a-kash-singh/bolometer/internal/metrics"
	"github.com/a-kash-singh/bolometer/internal/profiler"
	"github.com/a-kash-singh/bolometer/internal/uploader"
)

const (
	// requeueInterval is how often a ProfilingConfig is re-reconciled to pick
	// up pod changes. It has no effect on the monitoring schedules.
	requeueInterval = 30 * time.Second

	// captureTimeout bounds a single capture-and-upload. Monitors are
	// long-lived, so a capture that never returns would stall its schedule
	// for good. A capture is normally one 30s CPU profile plus a few
	// near-instant profiles and an upload.
	captureTimeout = 5 * time.Minute

	// maxConcurrentCapturesPerConfig caps the captures (threshold and
	// on-demand together) a ProfilingConfig runs at once, so that many pods
	// crossing a threshold together do not open a port-forward each through
	// the API server. Pods over the cap are skipped and retried on the next
	// pass.
	maxConcurrentCapturesPerConfig = 4

	// Retry backoff for writing status counters.
	statsRetryInitial = 50 * time.Millisecond
	statsRetryMax     = 5 * time.Second
	// finalFlushTimeout bounds how long a stopping monitor waits for its
	// captures and for writing their counts.
	finalFlushTimeout = 10 * time.Second

	// Fallbacks matching the CRD defaults, used when a field reaches the
	// controller unset (e.g. the object bypassed CRD defaulting). A zero
	// interval would otherwise spin or panic.
	defaultCheckIntervalSeconds    = 30
	defaultCooldownSeconds         = 300
	defaultOnDemandIntervalSeconds = 35
)

// Status condition and events reporting whether a ProfilingConfig is valid
// and being monitored.
const (
	// ConditionReady is True while the config is valid and monitored, and
	// False (reason ReasonInvalidConfig) while it is invalid.
	ConditionReady = "Ready"

	ReasonMonitoring    = "Monitoring"
	ReasonInvalidConfig = "InvalidConfig"
	// ReasonConfigValid is the reason of the event emitted when an invalid
	// config has been fixed.
	ReasonConfigValid = "ConfigValid"
)

// podMetricsGetter is implemented by *metrics.Collector.
type podMetricsGetter interface {
	GetPodMetrics(ctx context.Context, namespace, podName string, pod *corev1.Pod) (*metrics.PodMetrics, error)
}

// ProfilingConfigReconciler reconciles a ProfilingConfig object
type ProfilingConfigReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	Clientset     kubernetes.Interface
	MetricsClient metricsv.Interface
	RestConfig    *rest.Config

	podWatcher       *PodWatcher
	metricsCollector podMetricsGetter
	profiler         *profiler.Profiler

	// newUploader builds an S3 uploader (client). nil means
	// uploader.NewS3Uploader; tests wrap it to count clients built.
	newUploader func(ctx context.Context, cfg uploader.S3Config) (*uploader.S3Uploader, error)

	// captureFn captures and uploads profiles for a pod. nil means
	// captureAndUpload; tests substitute a fake.
	captureFn func(ctx context.Context, pod *corev1.Pod, config *profilingv1alpha1.ProfilingConfig, reason string) (captureResult, error)

	// clock drives the monitoring schedules. nil means the real clock.
	clock clock

	// baseCtx is a long-lived context for monitoring goroutines. It must NOT be
	// the per-request reconcile context, which is cancelled when Reconcile()
	// returns. Individual monitors are cancelled via their configMonitor.
	baseCtx    context.Context
	baseCancel context.CancelFunc

	// activeMonitors holds one long-lived monitor per ProfilingConfig, keyed
	// by namespace/name. Monitors are created on first reconcile, updated in
	// place on later reconciles, and stopped when the config is deleted.
	monitorsMu     sync.Mutex
	activeMonitors map[string]*configMonitor

	// statsWriters tracks the monitors' status writers, so that shutdown can
	// wait for them to write the last counts.
	statsWriters sync.WaitGroup

	// recorder emits Kubernetes events about ProfilingConfigs. nil disables
	// events.
	recorder record.EventRecorder

	// invalid holds, per config (namespace/name), the Ready message last
	// recorded for it while invalid; "" once recorded valid. It lets events
	// and logs be emitted on transitions only.
	invalidMu sync.Mutex
	invalid   map[string]string
}

// captureResult reports what a capture achieved, also when it failed part
// way: Profiles is the number of profiles captured, Uploaded the number of
// them stored in S3.
type captureResult struct {
	Profiles int
	Uploaded int
}

// configMonitor runs the threshold and on-demand schedules for one
// ProfilingConfig. It survives reconciles: Reconcile hands it the latest
// config through update() rather than restarting it, so schedules, cooldowns
// and in-flight captures do not depend on how often the controller
// reconciles.
type configMonitor struct {
	key    types.NamespacedName
	cancel context.CancelFunc

	mu     sync.Mutex
	config *profilingv1alpha1.ProfilingConfig
	// changed is closed, and replaced, whenever the spec changes, waking the
	// schedule loops so they re-evaluate their intervals.
	changed chan struct{}

	// slots holds a token per running capture (maxConcurrentCapturesPerConfig).
	slots chan struct{}
	// captures tracks the running capture goroutines.
	captures sync.WaitGroup

	// s3 is the config's S3 client, shared by its captures. It is built on
	// first use, rebuilt when the region or endpoint changes (bucket and
	// prefix are applied per upload), and dropped with the monitor when the
	// config is deleted or becomes invalid.
	s3Mu       sync.Mutex
	s3         *uploader.S3Uploader
	s3Region   string
	s3Endpoint string

	// Status counter increments not yet written. They are only subtracted
	// once a write including them has succeeded, so none are lost.
	statsMu   sync.Mutex
	pending   statsDelta
	statsKick chan struct{}
}

// statsDelta is a set of status counter increments.
type statsDelta struct {
	profiles    int64
	uploads     int64
	lastProfile time.Time
}

func (d statsDelta) isZero() bool {
	return d.profiles == 0 && d.uploads == 0 && d.lastProfile.IsZero()
}

// tryAcquireSlot takes a capture slot if one is free.
func (m *configMonitor) tryAcquireSlot() bool {
	select {
	case m.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (m *configMonitor) releaseSlot() { <-m.slots }

// addStats records the outcome of a capture and wakes the status writer.
func (m *configMonitor) addStats(res captureResult, at time.Time) {
	if res.Profiles == 0 && res.Uploaded == 0 {
		return
	}
	m.statsMu.Lock()
	m.pending.profiles += int64(res.Profiles)
	m.pending.uploads += int64(res.Uploaded)
	if res.Profiles > 0 && at.After(m.pending.lastProfile) {
		m.pending.lastProfile = at
	}
	m.statsMu.Unlock()

	select {
	case m.statsKick <- struct{}{}:
	default: // a write is already due
	}
}

func (m *configMonitor) pendingStats() statsDelta {
	m.statsMu.Lock()
	defer m.statsMu.Unlock()
	return m.pending
}

// statsWritten removes increments that have been written.
func (m *configMonitor) statsWritten(d statsDelta) {
	m.statsMu.Lock()
	defer m.statsMu.Unlock()
	m.pending.profiles -= d.profiles
	m.pending.uploads -= d.uploads
	if m.pending.lastProfile.Equal(d.lastProfile) {
		m.pending.lastProfile = time.Time{}
	}
}

func (m *configMonitor) dropStats() {
	m.statsMu.Lock()
	defer m.statsMu.Unlock()
	m.pending = statsDelta{}
}

// current returns the latest config and a channel closed on the next spec change.
func (m *configMonitor) current() (*profilingv1alpha1.ProfilingConfig, <-chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.config, m.changed
}

// update stores the latest config, waking the schedule loops if the spec changed.
func (m *configMonitor) update(config *profilingv1alpha1.ProfilingConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	specChanged := !equality.Semantic.DeepEqual(m.config.Spec, config.Spec)
	m.config = config
	if specChanged {
		close(m.changed)
		m.changed = make(chan struct{})
	}
}

// NewProfilingConfigReconciler creates a new reconciler
func NewProfilingConfigReconciler(
	client client.Client,
	scheme *runtime.Scheme,
	clientset kubernetes.Interface,
	metricsClient metricsv.Interface,
	restConfig *rest.Config,
) *ProfilingConfigReconciler {
	baseCtx, baseCancel := context.WithCancel(context.Background())

	// Events are written through the clientset; the broadcaster stops with
	// the reconciler.
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: clientset.CoreV1().Events("")})
	go func() {
		<-baseCtx.Done()
		broadcaster.Shutdown()
	}()

	return &ProfilingConfigReconciler{
		Client:           client,
		Scheme:           scheme,
		Clientset:        clientset,
		MetricsClient:    metricsClient,
		RestConfig:       restConfig,
		podWatcher:       NewPodWatcher(clientset),
		metricsCollector: metrics.NewCollector(metricsClient),
		profiler:         profiler.NewProfiler(clientset, restConfig),
		baseCtx:          baseCtx,
		baseCancel:       baseCancel,
		activeMonitors:   make(map[string]*configMonitor),
		recorder:         broadcaster.NewRecorder(scheme, corev1.EventSource{Component: "bolometer"}),
	}
}

// +kubebuilder:rbac:groups=bolometer.io,resources=profilingconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=bolometer.io,resources=profilingconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=bolometer.io,resources=profilingconfigs/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/portforward,verbs=create;get
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=metrics.k8s.io,resources=pods,verbs=get;list

// Reconcile handles ProfilingConfig changes
func (r *ProfilingConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the ProfilingConfig
	config := &profilingv1alpha1.ProfilingConfig{}
	if err := r.Get(ctx, req.NamespacedName, config); err != nil {
		if errors.IsNotFound(err) {
			// Object deleted: stop monitoring and drop the pods it tracked.
			r.stopMonitoring(req.NamespacedName.String())
			r.podWatcher.UntrackConfig(req.NamespacedName.String())
			r.forgetValidity(req.NamespacedName.String())
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Validate configuration. An invalid config stops monitoring until it
	// is fixed; the problem is reported in the status and as an event.
	if err := r.validateConfig(config); err != nil {
		return r.reconcileInvalid(ctx, config, err)
	}
	wasInvalid, _ := r.lastValidity(config)

	// List matching pods
	pods, err := r.podWatcher.ListMatchingPods(ctx, config)
	if err != nil {
		logger.Error(err, "Failed to list pods")
		return ctrl.Result{}, err
	}

	logger.Info("Found matching pods", "count", len(pods))

	// Track exactly the matching pods for this config. Pods that were
	// deleted or no longer match are dropped.
	r.podWatcher.SyncPods(config, pods)

	// Update status.activePods and the Ready condition.
	ready := metav1.Condition{
		Type:    ConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  ReasonMonitoring,
		Message: "Configuration is valid; monitoring matching pods",
	}
	if err := r.patchStatus(ctx, config, len(pods), ready); err != nil {
		logger.Error(err, "Failed to update status")
	} else {
		if wasInvalid {
			logger.Info("Configuration fixed; monitoring resumed")
			r.event(config, corev1.EventTypeNormal, ReasonConfigValid, "Configuration is valid again; monitoring resumed")
		}
		r.setValidity(config, "")
	}

	// Start monitoring, or hand the latest config to the running monitor.
	// The monitor is deliberately not restarted here: reconciles happen every
	// requeueInterval and on every status update, and restarting would reset
	// the schedules and cancel in-flight captures.
	r.ensureMonitoring(config)

	return ctrl.Result{RequeueAfter: requeueInterval}, nil
}

// reconcileInvalid handles a config that failed validation: it stops the
// config's monitor (so nothing keeps running with the settings the user
// replaced, and in-flight captures are cancelled), drops its pods, and
// records the problem in the Ready condition. A Warning event is emitted and
// the problem logged when the config becomes invalid, or invalid in a
// different way; not on every reconcile. Once the status is recorded,
// Reconcile succeeds: retrying cannot fix the spec, and the edit that does
// will trigger a new reconcile.
func (r *ProfilingConfigReconciler) reconcileInvalid(ctx context.Context, config *profilingv1alpha1.ProfilingConfig, invalid error) (ctrl.Result, error) {
	configKey := configKeyOf(config)
	r.stopMonitoring(configKey)
	r.podWatcher.UntrackConfig(configKey)

	wasInvalid, previous := r.lastValidity(config)
	message := "Invalid configuration, monitoring stopped: " + invalid.Error()
	notReady := metav1.Condition{
		Type:    ConditionReady,
		Status:  metav1.ConditionFalse,
		Reason:  ReasonInvalidConfig,
		Message: message,
	}
	if err := r.patchStatus(ctx, config, 0, notReady); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to record invalid configuration in status: %w", err)
	}

	if !wasInvalid || previous != message {
		log.FromContext(ctx).Error(invalid, "Invalid configuration; monitoring stopped until it is fixed")
		r.event(config, corev1.EventTypeWarning, ReasonInvalidConfig, message)
	}
	r.setValidity(config, message)
	return ctrl.Result{}, nil
}

// patchStatus sets status.activePods and the given condition. It patches
// those fields alone: the config may have been read from a stale cache, and
// writing back the whole status would either conflict with or overwrite the
// counters the monitors maintain. A patch that changes nothing is a no-op.
func (r *ProfilingConfigReconciler) patchStatus(ctx context.Context, config *profilingv1alpha1.ProfilingConfig, activePods int, condition metav1.Condition) error {
	conditions := append([]metav1.Condition(nil), config.Status.Conditions...)
	condition.ObservedGeneration = config.Generation
	meta.SetStatusCondition(&conditions, condition)

	patch, err := json.Marshal(map[string]any{
		"status": map[string]any{
			"activePods": activePods,
			"conditions": conditions,
		},
	})
	if err != nil {
		return err
	}
	return r.Status().Patch(ctx, config, client.RawPatch(types.MergePatchType, patch))
}

// lastValidity reports whether the config was last recorded as invalid, and
// with which Ready message. After a restart it falls back to the config's
// Ready condition.
func (r *ProfilingConfigReconciler) lastValidity(config *profilingv1alpha1.ProfilingConfig) (invalid bool, message string) {
	r.invalidMu.Lock()
	message, known := r.invalid[configKeyOf(config)]
	r.invalidMu.Unlock()
	if known {
		return message != "", message
	}
	if c := meta.FindStatusCondition(config.Status.Conditions, ConditionReady); c != nil &&
		c.Status == metav1.ConditionFalse && c.Reason == ReasonInvalidConfig {
		return true, c.Message
	}
	return false, ""
}

// setValidity records the config's Ready message if invalid, "" if valid.
func (r *ProfilingConfigReconciler) setValidity(config *profilingv1alpha1.ProfilingConfig, invalidMessage string) {
	r.invalidMu.Lock()
	defer r.invalidMu.Unlock()
	if r.invalid == nil {
		r.invalid = make(map[string]string)
	}
	r.invalid[configKeyOf(config)] = invalidMessage
}

func (r *ProfilingConfigReconciler) forgetValidity(configKey string) {
	r.invalidMu.Lock()
	defer r.invalidMu.Unlock()
	delete(r.invalid, configKey)
}

// event emits a Kubernetes event about the config, if events are enabled.
func (r *ProfilingConfigReconciler) event(config *profilingv1alpha1.ProfilingConfig, eventType, reason, message string) {
	if r.recorder != nil {
		r.recorder.Event(config, eventType, reason, message)
	}
}

// ensureMonitoring starts monitoring for a ProfilingConfig if it is not
// running yet, and otherwise passes the latest config to the running monitor.
func (r *ProfilingConfigReconciler) ensureMonitoring(config *profilingv1alpha1.ProfilingConfig) {
	configKey := configKeyOf(config)

	r.monitorsMu.Lock()
	defer r.monitorsMu.Unlock()

	if m, ok := r.activeMonitors[configKey]; ok {
		m.update(config.DeepCopy())
		return
	}
	// NOTE: r.baseCtx (not the request ctx) so the goroutines survive
	// Reconcile() returning.
	r.startMonitoringLocked(r.baseCtx, config)
}

// startMonitoring starts monitoring for a ProfilingConfig, replacing any
// monitor already running for it.
func (r *ProfilingConfigReconciler) startMonitoring(parentCtx context.Context, config *profilingv1alpha1.ProfilingConfig) {
	r.monitorsMu.Lock()
	defer r.monitorsMu.Unlock()
	r.startMonitoringLocked(parentCtx, config)
}

func (r *ProfilingConfigReconciler) startMonitoringLocked(parentCtx context.Context, config *profilingv1alpha1.ProfilingConfig) {
	configKey := configKeyOf(config)
	if existing, ok := r.activeMonitors[configKey]; ok {
		existing.cancel()
	}
	if r.activeMonitors == nil {
		r.activeMonitors = make(map[string]*configMonitor)
	}

	ctx, cancel := context.WithCancel(parentCtx)
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("profilingconfig", configKey))
	m := &configMonitor{
		key:       client.ObjectKeyFromObject(config),
		cancel:    cancel,
		config:    config.DeepCopy(),
		changed:   make(chan struct{}),
		slots:     make(chan struct{}, maxConcurrentCapturesPerConfig),
		statsKick: make(chan struct{}, 1),
	}
	r.activeMonitors[configKey] = m

	r.statsWriters.Add(1)
	go r.writeStatsLoop(ctx, m)

	// Both schedules are measured from now. The on-demand loop always runs
	// and idles while on-demand is disabled, so enabling it later via a spec
	// edit takes effect without a restart.
	start := r.clk().Now()
	go r.monitorThresholds(ctx, m, start)
	go r.monitorOnDemand(ctx, m, start)
}

// stopMonitoring stops monitoring for a ProfilingConfig
func (r *ProfilingConfigReconciler) stopMonitoring(configKey string) {
	r.monitorsMu.Lock()
	defer r.monitorsMu.Unlock()

	if m, ok := r.activeMonitors[configKey]; ok {
		m.cancel()
		delete(r.activeMonitors, configKey)
	}
}

// runSchedule calls fn with the monitor's current config every interval, as
// returned by schedule for that config. Each run is timed from the previous
// run (the first from start), never from the last reconcile, so frequent
// reconciles cannot postpone it.
//
// A spec change wakes the loop to re-evaluate the deadline against the new
// interval: if a shortened interval is already overdue it runs immediately,
// and a lengthened one pushes the next run out. When schedule reports the
// schedule disabled, the loop idles until the next spec change; re-enabling
// starts a fresh interval.
func (r *ProfilingConfigReconciler) runSchedule(
	ctx context.Context,
	m *configMonitor,
	start time.Time,
	schedule func(*profilingv1alpha1.ProfilingConfigSpec) (time.Duration, bool),
	fn func(context.Context, *profilingv1alpha1.ProfilingConfig),
) {
	clk := r.clk()
	last := start
	active := true

	for {
		config, changed := m.current()
		interval, enabled := schedule(&config.Spec)

		var t timer
		var fire <-chan time.Time
		if enabled {
			now := clk.Now()
			if !active {
				last = now
				active = true
			}
			wait := last.Add(interval).Sub(now)
			if wait < 0 {
				wait = 0
			}
			t = clk.NewTimer(wait)
			fire = t.C()
		} else {
			active = false
		}

		select {
		case <-ctx.Done():
			if t != nil {
				t.Stop()
			}
			return
		case <-changed:
			if t != nil {
				t.Stop()
			}
		case <-fire:
			last = clk.Now()
			// Use the newest config, in case an edit raced with the timer.
			config, _ = m.current()
			if _, stillEnabled := schedule(&config.Spec); stillEnabled {
				fn(ctx, config)
			}
		}
	}
}

// monitorThresholds monitors pods for threshold violations every
// checkIntervalSeconds.
func (r *ProfilingConfigReconciler) monitorThresholds(ctx context.Context, m *configMonitor, start time.Time) {
	logger := log.FromContext(ctx)
	r.runSchedule(ctx, m, start, thresholdSchedule, func(ctx context.Context, config *profilingv1alpha1.ProfilingConfig) {
		r.checkPodsThresholds(ctx, m, config, logger)
	})
}

func thresholdSchedule(spec *profilingv1alpha1.ProfilingConfigSpec) (time.Duration, bool) {
	return secondsOrDefault(spec.Thresholds.CheckIntervalSeconds, defaultCheckIntervalSeconds), true
}

func onDemandSchedule(spec *profilingv1alpha1.ProfilingConfigSpec) (time.Duration, bool) {
	if spec.OnDemand == nil || !spec.OnDemand.Enabled {
		return 0, false
	}
	return secondsOrDefault(spec.OnDemand.IntervalSeconds, defaultOnDemandIntervalSeconds), true
}

func cooldownSeconds(spec *profilingv1alpha1.ProfilingConfigSpec) int {
	if spec.Thresholds.CooldownSeconds <= 0 {
		return defaultCooldownSeconds
	}
	return spec.Thresholds.CooldownSeconds
}

func secondsOrDefault(seconds, fallback int) time.Duration {
	if seconds <= 0 {
		seconds = fallback
	}
	return time.Duration(seconds) * time.Second
}

// checkPodsThresholds checks the config's tracked pods for threshold
// violations.
//
// Captures run in the background, so a slow or unresponsive pod cannot hold
// up the checks of the others: a capture includes a 30s CPU profile, and a
// port-forward to an unreachable pod takes up to 10s to time out. A pod whose
// capture is still running is skipped, and checked again on a later pass, as
// is a pod over the threshold while the config is at its capture limit.
func (r *ProfilingConfigReconciler) checkPodsThresholds(ctx context.Context, m *configMonitor, config *profilingv1alpha1.ProfilingConfig, logger logr.Logger) {
	trackedPods := r.podWatcher.GetTrackedPodsForConfig(configKeyOf(config))

	for _, tracked := range trackedPods {
		if ctx.Err() != nil {
			return
		}

		// Skip if in cooldown period
		if !r.podWatcher.CanProfile(tracked.Pod, cooldownSeconds(&config.Spec)) {
			continue
		}

		// Skip while a capture of the pod is running; it is retried on a
		// later pass if that capture fails.
		if r.podWatcher.IsCapturing(tracked.Pod) {
			continue
		}

		// Get pod metrics
		podMetrics, err := r.metricsCollector.GetPodMetrics(ctx, tracked.Pod.Namespace, tracked.Pod.Name, tracked.Pod)
		if err != nil {
			logger.Error(err, "Failed to get pod metrics", "pod", tracked.Pod.Name)
			continue
		}

		// Check thresholds
		exceeded, reason := podMetrics.CheckThresholds(
			config.Spec.Thresholds.CPUThresholdPercent,
			config.Spec.Thresholds.MemoryThresholdPercent,
		)

		if exceeded {
			switch r.startCapture(ctx, m, tracked.Pod, config, reason, true, logger) {
			case captureStarted:
				logger.Info("Threshold exceeded, capturing profile",
					"pod", tracked.Pod.Name,
					"reason", reason,
				)
			case capturePodBusy:
				logger.Info("Skipping threshold capture, another capture of this pod is in progress", "pod", tracked.Pod.Name)
			case captureLimitReached:
				logger.Info("Threshold exceeded, but the config is at its capture limit; will retry on the next check",
					"pod", tracked.Pod.Name, "limit", maxConcurrentCapturesPerConfig)
			}
		}
	}
}

type captureStart int

const (
	captureStarted captureStart = iota
	capturePodBusy
	captureLimitReached
)

// startCapture starts a capture of the pod in the background, unless the
// config is at its capture limit or the pod is already being captured. The
// slot and the pod are claimed before it returns, so the next pass or
// schedule sees them taken. A successful threshold capture starts the pod's
// cooldown; the capture's counts are added to the config's status either way.
func (r *ProfilingConfigReconciler) startCapture(ctx context.Context, m *configMonitor, pod *corev1.Pod, config *profilingv1alpha1.ProfilingConfig, reason string, threshold bool, logger logr.Logger) captureStart {
	if r.podWatcher.IsCapturing(pod) {
		return capturePodBusy
	}
	if !m.tryAcquireSlot() {
		return captureLimitReached
	}
	if !r.podWatcher.TryStartCapture(pod) {
		m.releaseSlot()
		return capturePodBusy
	}

	m.captures.Add(1)
	go func() {
		defer m.captures.Done()
		defer m.releaseSlot()
		defer r.podWatcher.FinishCapture(pod)

		res, err := r.capture(ctx, m, pod, config, reason)
		m.addStats(res, r.clk().Now())
		if err != nil {
			logger.Error(err, "Failed to capture and upload profiles", "pod", pod.Name, "reason", reason,
				"profiles", res.Profiles, "uploaded", res.Uploaded)
			return
		}
		if threshold {
			// Start the cooldown before releasing the pod, so there is no
			// moment when it is neither busy nor cooling down.
			r.podWatcher.UpdateLastProfileTime(pod)
		}
	}()
	return captureStarted
}

// monitorOnDemand performs on-demand continuous profiling every
// onDemand.intervalSeconds while on-demand mode is enabled. Like threshold
// captures, on-demand captures run in the background under the config's
// capture limit; pods that are busy or over the limit are captured on the
// next run. Pods are visited least recently attempted first, so with more
// pods than the limit every pod gets its turn.
func (r *ProfilingConfigReconciler) monitorOnDemand(ctx context.Context, m *configMonitor, start time.Time) {
	logger := log.FromContext(ctx)
	r.runSchedule(ctx, m, start, onDemandSchedule, func(ctx context.Context, config *profilingv1alpha1.ProfilingConfig) {
		deferred := 0
		for _, tracked := range r.podWatcher.GetTrackedPodsForConfig(configKeyOf(config)) {
			if ctx.Err() != nil {
				return
			}
			switch r.startCapture(ctx, m, tracked.Pod, config, "on-demand", false, logger) {
			case captureStarted:
				logger.Info("On-demand profiling", "pod", tracked.Pod.Name)
			case capturePodBusy:
				logger.Info("Skipping on-demand capture, another capture of this pod is in progress", "pod", tracked.Pod.Name)
			case captureLimitReached:
				deferred++
			}
		}
		if deferred > 0 {
			logger.Info("Config is at its capture limit; remaining pods will be captured on the next run",
				"deferred", deferred, "limit", maxConcurrentCapturesPerConfig)
		}
	})
}

// capture captures and uploads profiles for a pod, bounded by
// captureTimeout. Callers must hold the pod's capture claim
// (PodWatcher.TryStartCapture), so that captures of a pod never overlap:
// pprof refuses a second concurrent CPU profile.
func (r *ProfilingConfigReconciler) capture(ctx context.Context, m *configMonitor, pod *corev1.Pod, config *profilingv1alpha1.ProfilingConfig, reason string) (captureResult, error) {
	ctx, cancel := context.WithTimeout(ctx, captureTimeout)
	defer cancel()

	if r.captureFn != nil {
		return r.captureFn(ctx, pod, config, reason)
	}
	return r.captureAndUpload(ctx, m, pod, config, reason)
}

func (r *ProfilingConfigReconciler) clk() clock {
	if r.clock == nil {
		return realClock{}
	}
	return r.clock
}

// captureAndUpload captures profiles and uploads them to S3. The result
// counts what was captured and uploaded, also when an error is returned.
//
// If some profile types fail, the others are still captured and uploaded;
// the failures are logged, and the capture succeeds as long as at least one
// profile was captured and every captured profile was uploaded. It fails if
// no profile could be captured or an upload failed.
func (r *ProfilingConfigReconciler) captureAndUpload(ctx context.Context, m *configMonitor, pod *corev1.Pod, config *profilingv1alpha1.ProfilingConfig, reason string) (captureResult, error) {
	// Determine which profile types to capture
	profileTypes := config.Spec.ProfileTypes
	if len(profileTypes) == 0 {
		profileTypes = []string{"heap", "cpu", "goroutine", "mutex"}
	}

	// Capture profiles. A pod in trouble may fail some profile types; keep
	// whatever could be captured rather than discarding it.
	profiles, err := r.profiler.CaptureProfiles(ctx, pod, profileTypes)
	if len(profiles) == 0 {
		return captureResult{}, fmt.Errorf("failed to capture profiles: %w", err)
	}
	if err != nil {
		log.FromContext(ctx).Error(err, "Some profile types could not be captured; uploading the others",
			"pod", pod.Name, "captured", len(profiles), "requested", len(profileTypes))
	}
	res := captureResult{Profiles: len(profiles)}

	// The config's S3 client, for the bucket and prefix of this capture.
	s3Uploader, err := r.uploaderFor(ctx, m, config.Spec.S3Config)
	if err != nil {
		return res, fmt.Errorf("failed to create S3 uploader: %w", err)
	}

	// Upload profiles
	res.Uploaded, err = s3Uploader.UploadProfiles(ctx, pod, profiles, reason)
	if err != nil {
		return res, fmt.Errorf("failed to upload %d of %d profiles: %w", len(profiles)-res.Uploaded, len(profiles), err)
	}

	return res, nil
}

// uploaderFor returns an uploader for the S3 settings through the config's
// shared S3 client, building the client on first use and rebuilding it when
// the region or endpoint changed. A failed build is not cached; the next
// capture tries again. Captures already using a replaced client finish with
// it.
func (r *ProfilingConfigReconciler) uploaderFor(ctx context.Context, m *configMonitor, s3cfg profilingv1alpha1.S3Configuration) (*uploader.S3Uploader, error) {
	m.s3Mu.Lock()
	defer m.s3Mu.Unlock()

	if m.s3 == nil || m.s3Region != s3cfg.Region || m.s3Endpoint != s3cfg.Endpoint {
		build := r.newUploader
		if build == nil {
			build = uploader.NewS3Uploader
		}
		u, err := build(ctx, uploader.S3Config{
			Bucket:   s3cfg.Bucket,
			Prefix:   s3cfg.Prefix,
			Region:   s3cfg.Region,
			Endpoint: s3cfg.Endpoint,
		})
		if err != nil {
			return nil, err
		}
		log.FromContext(ctx).Info("Created S3 client", "region", s3cfg.Region, "endpoint", s3cfg.Endpoint)
		m.s3, m.s3Region, m.s3Endpoint = u, s3cfg.Region, s3cfg.Endpoint
	}
	return m.s3.ForDestination(s3cfg.Bucket, s3cfg.Prefix), nil
}

// writeStatsLoop writes the monitor's pending status counters whenever
// captures add to them. When the monitor stops, it waits (bounded) for the
// running captures to finish and writes their counts too.
func (r *ProfilingConfigReconciler) writeStatsLoop(ctx context.Context, m *configMonitor) {
	defer r.statsWriters.Done()
	logger := log.FromContext(ctx)

	for {
		select {
		case <-m.statsKick:
			r.flushStats(ctx, m, logger)
		case <-ctx.Done():
			final, cancel := context.WithTimeout(context.Background(), finalFlushTimeout)
			defer cancel()
			captured := make(chan struct{})
			go func() {
				m.captures.Wait()
				close(captured)
			}()
			select {
			case <-captured:
			case <-final.Done():
			}
			r.flushStats(final, m, logger)
			return
		}
	}
}

// flushStats writes the pending counter increments to the config's status,
// retrying with backoff until they are written, the config is gone or ctx
// ends. Each write re-reads the config and updates it with optimistic
// concurrency, so a write based on a stale read fails with a conflict and is
// retried rather than overwriting newer counts. Increments that arrive while
// a write is in flight stay pending for the next one.
func (r *ProfilingConfigReconciler) flushStats(ctx context.Context, m *configMonitor, logger logr.Logger) {
	backoff := statsRetryInitial
	for {
		delta := m.pendingStats()
		if delta.isZero() {
			return
		}

		err := r.writeStats(ctx, m.key, delta)
		switch {
		case err == nil:
			m.statsWritten(delta)
			backoff = statsRetryInitial
			continue
		case errors.IsNotFound(err):
			m.dropStats() // the config was deleted
			return
		case errors.IsConflict(err):
			// Stale read or concurrent write; retry with a fresh read.
		default:
			logger.Error(err, "Failed to update profile counters; will retry")
		}

		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		if backoff *= 2; backoff > statsRetryMax {
			backoff = statsRetryMax
		}
	}
}

// writeStats adds delta to the config's status counters.
func (r *ProfilingConfigReconciler) writeStats(ctx context.Context, key types.NamespacedName, delta statsDelta) error {
	latest := &profilingv1alpha1.ProfilingConfig{}
	if err := r.Get(ctx, key, latest); err != nil {
		return err
	}
	latest.Status.TotalProfiles += delta.profiles
	latest.Status.TotalUploads += delta.uploads
	if !delta.lastProfile.IsZero() &&
		(latest.Status.LastProfileTime == nil || delta.lastProfile.After(latest.Status.LastProfileTime.Time)) {
		at := metav1.NewTime(delta.lastProfile)
		latest.Status.LastProfileTime = &at
	}
	// Update carries the resourceVersion that was read, so it fails with a
	// conflict if the status changed since.
	return r.Status().Update(ctx, latest)
}

// validateConfig validates the ProfilingConfig
func (r *ProfilingConfigReconciler) validateConfig(config *profilingv1alpha1.ProfilingConfig) error {
	if config.Spec.S3Config.Bucket == "" {
		return fmt.Errorf("s3 bucket is required")
	}
	if config.Spec.S3Config.Region == "" {
		return fmt.Errorf("s3 region is required")
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager
func (r *ProfilingConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// When the manager stops, cancel the base context (and all monitoring
	// goroutines), then wait for the monitors to write their last counts.
	if err := mgr.Add(runnableFunc(r.shutdown)); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&profilingv1alpha1.ProfilingConfig{}).
		Complete(r)
}

// shutdown stops all monitors and waits for their status writers.
func (r *ProfilingConfigReconciler) shutdown() {
	r.baseCancel()
	r.statsWriters.Wait()
}

// runnableFunc adapts a func() into ctrl.Runnable so we can register the
// shutdown hook with the manager.
type runnableFunc func()

func (f runnableFunc) Start(ctx context.Context) error {
	<-ctx.Done() // block until manager signals shutdown
	f()          // then stop all monitoring goroutines
	return nil
}
