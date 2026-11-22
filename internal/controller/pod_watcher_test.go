package controller

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestNewPodWatcher(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	watcher := NewPodWatcher(clientset)

	if watcher == nil {
		t.Fatal("Expected non-nil PodWatcher")
	}

	if watcher.clientset == nil {
		t.Error("Expected clientset to be set")
	}

	if watcher.trackedPods == nil {
		t.Error("Expected trackedPods map to be initialized")
	}

	if watcher.lastProfileTime == nil {
		t.Error("Expected lastProfileTime map to be initialized")
	}
}

func TestPodWatcher_ListMatchingPods(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	watcher := NewPodWatcher(clientset)

	// Create test pods
	pod1 := createTestPod("pod-1", "default", true)
	pod2 := createTestPod("pod-2", "default", true)
	pod3 := createTestPod("pod-3", "default", false) // No annotation

	_, _ = clientset.CoreV1().Pods("default").Create(context.Background(), pod1, metav1.CreateOptions{})
	_, _ = clientset.CoreV1().Pods("default").Create(context.Background(), pod2, metav1.CreateOptions{})
	_, _ = clientset.CoreV1().Pods("default").Create(context.Background(), pod3, metav1.CreateOptions{})

	config := createTestProfilingConfig("test-config", "default")

	pods, err := watcher.ListMatchingPods(context.Background(), config)
	if err != nil {
		t.Fatalf("Failed to list pods: %v", err)
	}

	// Should only find pods with annotation and in running state
	if len(pods) != 2 {
		t.Errorf("Expected 2 matching pods, got %d", len(pods))
	}
}

func TestPodWatcher_ListMatchingPods_WithLabels(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	watcher := NewPodWatcher(clientset)

	// Create pods with different labels
	pod1 := createTestPod("pod-1", "default", true)
	pod1.Labels["app"] = "test-app"

	pod2 := createTestPod("pod-2", "default", true)
	pod2.Labels["app"] = "other-app"

	_, _ = clientset.CoreV1().Pods("default").Create(context.Background(), pod1, metav1.CreateOptions{})
	_, _ = clientset.CoreV1().Pods("default").Create(context.Background(), pod2, metav1.CreateOptions{})

	config := createTestProfilingConfig("test-config", "default")
	config.Spec.Selector.LabelSelector = map[string]string{"app": "test-app"}

	pods, err := watcher.ListMatchingPods(context.Background(), config)
	if err != nil {
		t.Fatalf("Failed to list pods: %v", err)
	}

	// Should only find pod matching label selector
	if len(pods) != 1 {
		t.Errorf("Expected 1 matching pod, got %d", len(pods))
	}

	if len(pods) > 0 && pods[0].Name != "pod-1" {
		t.Errorf("Expected pod-1, got %s", pods[0].Name)
	}
}

func TestPodWatcher_ListMatchingPods_DifferentNamespace(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	watcher := NewPodWatcher(clientset)

	pod1 := createTestPod("pod-1", "namespace-a", true)
	pod2 := createTestPod("pod-2", "namespace-b", true)

	_, _ = clientset.CoreV1().Pods("namespace-a").Create(context.Background(), pod1, metav1.CreateOptions{})
	_, _ = clientset.CoreV1().Pods("namespace-b").Create(context.Background(), pod2, metav1.CreateOptions{})

	config := createTestProfilingConfig("test-config", "default")
	config.Spec.Selector.Namespace = "namespace-a"

	pods, err := watcher.ListMatchingPods(context.Background(), config)
	if err != nil {
		t.Fatalf("Failed to list pods: %v", err)
	}

	if len(pods) != 1 {
		t.Errorf("Expected 1 pod from namespace-a, got %d", len(pods))
	}
}

func TestPodWatcher_ListMatchingPods_NonRunningPod(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	watcher := NewPodWatcher(clientset)

	pod := createTestPod("pod-1", "default", true)
	pod.Status.Phase = corev1.PodPending

	_, _ = clientset.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})

	config := createTestProfilingConfig("test-config", "default")

	pods, err := watcher.ListMatchingPods(context.Background(), config)
	if err != nil {
		t.Fatalf("Failed to list pods: %v", err)
	}

	// Should not include non-running pods
	if len(pods) != 0 {
		t.Errorf("Expected 0 non-running pods, got %d", len(pods))
	}
}

func TestPodWatcher_TrackPod(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	watcher := NewPodWatcher(clientset)

	pod := createTestPod("pod-1", "default", true)
	config := createTestProfilingConfig("test-config", "default")

	watcher.TrackPod(pod, config)

	tracked := watcher.GetTrackedPods()
	if len(tracked) != 1 {
		t.Errorf("Expected 1 tracked pod, got %d", len(tracked))
	}

	if len(tracked) > 0 {
		if tracked[0].Pod.Name != "pod-1" {
			t.Errorf("Expected tracked pod name 'pod-1', got '%s'", tracked[0].Pod.Name)
		}
		if tracked[0].Config.Name != "test-config" {
			t.Errorf("Expected config name 'test-config', got '%s'", tracked[0].Config.Name)
		}
	}
}

func TestPodWatcher_TrackPod_ReplaceExisting(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	watcher := NewPodWatcher(clientset)

	config := createTestProfilingConfig("config-1", "default")

	// Track, then track again with a newer copy of the same pod (as a later
	// reconcile does): the entry is replaced, not duplicated.
	watcher.TrackPod(createTestPod("pod-1", "default", true), config)
	updated := createTestPod("pod-1", "default", true)
	updated.Labels["version"] = "2"
	watcher.TrackPod(updated, config)

	tracked := watcher.GetTrackedPods()
	if len(tracked) != 1 {
		t.Fatalf("Expected 1 tracked pod, got %d", len(tracked))
	}
	if tracked[0].Pod.Labels["version"] != "2" {
		t.Errorf("Expected the tracked pod to be replaced by the newer copy")
	}
}

// A pod selected by two configs is tracked separately for each; tracking it
// for one config must not take it away from the other.
func TestPodWatcher_TrackPod_SeparatePerConfig(t *testing.T) {
	watcher := NewPodWatcher(fake.NewSimpleClientset())
	pod := createTestPod("pod-1", "default", true)
	config1 := createTestProfilingConfig("config-1", "default")
	config2 := createTestProfilingConfig("config-2", "default")

	watcher.TrackPod(pod, config1)
	watcher.TrackPod(pod, config2)

	for _, cfg := range []string{"default/config-1", "default/config-2"} {
		tracked := watcher.GetTrackedPodsForConfig(cfg)
		if len(tracked) != 1 || tracked[0].Pod.Name != "pod-1" || configKeyOf(tracked[0].Config) != cfg {
			t.Errorf("%s: expected exactly pod-1 tracked for it, got %d entries", cfg, len(tracked))
		}
	}
	if n := watcher.GetActivePodCount(); n != 1 {
		t.Errorf("Expected 1 distinct active pod, got %d", n)
	}
}

func TestPodWatcher_StopTrackingPod(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	watcher := NewPodWatcher(clientset)

	pod := createTestPod("pod-1", "default", true)
	config := createTestProfilingConfig("test-config", "default")

	watcher.TrackPod(pod, config)

	tracked := watcher.GetTrackedPods()
	if len(tracked) != 1 {
		t.Fatalf("Expected 1 tracked pod initially, got %d", len(tracked))
	}

	watcher.StopTrackingPod(pod)

	tracked = watcher.GetTrackedPods()
	if len(tracked) != 0 {
		t.Errorf("Expected 0 tracked pods after stopping, got %d", len(tracked))
	}
}

func TestPodWatcher_GetTrackedPods(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	watcher := NewPodWatcher(clientset)

	config := createTestProfilingConfig("test-config", "default")

	// Initially empty
	tracked := watcher.GetTrackedPods()
	if len(tracked) != 0 {
		t.Errorf("Expected 0 tracked pods initially, got %d", len(tracked))
	}

	// Add multiple pods
	pod1 := createTestPod("pod-1", "default", true)
	pod2 := createTestPod("pod-2", "default", true)
	pod3 := createTestPod("pod-3", "default", true)

	watcher.TrackPod(pod1, config)
	watcher.TrackPod(pod2, config)
	watcher.TrackPod(pod3, config)

	tracked = watcher.GetTrackedPods()
	if len(tracked) != 3 {
		t.Errorf("Expected 3 tracked pods, got %d", len(tracked))
	}
}

func TestPodWatcher_CanProfile_FirstTime(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	watcher := NewPodWatcher(clientset)

	pod := createTestPod("pod-1", "default", true)

	// First time should always return true
	if !watcher.CanProfile(pod, 300) {
		t.Error("Expected CanProfile to return true for first profile")
	}
}

func TestPodWatcher_CanProfile_WithinCooldown(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	watcher := NewPodWatcher(clientset)

	pod := createTestPod("pod-1", "default", true)

	// Update last profile time
	watcher.UpdateLastProfileTime(pod)

	// Should not be able to profile within cooldown
	if watcher.CanProfile(pod, 300) {
		t.Error("Expected CanProfile to return false within cooldown period")
	}
}

func TestPodWatcher_CanProfile_AfterCooldown(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	watcher := NewPodWatcher(clientset)

	pod := createTestPod("pod-1", "default", true)

	// Manually set last profile time to past
	key := watcher.getPodKey(pod)
	watcher.mu.Lock()
	watcher.lastProfileTime[key] = time.Now().Add(-10 * time.Minute)
	watcher.mu.Unlock()

	// Should be able to profile after cooldown
	if !watcher.CanProfile(pod, 300) {
		t.Error("Expected CanProfile to return true after cooldown period")
	}
}

func TestPodWatcher_UpdateLastProfileTime(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	watcher := NewPodWatcher(clientset)

	pod := createTestPod("pod-1", "default", true)

	before := time.Now()
	watcher.UpdateLastProfileTime(pod)
	after := time.Now()

	key := watcher.getPodKey(pod)
	watcher.mu.RLock()
	lastTime, ok := watcher.lastProfileTime[key]
	watcher.mu.RUnlock()

	if !ok {
		t.Error("Expected last profile time to be set")
	}

	if lastTime.Before(before) || lastTime.After(after) {
		t.Error("Last profile time not in expected range")
	}
}

func TestPodWatcher_GetActivePodCount(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	watcher := NewPodWatcher(clientset)

	config := createTestProfilingConfig("test-config", "default")

	// Initially 0
	if watcher.GetActivePodCount() != 0 {
		t.Errorf("Expected 0 active pods, got %d", watcher.GetActivePodCount())
	}

	// Add pods
	pod1 := createTestPod("pod-1", "default", true)
	pod2 := createTestPod("pod-2", "default", true)

	watcher.TrackPod(pod1, config)
	if watcher.GetActivePodCount() != 1 {
		t.Errorf("Expected 1 active pod, got %d", watcher.GetActivePodCount())
	}

	watcher.TrackPod(pod2, config)
	if watcher.GetActivePodCount() != 2 {
		t.Errorf("Expected 2 active pods, got %d", watcher.GetActivePodCount())
	}

	// Remove pod
	watcher.StopTrackingPod(pod1)
	if watcher.GetActivePodCount() != 1 {
		t.Errorf("Expected 1 active pod after removal, got %d", watcher.GetActivePodCount())
	}
}

func TestPodWatcher_IsPodProfilingEnabled(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	watcher := NewPodWatcher(clientset)

	tests := []struct {
		name     string
		pod      *corev1.Pod
		expected bool
	}{
		{
			name:     "Pod with profiling enabled",
			pod:      createTestPod("pod-1", "default", true),
			expected: true,
		},
		{
			name:     "Pod without annotation",
			pod:      createTestPod("pod-2", "default", false),
			expected: false,
		},
		{
			name: "Pod with false annotation",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod-3",
					Namespace: "default",
					Annotations: map[string]string{
						ProfilingEnabledAnnotation: "false",
					},
				},
			},
			expected: false,
		},
		{
			name: "Pod with nil annotations",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod-4",
					Namespace: "default",
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := watcher.isPodProfilingEnabled(tt.pod)
			if result != tt.expected {
				t.Errorf("Expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestPodWatcher_GetPodKey(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	watcher := NewPodWatcher(clientset)

	pod := createTestPod("test-pod", "test-namespace", true)

	key := watcher.getPodKey(pod)
	expected := "test-namespace/test-pod"

	if key != expected {
		t.Errorf("Expected key '%s', got '%s'", expected, key)
	}
}

func TestPodWatcher_ConcurrentAccess(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	watcher := NewPodWatcher(clientset)

	config := createTestProfilingConfig("test-config", "default")

	// Test concurrent reads and writes
	done := make(chan bool)

	// Writer goroutine
	go func() {
		for i := 0; i < 100; i++ {
			pod := createTestPod("pod-1", "default", true)
			watcher.TrackPod(pod, config)
			time.Sleep(time.Microsecond)
		}
		done <- true
	}()

	// Reader goroutine
	go func() {
		for i := 0; i < 100; i++ {
			_ = watcher.GetTrackedPods()
			_ = watcher.GetActivePodCount()
			time.Sleep(time.Microsecond)
		}
		done <- true
	}()

	// Wait for both goroutines
	<-done
	<-done

	// If we get here without deadlock or race, the test passes
}

// Reconcile re-tracks every matching pod; that used to wipe the pod's
// cooldown, allowing it to be profiled again immediately.
func TestPodWatcher_TrackPod_PreservesCooldown(t *testing.T) {
	watcher := NewPodWatcher(fake.NewSimpleClientset())
	config := createTestProfilingConfig("test-config", "default")
	pod := createTestPod("pod-1", "default", true)

	watcher.TrackPod(pod, config)
	watcher.UpdateLastProfileTime(pod)

	// Same pod as returned by a later List.
	watcher.TrackPod(createTestPod("pod-1", "default", true), config)

	if watcher.CanProfile(pod, 300) {
		t.Error("re-tracking the pod reset its cooldown")
	}
}

func TestPodWatcher_StopTrackingPod_ClearsCooldown(t *testing.T) {
	watcher := NewPodWatcher(fake.NewSimpleClientset())
	config := createTestProfilingConfig("test-config", "default")
	pod := createTestPod("pod-1", "default", true)

	watcher.TrackPod(pod, config)
	watcher.UpdateLastProfileTime(pod)
	watcher.StopTrackingPod(pod)

	if !watcher.CanProfile(pod, 300) {
		t.Error("expected cooldown to be cleared once the pod is no longer tracked")
	}
}

func TestPodWatcher_TryStartCapture(t *testing.T) {
	watcher := NewPodWatcher(fake.NewSimpleClientset())
	pod := createTestPod("pod-1", "default", true)
	other := createTestPod("pod-2", "default", true)

	if !watcher.TryStartCapture(pod) {
		t.Fatal("first capture of pod should start")
	}
	if watcher.TryStartCapture(pod) {
		t.Fatal("second concurrent capture of the same pod should be refused")
	}
	if !watcher.TryStartCapture(other) {
		t.Fatal("capture of a different pod should start")
	}
	watcher.FinishCapture(pod)
	if !watcher.TryStartCapture(pod) {
		t.Fatal("capture should start again after FinishCapture")
	}
}

func podNames(tracked []*TrackedPod) []string {
	var names []string
	for _, tp := range tracked {
		names = append(names, tp.Pod.Name)
	}
	sort.Strings(names)
	return names
}

func TestPodWatcher_SyncPods(t *testing.T) {
	watcher := NewPodWatcher(fake.NewSimpleClientset())
	config := createTestProfilingConfig("config-1", "default")
	pod0 := createTestPod("pod-0", "default", true)
	pod1 := createTestPod("pod-1", "default", true)
	pod2 := createTestPod("pod-2", "default", true)

	watcher.SyncPods(config, []*corev1.Pod{pod0, pod1})
	watcher.UpdateLastProfileTime(pod0)
	watcher.UpdateLastProfileTime(pod1)

	// pod-1 is gone, pod-2 is new.
	watcher.SyncPods(config, []*corev1.Pod{createTestPod("pod-0", "default", true), pod2})

	if got := podNames(watcher.GetTrackedPodsForConfig("default/config-1")); strings.Join(got, ",") != "pod-0,pod-2" {
		t.Fatalf("tracked pods: got %v, want [pod-0 pod-2]", got)
	}
	if watcher.CanProfile(pod0, 300) {
		t.Error("pod-0 is still selected; its cooldown must be kept")
	}
	if !watcher.CanProfile(pod1, 300) {
		t.Error("pod-1 was dropped; its cooldown state must be dropped too")
	}

	// An empty selection drops everything, including the config's entry.
	watcher.SyncPods(config, nil)
	if n := len(watcher.GetTrackedPods()); n != 0 {
		t.Errorf("expected no tracked pods, got %d", n)
	}
	if n := len(watcher.trackedPods) + len(watcher.lastProfileTime); n != 0 {
		t.Errorf("expected no retained state, got %d trackedPods and lastProfileTime entries", n)
	}
}

// A pod dropped by one config but still selected by another keeps its
// cooldown; it is released once the last config drops it.
func TestPodWatcher_SharedPodCooldownReleasedWithLastConfig(t *testing.T) {
	watcher := NewPodWatcher(fake.NewSimpleClientset())
	pod := createTestPod("pod-0", "default", true)
	config1 := createTestProfilingConfig("config-1", "default")
	config2 := createTestProfilingConfig("config-2", "default")

	watcher.SyncPods(config1, []*corev1.Pod{pod})
	watcher.SyncPods(config2, []*corev1.Pod{pod})
	watcher.UpdateLastProfileTime(pod)

	watcher.SyncPods(config1, nil)
	if watcher.CanProfile(pod, 300) {
		t.Error("config-2 still tracks the pod; its cooldown must be kept")
	}
	if got := podNames(watcher.GetTrackedPodsForConfig("default/config-2")); len(got) != 1 {
		t.Errorf("config-2 should still track the pod, got %v", got)
	}

	watcher.UntrackConfig("default/config-2")
	if !watcher.CanProfile(pod, 300) {
		t.Error("no config tracks the pod any more; its cooldown state must be dropped")
	}
	if n := len(watcher.GetTrackedPods()); n != 0 {
		t.Errorf("expected no tracked pods, got %d", n)
	}
}

func TestPodWatcher_UntrackConfig_LeavesOtherConfigsAlone(t *testing.T) {
	watcher := NewPodWatcher(fake.NewSimpleClientset())
	configA := createTestProfilingConfig("config-a", "default")
	configB := createTestProfilingConfig("config-b", "default")
	watcher.SyncPods(configA, []*corev1.Pod{createTestPod("a-0", "default", true)})
	watcher.SyncPods(configB, []*corev1.Pod{createTestPod("b-0", "default", true)})

	watcher.UntrackConfig("default/config-a")

	if got := podNames(watcher.GetTrackedPods()); strings.Join(got, ",") != "b-0" {
		t.Errorf("tracked pods: got %v, want [b-0]", got)
	}
	if got := watcher.GetTrackedPodsForConfig("default/config-a"); len(got) != 0 {
		t.Errorf("config-a should track nothing, got %d pods", len(got))
	}
}

func TestPodWatcher_StopTrackingPod_AllConfigs(t *testing.T) {
	watcher := NewPodWatcher(fake.NewSimpleClientset())
	pod := createTestPod("pod-0", "default", true)
	watcher.TrackPod(pod, createTestProfilingConfig("config-1", "default"))
	watcher.TrackPod(pod, createTestProfilingConfig("config-2", "default"))
	watcher.UpdateLastProfileTime(pod)

	watcher.StopTrackingPod(pod)

	if n := len(watcher.GetTrackedPods()); n != 0 {
		t.Errorf("expected the pod to be untracked for every config, got %d entries", n)
	}
	if !watcher.CanProfile(pod, 300) {
		t.Error("expected the pod's cooldown state to be dropped")
	}
}

func TestPodWatcher_IsCapturing(t *testing.T) {
	watcher := NewPodWatcher(fake.NewSimpleClientset())
	pod := createTestPod("pod-0", "default", true)

	if watcher.IsCapturing(pod) {
		t.Fatal("no capture started yet")
	}
	watcher.TryStartCapture(pod)
	if !watcher.IsCapturing(pod) {
		t.Fatal("expected capture in flight")
	}
	watcher.FinishCapture(pod)
	if watcher.IsCapturing(pod) {
		t.Fatal("expected no capture in flight after FinishCapture")
	}
}

// A capture can finish after its pod stopped being tracked (deleted, no
// longer matching, config deleted). The cooldown it records must not be left
// behind once the capture is released.
func TestPodWatcher_FinishCapture_DropsCooldownOfUntrackedPod(t *testing.T) {
	watcher := NewPodWatcher(fake.NewSimpleClientset())
	config := createTestProfilingConfig("config-1", "default")
	gone := createTestPod("gone", "default", true)
	kept := createTestPod("kept", "default", true)
	watcher.SyncPods(config, []*corev1.Pod{gone, kept})

	watcher.TryStartCapture(gone)
	watcher.TryStartCapture(kept)
	watcher.SyncPods(config, []*corev1.Pod{kept}) // "gone" dropped mid-capture
	watcher.UpdateLastProfileTime(gone)
	watcher.UpdateLastProfileTime(kept)
	watcher.FinishCapture(gone)
	watcher.FinishCapture(kept)

	if _, held := watcher.lastProfileTime["default/gone"]; held {
		t.Error("cooldown of the untracked pod was left behind")
	}
	if watcher.CanProfile(kept, 300) {
		t.Error("cooldown of the still-tracked pod must be kept")
	}
}

// Pods are returned least recently attempted first (never attempted first),
// then by name, so a capture limit rotates through them.
func TestPodWatcher_GetTrackedPodsForConfig_LeastRecentlyAttemptedFirst(t *testing.T) {
	watcher := NewPodWatcher(fake.NewSimpleClientset())
	clk := newFakeClock()
	watcher.clock = clk
	config := createTestProfilingConfig("config-1", "default")
	var pods []*corev1.Pod
	for _, name := range []string{"d", "c", "b", "a"} {
		pods = append(pods, createTestPod(name, "default", true))
	}
	watcher.SyncPods(config, pods)

	order := func() string {
		return strings.Join(func() (names []string) {
			for _, tp := range watcher.GetTrackedPodsForConfig("default/config-1") {
				names = append(names, tp.Pod.Name)
			}
			return
		}(), ",")
	}
	if got := order(); got != "a,b,c,d" {
		t.Fatalf("never attempted: got %s, want a,b,c,d", got)
	}

	attempt := func(name string) {
		pod := createTestPod(name, "default", true)
		watcher.TryStartCapture(pod)
		watcher.FinishCapture(pod)
		clk.Step(time.Second)
	}
	attempt("a")
	attempt("c")
	if got := order(); got != "b,d,a,c" {
		t.Fatalf("after capturing a then c: got %s, want b,d,a,c", got)
	}
	attempt("b")
	attempt("d")
	attempt("a")
	if got := order(); got != "c,b,d,a" {
		t.Fatalf("got %s, want c,b,d,a", got)
	}
}
