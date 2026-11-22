package uploader

// Regression test that only uses APIs that also exist in the original
// uploader, so it can be copied onto the original code.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/a-kash-singh/bolometer/internal/profiler"
)

// Pod labels are sent as S3 metadata headers. Label keys like
// "app.kubernetes.io/name" contain '/', which is not allowed in a header
// name; uploads from such pods used to fail before reaching S3.
func TestUploadProfile_PodWithSlashInLabelKey(t *testing.T) {
	var gotMeta http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMeta = r.Header.Clone()
		w.Header().Set("ETag", `"test"`)
	}))
	defer srv.Close()

	none := filepath.Join(t.TempDir(), "none")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", none)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", none)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	ctx := context.Background()
	u, err := NewS3Uploader(ctx, S3Config{Bucket: "bucket", Region: "us-west-2", Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "web-0",
		Namespace: "default",
		Labels: map[string]string{
			"app.kubernetes.io/name":     "web",
			"app.kubernetes.io/instance": "web-prod",
			"tier":                       "frontend",
		},
	}}
	profile := profiler.Profile{Type: "heap", Data: []byte("data"), Timestamp: time.Now()}

	if err := u.UploadProfile(ctx, pod, profile, "test"); err != nil {
		t.Fatalf("upload from a pod with app.kubernetes.io/* labels failed: %v", err)
	}
	for header, want := range map[string]string{
		"X-Amz-Meta-Pod-Label-App.kubernetes.io_name":     "web",
		"X-Amz-Meta-Pod-Label-App.kubernetes.io_instance": "web-prod",
		"X-Amz-Meta-Pod-Label-Tier":                       "frontend",
	} {
		if got := gotMeta.Get(header); got != want {
			t.Errorf("metadata header %s = %q, want %q", header, got, want)
		}
	}
}
