package uploader

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
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

// ForDestination returns an uploader that uploads to bucket and prefix
// through the same S3 client (and thus the same credentials and
// connections). A client is bound to a region and endpoint only, so a bucket
// or prefix change does not need a new one.
func (u *S3Uploader) ForDestination(bucket, prefix string) *S3Uploader {
	c := *u
	c.bucket = bucket
	c.prefix = prefix
	return &c
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
		metadata[metadataKey("pod-label-"+k)] = v
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

// metadataKey makes s usable as an S3 user-metadata key. Metadata is sent as
// x-amz-meta-* HTTP headers, so the key must be a valid header field name;
// label keys such as "app.kubernetes.io/name" are not (the '/'), and the SDK
// refuses to send the request. Invalid characters are replaced with '_'.
func metadataKey(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case strings.ContainsRune("!#$%&'*+-.^_`|~", r):
			return r
		default:
			return '_'
		}
	}, s)
}

// UploadProfiles uploads multiple profiles to S3. It attempts every profile
// even if some uploads fail, and returns how many were uploaded together with
// the errors of the failed ones.
func (u *S3Uploader) UploadProfiles(ctx context.Context, pod *corev1.Pod, profiles []profiler.Profile, reason string) (uploaded int, err error) {
	var errs []error
	for _, profile := range profiles {
		if err := u.UploadProfile(ctx, pod, profile, reason); err != nil {
			errs = append(errs, fmt.Errorf("%s profile: %w", profile.Type, err))
			continue
		}
		uploaded++
	}
	return uploaded, errors.Join(errs...)
}

// generateKey generates the S3 key for a profile
func (u *S3Uploader) generateKey(pod *corev1.Pod, profile profiler.Profile) string {
	// Format: {prefix}/{date}/{service-name}/{timestamp}-{pod-name}-{profile-type}.pprof
	// Date format: YYYY-MM-DD
	date := profile.Timestamp.Format("2006-01-02")

	// Extract service name from pod labels (app, app.kubernetes.io/name, or fallback to pod name prefix)
	serviceName := u.getServiceName(pod)

	// The timestamp (fixed width, first, so files sort by time) alone is not
	// unique: replicas of a service are often captured at the same moment.
	// The pod name tells them apart. Pod names contain '-' but profile types
	// do not, so the type is whatever follows the last '-'.
	timestamp := profile.Timestamp.Format("20060102-150405")
	filename := fmt.Sprintf("%s-%s-%s.pprof", timestamp, pod.Name, profile.Type)

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
