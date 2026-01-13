package controller

import (
	"context"
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

	mu              sync.RWMutex
	trackedPods     map[string]*TrackedPod
	lastProfileTime map[string]time.Time
}

// TrackedPod represents a pod being monitored for profiling
type TrackedPod struct {
	Pod             *corev1.Pod
	Config          *profilingv1alpha1.ProfilingConfig
	LastProfileTime time.Time
	OnDemandTicker  *time.Ticker
	StopChan        chan struct{}
}

// NewPodWatcher creates a new pod watcher
func NewPodWatcher(clientset kubernetes.Interface) *PodWatcher {
	return &PodWatcher{
		clientset:       clientset,
		trackedPods:     make(map[string]*TrackedPod),
		lastProfileTime: make(map[string]time.Time),
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

// TrackPod starts tracking a pod for profiling
func (pw *PodWatcher) TrackPod(pod *corev1.Pod, config *profilingv1alpha1.ProfilingConfig) {
	pw.mu.Lock()
	defer pw.mu.Unlock()

	key := pw.getPodKey(pod)

	// Stop existing tracking if any
	if existing, ok := pw.trackedPods[key]; ok {
		pw.stopTrackingLocked(key, existing)
	}

	tracked := &TrackedPod{
		Pod:    pod,
		Config: config,
	}

	pw.trackedPods[key] = tracked
}

// StopTrackingPod stops tracking a pod
func (pw *PodWatcher) StopTrackingPod(pod *corev1.Pod) {
	pw.mu.Lock()
	defer pw.mu.Unlock()

	key := pw.getPodKey(pod)
	if tracked, ok := pw.trackedPods[key]; ok {
		pw.stopTrackingLocked(key, tracked)
	}
}

// stopTrackingLocked stops tracking (must be called with lock held)
func (pw *PodWatcher) stopTrackingLocked(key string, tracked *TrackedPod) {
	if tracked.StopChan != nil {
		close(tracked.StopChan)
	}
	if tracked.OnDemandTicker != nil {
		tracked.OnDemandTicker.Stop()
	}
	delete(pw.trackedPods, key)
	delete(pw.lastProfileTime, key)
}

// GetTrackedPods returns all currently tracked pods
func (pw *PodWatcher) GetTrackedPods() []*TrackedPod {
	pw.mu.RLock()
	defer pw.mu.RUnlock()

	pods := make([]*TrackedPod, 0, len(pw.trackedPods))
	for _, tracked := range pw.trackedPods {
		pods = append(pods, tracked)
	}

	return pods
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
	return time.Since(lastTime) > cooldown
}

// UpdateLastProfileTime updates the last profile time for a pod
func (pw *PodWatcher) UpdateLastProfileTime(pod *corev1.Pod) {
	pw.mu.Lock()
	defer pw.mu.Unlock()

	key := pw.getPodKey(pod)
	pw.lastProfileTime[key] = time.Now()
}

// getPodKey generates a unique key for a pod
func (pw *PodWatcher) getPodKey(pod *corev1.Pod) string {
	return pod.Namespace + "/" + pod.Name
}

// GetActivePodCount returns the number of tracked pods
func (pw *PodWatcher) GetActivePodCount() int {
	pw.mu.RLock()
	defer pw.mu.RUnlock()
	return len(pw.trackedPods)
}
