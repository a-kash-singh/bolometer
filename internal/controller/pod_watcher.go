package controller

import (
	"context"
	"sort"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"

	profilingv1alpha1 "github.com/a-kash-singh/bolometer/api/v1alpha1"
)

const (
	// ProfilingEnabledAnnotation is the annotation that enables profiling
	ProfilingEnabledAnnotation = "bolometer.io/enabled"
)

// PodWatcher watches and tracks pods that should be profiled
type PodWatcher struct {
	clientset kubernetes.Interface

	// clock is used for cooldown bookkeeping; tests replace it with a fake.
	clock clock

	mu sync.RWMutex
	// trackedPods holds, for each ProfilingConfig (namespace/name), the pods
	// it selected on its last reconcile, keyed by pod namespace/name. Each
	// config's monitors only ever see their own set.
	trackedPods map[string]map[string]*TrackedPod
	// lastProfileTime records the last successful threshold-triggered capture
	// per pod. It must outlive re-tracking (every reconcile re-tracks every
	// matching pod) so that cooldownSeconds is honoured across reconciles,
	// and is dropped once no config tracks the pod any more.
	lastProfileTime map[string]time.Time
	// capturing holds the pods with a capture in flight. pprof can only run
	// one CPU profile per process at a time, so threshold and on-demand
	// captures of the same pod must not overlap.
	capturing map[string]struct{}
	// lastAttempt records when a capture of each pod last started, to give
	// every pod its turn when captures are limited.
	lastAttempt map[string]time.Time
}

// TrackedPod is a pod tracked for a ProfilingConfig
type TrackedPod struct {
	Pod    *corev1.Pod
	Config *profilingv1alpha1.ProfilingConfig
}

// NewPodWatcher creates a new pod watcher
func NewPodWatcher(clientset kubernetes.Interface) *PodWatcher {
	return &PodWatcher{
		clientset:       clientset,
		clock:           realClock{},
		trackedPods:     make(map[string]map[string]*TrackedPod),
		lastProfileTime: make(map[string]time.Time),
		capturing:       make(map[string]struct{}),
		lastAttempt:     make(map[string]time.Time),
	}
}

// ListMatchingPods lists pods that match the profiling config selector
func (pw *PodWatcher) ListMatchingPods(ctx context.Context, config *profilingv1alpha1.ProfilingConfig) ([]*corev1.Pod, error) {
	namespace := config.Spec.Selector.Namespace
	if namespace == "" {
		namespace = config.Namespace
	}

	// List pods with the profiling annotation
	listOptions := metav1.ListOptions{}

	// Add label selector if specified
	if len(config.Spec.Selector.LabelSelector) > 0 {
		selector := labels.SelectorFromSet(config.Spec.Selector.LabelSelector)
		listOptions.LabelSelector = selector.String()
	}

	podList, err := pw.clientset.CoreV1().Pods(namespace).List(ctx, listOptions)
	if err != nil {
		return nil, err
	}

	// Filter pods by annotation
	var matchingPods []*corev1.Pod
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pw.isPodProfilingEnabled(pod) && pod.Status.Phase == corev1.PodRunning {
			matchingPods = append(matchingPods, pod)
		}
	}

	return matchingPods, nil
}

// isPodProfilingEnabled checks if a pod has profiling enabled
func (pw *PodWatcher) isPodProfilingEnabled(pod *corev1.Pod) bool {
	if pod.Annotations == nil {
		return false
	}

	value, ok := pod.Annotations[ProfilingEnabledAnnotation]
	return ok && value == "true"
}

// TrackPod starts tracking a pod for a ProfilingConfig, or refreshes it if
// that config already tracks it. Refreshing keeps the pod's cooldown state.
// Tracking a pod for one config has no effect on other configs.
func (pw *PodWatcher) TrackPod(pod *corev1.Pod, config *profilingv1alpha1.ProfilingConfig) {
	pw.mu.Lock()
	defer pw.mu.Unlock()

	configKey := configKeyOf(config)
	pods, ok := pw.trackedPods[configKey]
	if !ok {
		pods = make(map[string]*TrackedPod)
		pw.trackedPods[configKey] = pods
	}
	// Entries are replaced rather than mutated: GetTrackedPods hands out the
	// pointers to monitor goroutines that read them without the lock.
	pods[pw.getPodKey(pod)] = &TrackedPod{Pod: pod, Config: config}
}

// SyncPods makes pods the complete set tracked for config: new pods are
// added, pods already tracked are refreshed (keeping their cooldown), and
// pods that are no longer in the set (deleted, no longer matching the
// selector, annotation or phase) are dropped.
func (pw *PodWatcher) SyncPods(config *profilingv1alpha1.ProfilingConfig, pods []*corev1.Pod) {
	pw.mu.Lock()
	defer pw.mu.Unlock()

	configKey := configKeyOf(config)
	previous := pw.trackedPods[configKey]

	current := make(map[string]*TrackedPod, len(pods))
	for _, pod := range pods {
		current[pw.getPodKey(pod)] = &TrackedPod{Pod: pod, Config: config}
	}
	if len(current) == 0 {
		delete(pw.trackedPods, configKey)
	} else {
		pw.trackedPods[configKey] = current
	}

	for podKey := range previous {
		if _, still := current[podKey]; !still {
			pw.releaseLocked(podKey)
		}
	}
}

// UntrackConfig drops every pod tracked for the ProfilingConfig with the
// given namespace/name key, e.g. because the config was deleted.
func (pw *PodWatcher) UntrackConfig(configKey string) {
	pw.mu.Lock()
	defer pw.mu.Unlock()

	previous := pw.trackedPods[configKey]
	delete(pw.trackedPods, configKey)
	for podKey := range previous {
		pw.releaseLocked(podKey)
	}
}

// StopTrackingPod stops tracking a pod for every config and drops its state.
func (pw *PodWatcher) StopTrackingPod(pod *corev1.Pod) {
	pw.mu.Lock()
	defer pw.mu.Unlock()

	podKey := pw.getPodKey(pod)
	for configKey, pods := range pw.trackedPods {
		if _, ok := pods[podKey]; !ok {
			continue
		}
		delete(pods, podKey)
		if len(pods) == 0 {
			delete(pw.trackedPods, configKey)
		}
		pw.releaseLocked(podKey)
	}
}

// releaseLocked cleans up after a pod was removed from one config's set. The
// pod's cooldown state is dropped once no config tracks it any more; an
// in-flight capture keeps its marker until FinishCapture. Must be called
// with the lock held.
func (pw *PodWatcher) releaseLocked(podKey string) {
	if !pw.isTrackedLocked(podKey) {
		delete(pw.lastProfileTime, podKey)
		if _, busy := pw.capturing[podKey]; !busy {
			delete(pw.lastAttempt, podKey)
		}
	}
}

// isTrackedLocked reports whether any config tracks the pod. Must be called
// with the lock held.
func (pw *PodWatcher) isTrackedLocked(podKey string) bool {
	for _, pods := range pw.trackedPods {
		if _, ok := pods[podKey]; ok {
			return true
		}
	}
	return false
}

// GetTrackedPods returns every tracked pod, once per config tracking it.
func (pw *PodWatcher) GetTrackedPods() []*TrackedPod {
	pw.mu.RLock()
	defer pw.mu.RUnlock()

	var out []*TrackedPod
	for _, pods := range pw.trackedPods {
		for _, tracked := range pods {
			out = append(out, tracked)
		}
	}
	return out
}

// GetTrackedPodsForConfig returns the pods tracked for the ProfilingConfig
// with the given namespace/name key, least recently attempted first: pods
// never captured come first, then by the start of their last capture, then
// by name. Visiting pods in this order gives each its turn when captures are
// limited.
func (pw *PodWatcher) GetTrackedPodsForConfig(configKey string) []*TrackedPod {
	pw.mu.RLock()
	defer pw.mu.RUnlock()

	pods := pw.trackedPods[configKey]
	out := make([]*TrackedPod, 0, len(pods))
	for _, tracked := range pods {
		out = append(out, tracked)
	}
	sort.Slice(out, func(i, j int) bool {
		ki, kj := pw.getPodKey(out[i].Pod), pw.getPodKey(out[j].Pod)
		ti, tj := pw.lastAttempt[ki], pw.lastAttempt[kj]
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return ki < kj
	})
	return out
}

// CanProfile checks if enough time has passed since last profile
func (pw *PodWatcher) CanProfile(pod *corev1.Pod, cooldownSeconds int) bool {
	pw.mu.RLock()
	defer pw.mu.RUnlock()

	key := pw.getPodKey(pod)
	lastTime, ok := pw.lastProfileTime[key]
	if !ok {
		return true
	}

	cooldown := time.Duration(cooldownSeconds) * time.Second
	return pw.now().Sub(lastTime) > cooldown
}

// UpdateLastProfileTime updates the last profile time for a pod
func (pw *PodWatcher) UpdateLastProfileTime(pod *corev1.Pod) {
	pw.mu.Lock()
	defer pw.mu.Unlock()

	key := pw.getPodKey(pod)
	pw.lastProfileTime[key] = pw.now()
}

// TryStartCapture marks a capture of the pod as in flight. It returns false
// if a capture of the pod is already running; otherwise the caller must call
// FinishCapture when done.
func (pw *PodWatcher) TryStartCapture(pod *corev1.Pod) bool {
	pw.mu.Lock()
	defer pw.mu.Unlock()

	if pw.capturing == nil {
		pw.capturing = make(map[string]struct{})
	}
	key := pw.getPodKey(pod)
	if _, busy := pw.capturing[key]; busy {
		return false
	}
	pw.capturing[key] = struct{}{}
	if pw.lastAttempt == nil {
		pw.lastAttempt = make(map[string]time.Time)
	}
	pw.lastAttempt[key] = pw.now()
	return true
}

// FinishCapture clears the in-flight marker set by TryStartCapture. If the
// pod stopped being tracked while the capture ran, the cooldown the capture
// may have recorded is dropped with it.
func (pw *PodWatcher) FinishCapture(pod *corev1.Pod) {
	pw.mu.Lock()
	defer pw.mu.Unlock()

	podKey := pw.getPodKey(pod)
	delete(pw.capturing, podKey)
	if !pw.isTrackedLocked(podKey) {
		delete(pw.lastProfileTime, podKey)
		delete(pw.lastAttempt, podKey)
	}
}

// IsCapturing reports whether a capture of the pod is in flight.
func (pw *PodWatcher) IsCapturing(pod *corev1.Pod) bool {
	pw.mu.RLock()
	defer pw.mu.RUnlock()
	_, busy := pw.capturing[pw.getPodKey(pod)]
	return busy
}

func (pw *PodWatcher) now() time.Time {
	if pw.clock == nil {
		return time.Now()
	}
	return pw.clock.Now()
}

// getPodKey generates a unique key for a pod
func (pw *PodWatcher) getPodKey(pod *corev1.Pod) string {
	return pod.Namespace + "/" + pod.Name
}

// GetActivePodCount returns the number of distinct tracked pods
func (pw *PodWatcher) GetActivePodCount() int {
	pw.mu.RLock()
	defer pw.mu.RUnlock()

	distinct := make(map[string]struct{})
	for _, pods := range pw.trackedPods {
		for podKey := range pods {
			distinct[podKey] = struct{}{}
		}
	}
	return len(distinct)
}

// configKeyOf returns the namespace/name key of a ProfilingConfig.
func configKeyOf(config *profilingv1alpha1.ProfilingConfig) string {
	return config.Namespace + "/" + config.Name
}
