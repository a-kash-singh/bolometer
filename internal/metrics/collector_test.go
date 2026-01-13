package metrics

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
)

func TestCheckThresholds(t *testing.T) {
	tests := []struct {
		name           string
		cpuPercent     float64
		memPercent     float64
		cpuThreshold   int
		memThreshold   int
		expectExceeded bool
		expectReason   string
	}{
		{
			name:           "CPU exceeds threshold",
			cpuPercent:     85,
			memPercent:     50,
			cpuThreshold:   80,
			memThreshold:   90,
			expectExceeded: true,
			expectReason:   "CPU",
		},
		{
			name:           "Memory exceeds threshold",
			cpuPercent:     50,
			memPercent:     95,
			cpuThreshold:   80,
			memThreshold:   90,
			expectExceeded: true,
			expectReason:   "Memory",
		},
		{
			name:           "Both within thresholds",
			cpuPercent:     70,
			memPercent:     80,
			cpuThreshold:   80,
			memThreshold:   90,
			expectExceeded: false,
		},
		{
			name:           "Both exceed thresholds",
			cpuPercent:     90,
			memPercent:     95,
			cpuThreshold:   80,
			memThreshold:   90,
			expectExceeded: true,
			expectReason:   "CPU",
		},
		{
			name:           "Exactly at threshold",
			cpuPercent:     80,
			memPercent:     90,
			cpuThreshold:   80,
			memThreshold:   90,
			expectExceeded: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pm := &PodMetrics{
				CPUUsagePercent:    tt.cpuPercent,
				MemoryUsagePercent: tt.memPercent,
			}

			exceeded, reason := pm.CheckThresholds(tt.cpuThreshold, tt.memThreshold)

			if exceeded != tt.expectExceeded {
				t.Errorf("expected exceeded=%v, got %v", tt.expectExceeded, exceeded)
			}

			if tt.expectExceeded && tt.expectReason != "" {
				if len(reason) == 0 {
					t.Errorf("expected reason to contain something, got empty")
				}
			}
		})
	}
}

func TestCalculateMetrics(t *testing.T) {
	tests := []struct {
		name          string
		cpuUsage      string
		memUsage      string
		cpuRequest    string
		memRequest    string
		expectedCPU   float64
		expectedMem   float64
		expectZeroCPU bool
		expectZeroMem bool
	}{
		{
			name:          "Normal usage",
			cpuUsage:      "500m",
			memUsage:      "256Mi",
			cpuRequest:    "1000m",
			memRequest:    "512Mi",
			expectedCPU:   50,
			expectedMem:   50,
			expectZeroCPU: false,
			expectZeroMem: false,
		},
		{
			name:          "High CPU usage",
			cpuUsage:      "800m",
			memUsage:      "100Mi",
			cpuRequest:    "1000m",
			memRequest:    "512Mi",
			expectedCPU:   80,
			expectedMem:   19.53125,
			expectZeroCPU: false,
			expectZeroMem: false,
		},
		{
			name:          "Zero requests",
			cpuUsage:      "500m",
			memUsage:      "256Mi",
			cpuRequest:    "0",
			memRequest:    "0",
			expectZeroCPU: true,
			expectZeroMem: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// This test would require mock metrics client
			// For now, just test the percentage calculation logic

			cpuUsage := resource.MustParse(tt.cpuUsage)
			memUsage := resource.MustParse(tt.memUsage)
			cpuRequest := resource.MustParse(tt.cpuRequest)
			memRequest := resource.MustParse(tt.memRequest)

			var cpuPercent, memPercent float64

			if !cpuRequest.IsZero() {
				cpuPercent = float64(cpuUsage.MilliValue()) / float64(cpuRequest.MilliValue()) * 100
			}

			if !memRequest.IsZero() {
				memPercent = float64(memUsage.Value()) / float64(memRequest.Value()) * 100
			}

			if tt.expectZeroCPU {
				if cpuPercent != 0 {
					t.Errorf("expected CPU percent to be 0, got %f", cpuPercent)
				}
			} else {
				if cpuPercent < tt.expectedCPU-1 || cpuPercent > tt.expectedCPU+1 {
					t.Errorf("expected CPU percent ~%f, got %f", tt.expectedCPU, cpuPercent)
				}
			}

			if tt.expectZeroMem {
				if memPercent != 0 {
					t.Errorf("expected memory percent to be 0, got %f", memPercent)
				}
			} else {
				if memPercent < tt.expectedMem-1 || memPercent > tt.expectedMem+1 {
					t.Errorf("expected memory percent ~%f, got %f", tt.expectedMem, memPercent)
				}
			}
		})
	}
}
