package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	profilingv1alpha1 "github.com/a-kash-singh/bolometer/api/v1alpha1"
	"github.com/a-kash-singh/bolometer/internal/metrics"
	"github.com/a-kash-singh/bolometer/internal/profiler"
	"github.com/a-kash-singh/bolometer/internal/uploader"
)

// ProfilingConfigReconciler reconciles a ProfilingConfig object
type ProfilingConfigReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	Clientset     kubernetes.Interface
	MetricsClient metricsv.Interface
	RestConfig    *rest.Config

	podWatcher       *PodWatcher
	metricsCollector *metrics.Collector
	profiler         *profiler.Profiler

	// Track active monitoring goroutines
	activeMonitors map[string]context.CancelFunc
}

// NewProfilingConfigReconciler creates a new reconciler
func NewProfilingConfigReconciler(
	client client.Client,
	scheme *runtime.Scheme,
	clientset kubernetes.Interface,
	metricsClient metricsv.Interface,
	restConfig *rest.Config,
) *ProfilingConfigReconciler {
	return &ProfilingConfigReconciler{
		Client:           client,
		Scheme:           scheme,
		Clientset:        clientset,
		MetricsClient:    metricsClient,
		RestConfig:       restConfig,
		podWatcher:       NewPodWatcher(clientset),
		metricsCollector: metrics.NewCollector(metricsClient),
		profiler:         profiler.NewProfiler(clientset, restConfig),
		activeMonitors:   make(map[string]context.CancelFunc),
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
			// Object deleted, stop monitoring
			r.stopMonitoring(req.NamespacedName.String())
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Validate configuration
	if err := r.validateConfig(config); err != nil {
		logger.Error(err, "Invalid configuration")
		return ctrl.Result{}, err
	}

	// List matching pods
	pods, err := r.podWatcher.ListMatchingPods(ctx, config)
	if err != nil {
		logger.Error(err, "Failed to list pods")
		return ctrl.Result{}, err
	}

	logger.Info("Found matching pods", "count", len(pods))

	// Track all matching pods
	for _, pod := range pods {
		r.podWatcher.TrackPod(pod, config)
	}

	// Update status
	config.Status.ActivePods = len(pods)
	if err := r.Status().Update(ctx, config); err != nil {
		logger.Error(err, "Failed to update status")
	}

	// Start or update monitoring
	configKey := req.NamespacedName.String()
	r.stopMonitoring(configKey)
	r.startMonitoring(ctx, config)

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// startMonitoring starts monitoring for a ProfilingConfig
func (r *ProfilingConfigReconciler) startMonitoring(parentCtx context.Context, config *profilingv1alpha1.ProfilingConfig) {
	configKey := config.Namespace + "/" + config.Name
	ctx, cancel := context.WithCancel(parentCtx)
	r.activeMonitors[configKey] = cancel

	// Start threshold-based monitoring
	go r.monitorThresholds(ctx, config)

	// Start on-demand monitoring if enabled
	if config.Spec.OnDemand != nil && config.Spec.OnDemand.Enabled {
		go r.monitorOnDemand(ctx, config)
	}
}

// stopMonitoring stops monitoring for a ProfilingConfig
func (r *ProfilingConfigReconciler) stopMonitoring(configKey string) {
	if cancel, ok := r.activeMonitors[configKey]; ok {
		cancel()
		delete(r.activeMonitors, configKey)
	}
}

// monitorThresholds monitors pods for threshold violations
func (r *ProfilingConfigReconciler) monitorThresholds(ctx context.Context, config *profilingv1alpha1.ProfilingConfig) {
	logger := log.FromContext(ctx)
	checkInterval := time.Duration(config.Spec.Thresholds.CheckIntervalSeconds) * time.Second
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.checkPodsThresholds(ctx, config, logger)
		}
	}
}

// checkPodsThresholds checks all tracked pods for threshold violations
func (r *ProfilingConfigReconciler) checkPodsThresholds(ctx context.Context, config *profilingv1alpha1.ProfilingConfig, logger logr.Logger) {
	trackedPods := r.podWatcher.GetTrackedPods()

	for _, tracked := range trackedPods {
		// Skip if in cooldown period
		if !r.podWatcher.CanProfile(tracked.Pod, config.Spec.Thresholds.CooldownSeconds) {
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
			logger.Info("Threshold exceeded, capturing profile",
				"pod", tracked.Pod.Name,
				"reason", reason,
			)

			if err := r.captureAndUpload(ctx, tracked.Pod, config, reason); err != nil {
				logger.Error(err, "Failed to capture and upload profile", "pod", tracked.Pod.Name)
			} else {
				r.podWatcher.UpdateLastProfileTime(tracked.Pod)
				r.updateProfileStats(ctx, config)
			}
		}
	}
}

// monitorOnDemand performs on-demand continuous profiling
func (r *ProfilingConfigReconciler) monitorOnDemand(ctx context.Context, config *profilingv1alpha1.ProfilingConfig) {
	logger := log.FromContext(ctx)
	interval := time.Duration(config.Spec.OnDemand.IntervalSeconds) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			trackedPods := r.podWatcher.GetTrackedPods()
			for _, tracked := range trackedPods {
				logger.Info("On-demand profiling", "pod", tracked.Pod.Name)

				if err := r.captureAndUpload(ctx, tracked.Pod, config, "on-demand"); err != nil {
					logger.Error(err, "Failed to capture on-demand profile", "pod", tracked.Pod.Name)
				} else {
					r.updateProfileStats(ctx, config)
				}
			}
		}
	}
}

// captureAndUpload captures profiles and uploads them to S3
func (r *ProfilingConfigReconciler) captureAndUpload(ctx context.Context, pod *corev1.Pod, config *profilingv1alpha1.ProfilingConfig, reason string) error {
	// Determine which profile types to capture
	profileTypes := config.Spec.ProfileTypes
	if len(profileTypes) == 0 {
		profileTypes = []string{"heap", "cpu", "goroutine", "mutex"}
	}

	// Capture profiles
	profiles, err := r.profiler.CaptureProfiles(ctx, pod, profileTypes)
	if err != nil {
		return fmt.Errorf("failed to capture profiles: %w", err)
	}

	// Create S3 uploader
	s3Uploader, err := uploader.NewS3Uploader(ctx, uploader.S3Config{
		Bucket:   config.Spec.S3Config.Bucket,
		Prefix:   config.Spec.S3Config.Prefix,
		Region:   config.Spec.S3Config.Region,
		Endpoint: config.Spec.S3Config.Endpoint,
	})
	if err != nil {
		return fmt.Errorf("failed to create S3 uploader: %w", err)
	}

	// Upload profiles
	if err := s3Uploader.UploadProfiles(ctx, pod, profiles, reason); err != nil {
		return fmt.Errorf("failed to upload profiles: %w", err)
	}

	return nil
}

// updateProfileStats updates the profile statistics in the status
func (r *ProfilingConfigReconciler) updateProfileStats(ctx context.Context, config *profilingv1alpha1.ProfilingConfig) {
	// Fetch latest version
	latest := &profilingv1alpha1.ProfilingConfig{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(config), latest); err != nil {
		return
	}

	now := metav1.Now()
	latest.Status.LastProfileTime = &now
	latest.Status.TotalProfiles++
	latest.Status.TotalUploads++

	if err := r.Status().Update(ctx, latest); err != nil {
		// Log but don't fail
		log.FromContext(ctx).Error(err, "Failed to update stats")
	}
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
	return ctrl.NewControllerManagedBy(mgr).
		For(&profilingv1alpha1.ProfilingConfig{}).
		Complete(r)
}
