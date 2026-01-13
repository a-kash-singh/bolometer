package uploader

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/a-kash-singh/bolometer/internal/profiler"
)

func TestGetServiceName(t *testing.T) {
	tests := []struct {
		name        string
		pod         *corev1.Pod
		expected    string
		description string
	}{
		{
			name: "app.kubernetes.io/name label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-pod-abc123-xyz456",
					Labels: map[string]string{
						"app.kubernetes.io/name": "my-service",
						"app":                    "other-app",
					},
				},
			},
			expected:    "my-service",
			description: "Should prioritize app.kubernetes.io/name",
		},
		{
			name: "app label only",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-pod-abc123-xyz456",
					Labels: map[string]string{
						"app": "payment-service",
					},
				},
			},
			expected:    "payment-service",
			description: "Should use app label when k8s label not present",
		},
		{
			name: "k8s-app label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-pod-abc123-xyz456",
					Labels: map[string]string{
						"k8s-app": "auth-service",
					},
				},
			},
			expected:    "auth-service",
			description: "Should use k8s-app label",
		},
		{
			name: "owner reference",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "web-app-7d8f9c5b6d-xyz456",
					OwnerReferences: []metav1.OwnerReference{
						{
							Kind: "ReplicaSet",
							Name: "web-app-7d8f9c5b6d",
						},
					},
				},
			},
			expected:    "web-app",
			description: "Should extract from ReplicaSet owner, removing hash",
		},
		{
			name: "statefulset owner",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "database-0",
					OwnerReferences: []metav1.OwnerReference{
						{
							Kind: "StatefulSet",
							Name: "database",
						},
					},
				},
			},
			expected:    "database",
			description: "Should use StatefulSet name directly",
		},
		{
			name: "fallback to pod name",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "standalone-service-abc123-xyz456",
				},
			},
			expected:    "standalone-service",
			description: "Should extract prefix from pod name",
		},
		{
			name: "simple pod name",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "simple-pod",
				},
			},
			expected:    "simple-pod",
			description: "Should use entire pod name if no dashes with hashes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uploader := &S3Uploader{
				bucket: "test-bucket",
				prefix: "test",
			}

			result := uploader.getServiceName(tt.pod)

			if result != tt.expected {
				t.Errorf("%s: expected %q, got %q", tt.description, tt.expected, result)
			}
		})
	}
}

func TestGenerateKey(t *testing.T) {
	uploader := &S3Uploader{
		bucket: "test-bucket",
		prefix: "profiles",
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-app-abc123-xyz456",
			Namespace: "production",
			Labels: map[string]string{
				"app": "test-app",
			},
		},
	}

	timestamp := time.Date(2024, 1, 15, 12, 30, 45, 0, time.UTC)
	profile := profiler.Profile{
		Type:      "heap",
		Data:      []byte("test data"),
		Timestamp: timestamp,
	}

	key := uploader.generateKey(pod, profile)

	// Expected format: profiles/2024-01-15/test-app/20240115-123045-heap.pprof
	expectedDate := "2024-01-15"
	expectedService := "test-app"
	expectedPrefix := "profiles"

	if !containsAll(key, expectedPrefix, expectedDate, expectedService, "heap.pprof") {
		t.Errorf("Generated key %q doesn't contain expected components", key)
	}

	// Check the exact format
	expectedKey := "profiles/2024-01-15/test-app/20240115-123045-heap.pprof"
	if key != expectedKey {
		t.Errorf("Expected key %q, got %q", expectedKey, key)
	}
}

func TestGenerateKeyDifferentDates(t *testing.T) {
	uploader := &S3Uploader{
		bucket: "test-bucket",
		prefix: "data",
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "service-abc",
			Labels: map[string]string{
				"app.kubernetes.io/name": "my-service",
			},
		},
	}

	tests := []struct {
		date     time.Time
		expected string
	}{
		{
			date:     time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
			expected: "data/2024-01-15/my-service/20240115-100000-cpu.pprof",
		},
		{
			date:     time.Date(2024, 12, 31, 23, 59, 59, 0, time.UTC),
			expected: "data/2024-12-31/my-service/20241231-235959-cpu.pprof",
		},
		{
			date:     time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC),
			expected: "data/2025-02-01/my-service/20250201-000000-cpu.pprof",
		},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			profile := profiler.Profile{
				Type:      "cpu",
				Timestamp: tt.date,
			}

			key := uploader.generateKey(pod, profile)

			if key != tt.expected {
				t.Errorf("Expected %q, got %q", tt.expected, key)
			}
		})
	}
}

// Helper function to check if string contains all substrings
func containsAll(s string, substrs ...string) bool {
	for _, substr := range substrs {
		found := false
		for i := 0; i <= len(s)-len(substr); i++ {
			if s[i:i+len(substr)] == substr {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
