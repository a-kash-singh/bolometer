package uploader

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	corev1 "k8s.io/api/core/v1"

	"github.com/a-kash-singh/bolometer/internal/profiler"
)

// S3Uploader uploads profiles to S3
type S3Uploader struct {
	client *s3.Client
	bucket string
	prefix string
}

// S3Config holds S3 configuration
type S3Config struct {
	Bucket   string
	Prefix   string
	Region   string
	Endpoint string
}

// NewS3Uploader creates a new S3 uploader
func NewS3Uploader(ctx context.Context, cfg S3Config) (*S3Uploader, error) {
	// Load AWS config from environment (uses IRSA/IAM roles automatically)
	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Create S3 client
	var client *s3.Client
	if cfg.Endpoint != "" {
		// Custom endpoint for S3-compatible services
		client = s3.NewFromConfig(awsCfg, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
			o.UsePathStyle = true
		})
	} else {
		client = s3.NewFromConfig(awsCfg)
	}

	return &S3Uploader{
		client: client,
		bucket: cfg.Bucket,
		prefix: cfg.Prefix,
	}, nil
}

// UploadProfile uploads a single profile to S3
func (u *S3Uploader) UploadProfile(ctx context.Context, pod *corev1.Pod, profile profiler.Profile, reason string) error {
	key := u.generateKey(pod, profile)

	// Prepare metadata
	metadata := map[string]string{
		"pod-name":      pod.Name,
		"pod-namespace": pod.Namespace,
		"profile-type":  profile.Type,
		"reason":        reason,
		"timestamp":     profile.Timestamp.Format(time.RFC3339),
	}

	// Add pod labels as metadata
	for k, v := range pod.Labels {
		// S3 metadata keys must be lowercase and cannot contain special chars
		safeKey := fmt.Sprintf("pod-label-%s", k)
		metadata[safeKey] = v
	}

	// Upload to S3
	_, err := u.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(u.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(profile.Data),
		ContentType: aws.String("application/octet-stream"),
		Metadata:    metadata,
	})

	if err != nil {
		return fmt.Errorf("failed to upload to S3: %w", err)
	}

	return nil
}

// UploadProfiles uploads multiple profiles to S3
func (u *S3Uploader) UploadProfiles(ctx context.Context, pod *corev1.Pod, profiles []profiler.Profile, reason string) error {
	for _, profile := range profiles {
		if err := u.UploadProfile(ctx, pod, profile, reason); err != nil {
			return err
		}
	}
	return nil
}

// generateKey generates the S3 key for a profile
func (u *S3Uploader) generateKey(pod *corev1.Pod, profile profiler.Profile) string {
	// Format: {prefix}/{date}/{service-name}/{timestamp}-{profile-type}.pprof
	// Date format: YYYY-MM-DD
	date := profile.Timestamp.Format("2006-01-02")

	// Extract service name from pod labels (app, app.kubernetes.io/name, or fallback to pod name prefix)
	serviceName := u.getServiceName(pod)

	// Timestamp for uniqueness
	timestamp := profile.Timestamp.Format("20060102-150405")
	filename := fmt.Sprintf("%s-%s.pprof", timestamp, profile.Type)

	parts := []string{
		u.prefix,
		date,
		serviceName,
		filename,
	}

	return filepath.Join(parts...)
}

// getServiceName extracts the service name from pod labels or metadata
func (u *S3Uploader) getServiceName(pod *corev1.Pod) string {
	// Try common label keys for service name
	if pod.Labels != nil {
		// Check app.kubernetes.io/name (recommended label)
		if name, ok := pod.Labels["app.kubernetes.io/name"]; ok && name != "" {
			return name
		}

		// Check app label (common convention)
		if app, ok := pod.Labels["app"]; ok && app != "" {
			return app
		}

		// Check k8s-app label
		if app, ok := pod.Labels["k8s-app"]; ok && app != "" {
			return app
		}
	}

	// Fallback: extract from owner reference (deployment, statefulset, etc.)
	if len(pod.OwnerReferences) > 0 {
		owner := pod.OwnerReferences[0]
		if owner.Kind == "ReplicaSet" {
			// For ReplicaSets owned by Deployments, strip the hash suffix
			// e.g., "myapp-7d8f9c5b6d" -> "myapp"
			name := owner.Name
			lastDash := len(name) - 1
			for i := len(name) - 1; i >= 0; i-- {
				if name[i] == '-' {
					lastDash = i
					break
				}
			}
			if lastDash > 0 {
				return name[:lastDash]
			}
		}
		return owner.Name
	}

	// Last resort: use pod name without hash
	name := pod.Name
	lastDash := -1
	dashCount := 0
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '-' {
			dashCount++
			if dashCount == 2 {
				lastDash = i
				break
			}
		}
	}
	if lastDash > 0 {
		return name[:lastDash]
	}

	return name
}
