package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProfilingConfigSpec defines the desired state of ProfilingConfig
type ProfilingConfigSpec struct {
	// Selector for target pods
	Selector PodSelector `json:"selector"`

	// Threshold configuration for abnormality detection
	Thresholds ThresholdConfig `json:"thresholds"`

	// On-demand profiling configuration
	// +optional
	OnDemand *OnDemandConfig `json:"onDemand,omitempty"`

	// S3 configuration for profile uploads
	S3Config S3Configuration `json:"s3Config"`

	// ProfileTypes specifies which profile types to capture
	// Valid values: heap, cpu, goroutine, mutex
	// +kubebuilder:default={"heap","cpu","goroutine","mutex"}
	ProfileTypes []string `json:"profileTypes,omitempty"`
}

// PodSelector defines how to select target pods for profiling
type PodSelector struct {
	// Namespace to watch for pods. If empty, watches all namespaces
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// LabelSelector to filter pods
	// +optional
	LabelSelector map[string]string `json:"labelSelector,omitempty"`
}

// ThresholdConfig defines resource thresholds for triggering profiling
type ThresholdConfig struct {
	// CPUThresholdPercent is the CPU usage percentage threshold (0-100)
	// +kubebuilder:default=80
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	CPUThresholdPercent int `json:"cpuThresholdPercent,omitempty"`

	// MemoryThresholdPercent is the memory usage percentage threshold (0-100)
	// +kubebuilder:default=90
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	MemoryThresholdPercent int `json:"memoryThresholdPercent,omitempty"`

	// CheckIntervalSeconds is how often to check metrics
	// +kubebuilder:default=30
	// +kubebuilder:validation:Minimum=10
	CheckIntervalSeconds int `json:"checkIntervalSeconds,omitempty"`

	// CooldownSeconds is the cooldown period after capturing a profile
	// to avoid capturing too frequently
	// +kubebuilder:default=300
	// +kubebuilder:validation:Minimum=60
	CooldownSeconds int `json:"cooldownSeconds,omitempty"`
}

// OnDemandConfig defines on-demand continuous profiling settings
type OnDemandConfig struct {
	// Enabled indicates whether on-demand profiling is enabled
	Enabled bool `json:"enabled"`

	// IntervalSeconds is how often to capture profiles in on-demand mode
	// +kubebuilder:default=35
	// +kubebuilder:validation:Minimum=30
	// +kubebuilder:validation:Maximum=60
	IntervalSeconds int `json:"intervalSeconds,omitempty"`
}

// S3Configuration defines S3 upload settings
type S3Configuration struct {
	// Bucket is the S3 bucket name
	Bucket string `json:"bucket"`

	// Prefix is the S3 key prefix for uploaded profiles
	// +optional
	Prefix string `json:"prefix,omitempty"`

	// Region is the AWS region
	Region string `json:"region"`

	// Endpoint is a custom S3 endpoint (for S3-compatible services)
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
}

// ProfilingConfigStatus defines the observed state of ProfilingConfig
type ProfilingConfigStatus struct {
	// ActivePods is the number of pods currently being monitored
	ActivePods int `json:"activePods"`

	// LastProfileTime is the timestamp of the last profile capture
	// +optional
	LastProfileTime *metav1.Time `json:"lastProfileTime,omitempty"`

	// TotalProfiles is the total number of profiles captured
	TotalProfiles int64 `json:"totalProfiles"`

	// TotalUploads is the total number of successful uploads to S3
	TotalUploads int64 `json:"totalUploads"`

	// Conditions represent the latest available observations of the ProfilingConfig's state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=pc
// +kubebuilder:printcolumn:name="Active Pods",type=integer,JSONPath=`.status.activePods`
// +kubebuilder:printcolumn:name="Total Profiles",type=integer,JSONPath=`.status.totalProfiles`
// +kubebuilder:printcolumn:name="Total Uploads",type=integer,JSONPath=`.status.totalUploads`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ProfilingConfig is the Schema for the profilingconfigs API
type ProfilingConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProfilingConfigSpec   `json:"spec,omitempty"`
	Status ProfilingConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ProfilingConfigList contains a list of ProfilingConfig
type ProfilingConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ProfilingConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ProfilingConfig{}, &ProfilingConfigList{})
}
