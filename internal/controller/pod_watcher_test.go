package controller

import (
	"context"
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

	pod := createTestPod("pod-1", "default", true)
	config1 := createTestProfilingConfig("config-1", "default")
	config2 := createTestProfilingConfig("config-2", "default")

	// Track with first config
	watcher.TrackPod(pod, config1)

	// Track again with second config (should replace)
	watcher.TrackPod(pod, config2)

	tracked := watcher.GetTrackedPods()
	if len(tracked) != 1 {
		t.Errorf("Expected 1 tracked pod, got %d", len(tracked))
	}

	if len(tracked) > 0 && tracked[0].Config.Name != "config-2" {
		t.Errorf("Expected config 'config-2', got '%s'", tracked[0].Config.Name)
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
