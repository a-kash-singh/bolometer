package metrics

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"
)

// Collector collects and analyzes pod metrics
type Collector struct {
	metricsClient metricsv.Interface
}

// NewCollector creates a new metrics collector
func NewCollector(metricsClient metricsv.Interface) *Collector {
	return &Collector{
		metricsClient: metricsClient,
	}
}

// PodMetrics represents the resource usage of a pod
type PodMetrics struct {
	CPUUsagePercent    float64
	MemoryUsagePercent float64
	CPUUsage           resource.Quantity
	MemoryUsage        resource.Quantity
}

// GetPodMetrics retrieves metrics for a specific pod
func (c *Collector) GetPodMetrics(ctx context.Context, namespace, podName string, pod *corev1.Pod) (*PodMetrics, error) {
	podMetrics, err := c.metricsClient.MetricsV1beta1().PodMetricses(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get pod metrics: %w", err)
	}

	return c.calculateMetrics(pod, podMetrics)
}

// calculateMetrics calculates usage percentages based on requests
func (c *Collector) calculateMetrics(pod *corev1.Pod, podMetrics *v1beta1.PodMetrics) (*PodMetrics, error) {
	var totalCPUUsage, totalMemoryUsage resource.Quantity
	var totalCPURequest, totalMemoryRequest resource.Quantity

	// Aggregate metrics from all containers
	for _, container := range podMetrics.Containers {
		if cpu, ok := container.Usage[corev1.ResourceCPU]; ok {
			totalCPUUsage.Add(cpu)
		}
		if memory, ok := container.Usage[corev1.ResourceMemory]; ok {
			totalMemoryUsage.Add(memory)
		}
	}

	// Aggregate requests from pod spec
	for _, container := range pod.Spec.Containers {
		if cpu, ok := container.Resources.Requests[corev1.ResourceCPU]; ok {
			totalCPURequest.Add(cpu)
		}
		if memory, ok := container.Resources.Requests[corev1.ResourceMemory]; ok {
			totalMemoryRequest.Add(memory)
		}
	}

	// Calculate percentages
	cpuPercent := 0.0
	memoryPercent := 0.0

	if !totalCPURequest.IsZero() {
		cpuPercent = float64(totalCPUUsage.MilliValue()) / float64(totalCPURequest.MilliValue()) * 100
	}

	if !totalMemoryRequest.IsZero() {
		memoryPercent = float64(totalMemoryUsage.Value()) / float64(totalMemoryRequest.Value()) * 100
	}

	return &PodMetrics{
		CPUUsagePercent:    cpuPercent,
		MemoryUsagePercent: memoryPercent,
		CPUUsage:           totalCPUUsage,
		MemoryUsage:        totalMemoryUsage,
	}, nil
}

// CheckThresholds checks if metrics exceed configured thresholds
func (pm *PodMetrics) CheckThresholds(cpuThreshold, memoryThreshold int) (exceeded bool, reason string) {
	if pm.CPUUsagePercent > float64(cpuThreshold) {
		return true, fmt.Sprintf("CPU usage %.2f%% exceeds threshold %d%%", pm.CPUUsagePercent, cpuThreshold)
	}

	if pm.MemoryUsagePercent > float64(memoryThreshold) {
		return true, fmt.Sprintf("Memory usage %.2f%% exceeds threshold %d%%", pm.MemoryUsagePercent, memoryThreshold)
	}

	return false, ""
}
