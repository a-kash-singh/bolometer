package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	discoveryfake "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"
	metricsapiv1alpha1 "k8s.io/metrics/pkg/client/clientset/versioned/typed/metrics/v1alpha1"
	metricsapiv1beta1 "k8s.io/metrics/pkg/client/clientset/versioned/typed/metrics/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	profilingv1alpha1 "github.com/a-kash-singh/bolometer/api/v1alpha1"
)

// setupTestReconciler creates a test reconciler with fake clients
func setupTestReconciler(objs ...client.Object) *ProfilingConfigReconciler {
	scheme := runtime.NewScheme()
	_ = profilingv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&profilingv1alpha1.ProfilingConfig{}).
		Build()

	fakeClientset := fake.NewSimpleClientset()
	fakeMetricsClient := &fakeMetricsClientset{}

	reconciler := &ProfilingConfigReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		Clientset:      fakeClientset,
		MetricsClient:  fakeMetricsClient,
		RestConfig:     &rest.Config{},
		podWatcher:     NewPodWatcher(fakeClientset),
		activeMonitors: make(map[string]context.CancelFunc),
	}

	return reconciler
}

// createTestProfilingConfig creates a test ProfilingConfig
func createTestProfilingConfig(name, namespace string) *profilingv1alpha1.ProfilingConfig {
	return &profilingv1alpha1.ProfilingConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: profilingv1alpha1.ProfilingConfigSpec{
			Selector: profilingv1alpha1.PodSelector{
				Namespace: namespace,
				LabelSelector: map[string]string{
					"app": "test-app",
				},
			},
			Thresholds: profilingv1alpha1.ThresholdConfig{
				CPUThresholdPercent:    80,
				MemoryThresholdPercent: 90,
				CheckIntervalSeconds:   30,
				CooldownSeconds:        300,
			},
			S3Config: profilingv1alpha1.S3Configuration{
				Bucket: "test-bucket",
				Prefix: "profiles",
				Region: "us-west-2",
			},
			ProfileTypes: []string{"heap", "cpu"},
		},
	}
}

// createTestPod creates a test pod with profiling annotation
func createTestPod(name, namespace string, withAnnotation bool) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"app": "test-app",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "test-container",
					Image: "test:latest",
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}

	if withAnnotation {
		pod.Annotations = map[string]string{
			ProfilingEnabledAnnotation: "true",
		}
	}

	return pod
}

func TestReconcile_ConfigNotFound(t *testing.T) {
	reconciler := setupTestReconciler()

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "nonexistent",
			Namespace: "default",
		},
	}

	result, err := reconciler.Reconcile(context.Background(), req)

	if err != nil {
		t.Errorf("Reconcile returned unexpected error: %v", err)
	}

	if result.Requeue {
		t.Error("Expected no requeue when config not found")
	}

	// Verify monitoring is stopped
	configKey := req.NamespacedName.String()
	if _, ok := reconciler.activeMonitors[configKey]; ok {
		t.Error("Expected monitoring to be stopped for deleted config")
	}
}

func TestReconcile_ValidConfig(t *testing.T) {
	config := createTestProfilingConfig("test-config", "default")
	reconciler := setupTestReconciler(config)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      config.Name,
			Namespace: config.Namespace,
		},
	}

	result, err := reconciler.Reconcile(context.Background(), req)

	if err != nil {
		t.Errorf("Reconcile returned unexpected error: %v", err)
	}

	if !result.Requeue && result.RequeueAfter == 0 {
		t.Error("Expected requeue after interval")
	}

	if result.RequeueAfter != 30*time.Second {
		t.Errorf("Expected requeue after 30s, got %v", result.RequeueAfter)
	}

	// Verify monitoring is started
	configKey := req.NamespacedName.String()
	if _, ok := reconciler.activeMonitors[configKey]; !ok {
		t.Error("Expected monitoring to be started for valid config")
	}
}

func TestReconcile_InvalidConfig_MissingBucket(t *testing.T) {
	config := createTestProfilingConfig("test-config", "default")
	config.Spec.S3Config.Bucket = ""
	reconciler := setupTestReconciler(config)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      config.Name,
			Namespace: config.Namespace,
		},
	}

	_, err := reconciler.Reconcile(context.Background(), req)

	if err == nil {
		t.Error("Expected error for missing S3 bucket")
	}

	if err.Error() != "s3 bucket is required" {
		t.Errorf("Expected 's3 bucket is required' error, got: %v", err)
	}
}

func TestReconcile_InvalidConfig_MissingRegion(t *testing.T) {
	config := createTestProfilingConfig("test-config", "default")
	config.Spec.S3Config.Region = ""
	reconciler := setupTestReconciler(config)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      config.Name,
			Namespace: config.Namespace,
		},
	}

	_, err := reconciler.Reconcile(context.Background(), req)

	if err == nil {
		t.Error("Expected error for missing S3 region")
	}

	if err.Error() != "s3 region is required" {
		t.Errorf("Expected 's3 region is required' error, got: %v", err)
	}
}

func TestReconcile_StatusUpdate(t *testing.T) {
	config := createTestProfilingConfig("test-config", "default")
	pod := createTestPod("test-pod", "default", true)

	reconciler := setupTestReconciler(config, pod)

	// Create the pod in the fake clientset as well
	_, err := reconciler.Clientset.CoreV1().Pods("default").Create(
		context.Background(),
		pod,
		metav1.CreateOptions{},
	)
	if err != nil {
		t.Fatalf("Failed to create test pod: %v", err)
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      config.Name,
			Namespace: config.Namespace,
		},
	}

	_, err = reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("Reconcile returned unexpected error: %v", err)
	}

	// Fetch updated config
	updatedConfig := &profilingv1alpha1.ProfilingConfig{}
	err = reconciler.Get(context.Background(), req.NamespacedName, updatedConfig)
	if err != nil {
		t.Fatalf("Failed to get updated config: %v", err)
	}

	// Verify status was updated
	if updatedConfig.Status.ActivePods != 1 {
		t.Errorf("Expected ActivePods=1, got %d", updatedConfig.Status.ActivePods)
	}
}

func TestReconcile_MultiplePodsTracked(t *testing.T) {
	config := createTestProfilingConfig("test-config", "default")
	pod1 := createTestPod("test-pod-1", "default", true)
	pod2 := createTestPod("test-pod-2", "default", true)

	reconciler := setupTestReconciler(config, pod1, pod2)

	// Create pods in fake clientset
	_, err := reconciler.Clientset.CoreV1().Pods("default").Create(
		context.Background(),
		pod1,
		metav1.CreateOptions{},
	)
	if err != nil {
		t.Fatalf("Failed to create pod1: %v", err)
	}

	_, err = reconciler.Clientset.CoreV1().Pods("default").Create(
		context.Background(),
		pod2,
		metav1.CreateOptions{},
	)
	if err != nil {
		t.Fatalf("Failed to create pod2: %v", err)
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      config.Name,
			Namespace: config.Namespace,
		},
	}

	_, err = reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("Reconcile returned unexpected error: %v", err)
	}

	// Verify both pods are tracked
	tracked := reconciler.podWatcher.GetTrackedPods()
	if len(tracked) != 2 {
		t.Errorf("Expected 2 tracked pods, got %d", len(tracked))
	}

	// Verify status reflects multiple pods
	updatedConfig := &profilingv1alpha1.ProfilingConfig{}
	err = reconciler.Get(context.Background(), req.NamespacedName, updatedConfig)
	if err != nil {
		t.Fatalf("Failed to get updated config: %v", err)
	}

	if updatedConfig.Status.ActivePods != 2 {
		t.Errorf("Expected ActivePods=2, got %d", updatedConfig.Status.ActivePods)
	}
}

func TestReconcile_PodWithoutAnnotation(t *testing.T) {
	config := createTestProfilingConfig("test-config", "default")
	pod := createTestPod("test-pod", "default", false) // No annotation

	reconciler := setupTestReconciler(config, pod)

	_, err := reconciler.Clientset.CoreV1().Pods("default").Create(
		context.Background(),
		pod,
		metav1.CreateOptions{},
	)
	if err != nil {
		t.Fatalf("Failed to create test pod: %v", err)
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      config.Name,
			Namespace: config.Namespace,
		},
	}

	_, err = reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("Reconcile returned unexpected error: %v", err)
	}

	// Verify pod is not tracked
	tracked := reconciler.podWatcher.GetTrackedPods()
	if len(tracked) != 0 {
		t.Errorf("Expected 0 tracked pods, got %d", len(tracked))
	}

	// Verify status shows 0 active pods
	updatedConfig := &profilingv1alpha1.ProfilingConfig{}
	err = reconciler.Get(context.Background(), req.NamespacedName, updatedConfig)
	if err != nil {
		t.Fatalf("Failed to get updated config: %v", err)
	}

	if updatedConfig.Status.ActivePods != 0 {
		t.Errorf("Expected ActivePods=0, got %d", updatedConfig.Status.ActivePods)
	}
}

func TestReconcile_MonitoringRestartOnUpdate(t *testing.T) {
	config := createTestProfilingConfig("test-config", "default")
	reconciler := setupTestReconciler(config)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      config.Name,
			Namespace: config.Namespace,
		},
	}

	// First reconcile - start monitoring
	_, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("First reconcile failed: %v", err)
	}

	configKey := req.NamespacedName.String()
	firstCancel, ok := reconciler.activeMonitors[configKey]
	if !ok {
		t.Fatal("Expected monitoring to be started")
	}

	// Second reconcile - should restart monitoring
	_, err = reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("Second reconcile failed: %v", err)
	}

	secondCancel, ok := reconciler.activeMonitors[configKey]
	if !ok {
		t.Fatal("Expected monitoring to be restarted")
	}

	// Verify it's a new cancel function (monitoring was restarted)
	// We can't directly compare functions, but we can check they're both present
	if firstCancel == nil || secondCancel == nil {
		t.Error("Expected valid cancel functions")
	}
}

func TestReconcile_WithOnDemandEnabled(t *testing.T) {
	config := createTestProfilingConfig("test-config", "default")
	config.Spec.OnDemand = &profilingv1alpha1.OnDemandConfig{
		Enabled:         true,
		IntervalSeconds: 35,
	}

	reconciler := setupTestReconciler(config)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      config.Name,
			Namespace: config.Namespace,
		},
	}

	_, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("Reconcile returned unexpected error: %v", err)
	}

	// Verify monitoring is started
	configKey := req.NamespacedName.String()
	if _, ok := reconciler.activeMonitors[configKey]; !ok {
		t.Error("Expected monitoring to be started with on-demand enabled")
	}

	// Give goroutines time to start
	time.Sleep(10 * time.Millisecond)

	// Both threshold and on-demand monitoring should be active
	// (We can't easily verify this without complex mocking, but at least
	// we verify the reconcile succeeded)
}

func TestValidateConfig_Valid(t *testing.T) {
	config := createTestProfilingConfig("test-config", "default")
	reconciler := setupTestReconciler()

	err := reconciler.validateConfig(config)
	if err != nil {
		t.Errorf("Expected valid config, got error: %v", err)
	}
}

func TestValidateConfig_MissingBucket(t *testing.T) {
	config := createTestProfilingConfig("test-config", "default")
	config.Spec.S3Config.Bucket = ""
	reconciler := setupTestReconciler()

	err := reconciler.validateConfig(config)
	if err == nil {
		t.Error("Expected error for missing bucket")
	}
}

func TestValidateConfig_MissingRegion(t *testing.T) {
	config := createTestProfilingConfig("test-config", "default")
	config.Spec.S3Config.Region = ""
	reconciler := setupTestReconciler()

	err := reconciler.validateConfig(config)
	if err == nil {
		t.Error("Expected error for missing region")
	}
}

func TestStopMonitoring(t *testing.T) {
	config := createTestProfilingConfig("test-config", "default")
	reconciler := setupTestReconciler(config)

	// Start monitoring
	ctx := context.Background()
	reconciler.startMonitoring(ctx, config)

	configKey := config.Namespace + "/" + config.Name
	if _, ok := reconciler.activeMonitors[configKey]; !ok {
		t.Fatal("Expected monitoring to be started")
	}

	// Stop monitoring
	reconciler.stopMonitoring(configKey)

	if _, ok := reconciler.activeMonitors[configKey]; ok {
		t.Error("Expected monitoring to be stopped")
	}
}

func TestStopMonitoring_NotStarted(t *testing.T) {
	reconciler := setupTestReconciler()

	// Try to stop monitoring that was never started
	configKey := "default/nonexistent"
	reconciler.stopMonitoring(configKey) // Should not panic

	if _, ok := reconciler.activeMonitors[configKey]; ok {
		t.Error("Expected no monitoring entry")
	}
}

func TestReconcile_ConfigDeletion(t *testing.T) {
	config := createTestProfilingConfig("test-config", "default")
	reconciler := setupTestReconciler(config)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      config.Name,
			Namespace: config.Namespace,
		},
	}

	// First reconcile - start monitoring
	_, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("First reconcile failed: %v", err)
	}

	configKey := req.NamespacedName.String()
	if _, ok := reconciler.activeMonitors[configKey]; !ok {
		t.Fatal("Expected monitoring to be started")
	}

	// Delete the config
	err = reconciler.Delete(context.Background(), config)
	if err != nil {
		t.Fatalf("Failed to delete config: %v", err)
	}

	// Reconcile after deletion
	result, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("Reconcile after deletion failed: %v", err)
	}

	if result.Requeue {
		t.Error("Expected no requeue after deletion")
	}

	// Verify monitoring is stopped
	if _, ok := reconciler.activeMonitors[configKey]; ok {
		t.Error("Expected monitoring to be stopped after deletion")
	}
}

func TestReconcile_NamespaceIsolation(t *testing.T) {
	config := createTestProfilingConfig("test-config", "namespace-a")
	pod1 := createTestPod("pod-1", "namespace-a", true)
	pod2 := createTestPod("pod-2", "namespace-b", true)

	reconciler := setupTestReconciler(config, pod1, pod2)

	// Create pods in fake clientset
	_, err := reconciler.Clientset.CoreV1().Pods("namespace-a").Create(
		context.Background(),
		pod1,
		metav1.CreateOptions{},
	)
	if err != nil {
		t.Fatalf("Failed to create pod1: %v", err)
	}

	_, err = reconciler.Clientset.CoreV1().Pods("namespace-b").Create(
		context.Background(),
		pod2,
		metav1.CreateOptions{},
	)
	if err != nil {
		t.Fatalf("Failed to create pod2: %v", err)
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      config.Name,
			Namespace: config.Namespace,
		},
	}

	_, err = reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("Reconcile failed: %v", err)
	}

	// Verify only pod from namespace-a is tracked
	updatedConfig := &profilingv1alpha1.ProfilingConfig{}
	err = reconciler.Get(context.Background(), req.NamespacedName, updatedConfig)
	if err != nil {
		t.Fatalf("Failed to get updated config: %v", err)
	}

	if updatedConfig.Status.ActivePods != 1 {
		t.Errorf("Expected ActivePods=1 (only from namespace-a), got %d", updatedConfig.Status.ActivePods)
	}
}

func TestReconcile_GetError(t *testing.T) {
	// Create reconciler without the config in it
	reconciler := setupTestReconciler()

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-config",
			Namespace: "default",
		},
	}

	result, err := reconciler.Reconcile(context.Background(), req)

	// Should handle "not found" gracefully
	if err != nil {
		t.Errorf("Expected no error for not found, got: %v", err)
	}

	if result.Requeue {
		t.Error("Expected no requeue for not found")
	}
}

func TestNewProfilingConfigReconciler(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = profilingv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	fakeClient := fakeclient.NewClientBuilder().WithScheme(scheme).Build()
	fakeClientset := fake.NewSimpleClientset()
	fakeMetricsClient := &fakeMetricsClientset{}
	restConfig := &rest.Config{}

	reconciler := NewProfilingConfigReconciler(
		fakeClient,
		scheme,
		fakeClientset,
		fakeMetricsClient,
		restConfig,
	)

	if reconciler == nil {
		t.Fatal("Expected non-nil reconciler")
	}

	if reconciler.Client == nil {
		t.Error("Expected Client to be set")
	}

	if reconciler.Scheme == nil {
		t.Error("Expected Scheme to be set")
	}

	if reconciler.Clientset == nil {
		t.Error("Expected Clientset to be set")
	}

	if reconciler.MetricsClient == nil {
		t.Error("Expected MetricsClient to be set")
	}

	if reconciler.podWatcher == nil {
		t.Error("Expected podWatcher to be initialized")
	}

	if reconciler.metricsCollector == nil {
		t.Error("Expected metricsCollector to be initialized")
	}

	if reconciler.profiler == nil {
		t.Error("Expected profiler to be initialized")
	}

	if reconciler.activeMonitors == nil {
		t.Error("Expected activeMonitors map to be initialized")
	}
}

// Fake metrics clientset for testing
type fakeMetricsClientset struct {
	k8stesting.Fake
}

func (f *fakeMetricsClientset) Discovery() discovery.DiscoveryInterface {
	return &discoveryfake.FakeDiscovery{Fake: &f.Fake}
}

func (f *fakeMetricsClientset) MetricsV1beta1() metricsapiv1beta1.MetricsV1beta1Interface {
	return nil
}

func (f *fakeMetricsClientset) MetricsV1alpha1() metricsapiv1alpha1.MetricsV1alpha1Interface {
	return nil
}

// Ensure it implements the interface
var _ metricsv.Interface = &fakeMetricsClientset{}
