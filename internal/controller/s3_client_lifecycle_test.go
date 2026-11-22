package controller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	profilingv1alpha1 "github.com/a-kash-singh/bolometer/api/v1alpha1"
	"github.com/a-kash-singh/bolometer/internal/uploader"
)

// countBuilds makes r count (and record the settings of) the S3 clients it
// builds, delegating to build.
type buildCounter struct {
	mu     sync.Mutex
	builds []uploader.S3Config
	fail   error
}

func (c *buildCounter) wrap(build func(context.Context, uploader.S3Config) (*uploader.S3Uploader, error)) func(context.Context, uploader.S3Config) (*uploader.S3Uploader, error) {
	return func(ctx context.Context, cfg uploader.S3Config) (*uploader.S3Uploader, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.fail != nil {
			return nil, c.fail
		}
		c.builds = append(c.builds, cfg)
		return build(ctx, cfg)
	}
}

func (c *buildCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.builds)
}

// The config's client is reused while the region and endpoint stay the same
// (bucket and prefix are per upload), rebuilt when either changes, and a
// failed build is retried on the next capture.
func TestS3Client_RebuiltOnlyWhenRegionOrEndpointChanges(t *testing.T) {
	r := setupTestReconciler()
	counter := &buildCounter{}
	r.newUploader = counter.wrap(func(context.Context, uploader.S3Config) (*uploader.S3Uploader, error) {
		return &uploader.S3Uploader{}, nil
	})
	m := &configMonitor{}
	ctx := context.Background()
	use := func(bucket, prefix, region, endpoint string) error {
		_, err := r.uploaderFor(ctx, m, profilingv1alpha1.S3Configuration{Bucket: bucket, Prefix: prefix, Region: region, Endpoint: endpoint})
		return err
	}
	expect := func(step string, want int) {
		t.Helper()
		if got := counter.count(); got != want {
			t.Fatalf("%s: %d clients built, want %d", step, got, want)
		}
	}

	for i := 0; i < 3; i++ {
		if err := use("b", "p", "us-west-2", ""); err != nil {
			t.Fatal(err)
		}
	}
	expect("three captures", 1)

	_ = use("other-bucket", "other-prefix", "us-west-2", "")
	expect("bucket and prefix changed", 1)

	_ = use("b", "p", "eu-central-1", "")
	expect("region changed", 2)

	_ = use("b", "p", "eu-central-1", "http://minio:9000")
	_ = use("b", "p", "eu-central-1", "http://minio:9000")
	expect("endpoint changed", 3)

	counter.fail = errors.New("no credentials")
	if err := use("b", "p", "us-east-1", ""); err == nil {
		t.Fatal("expected the build error to be returned")
	}
	counter.fail = nil
	if err := use("b", "p", "us-east-1", ""); err != nil {
		t.Fatal(err)
	}
	expect("failed build retried", 4)
}

// Each config gets its own client. An invalid edit drops the config's client
// (with its monitor); once fixed, the config builds a new one. So does a
// config that is deleted and created again.
func TestS3Client_OnePerConfigAndDroppedWithIt(t *testing.T) {
	t.Parallel()

	s3 := newS3Server(t)
	newConfig := func(name, namespace string) *profilingv1alpha1.ProfilingConfig {
		c := isoConfig(name, namespace, "api")
		c.Spec.OnDemand = &profilingv1alpha1.OnDemandConfig{Enabled: true, IntervalSeconds: 1}
		c.Spec.ProfileTypes = []string{"heap"}
		c.Spec.S3Config.Endpoint = s3.srv.URL
		return c
	}
	configA, configB := newConfig("config-a", "team-a"), newConfig("config-b", "team-b")
	e := newIsoEnv(t, configA, configB)
	e.pprof.serveProfiles = true
	counter := &buildCounter{}
	e.r.newUploader = counter.wrap(uploader.NewS3Uploader)
	e.addPod(isoPod("a-0", "team-a", "api"))
	e.addPod(isoPod("b-0", "team-b", "api"))

	uploadsOf := func(pod string) int {
		n := 0
		for _, p := range s3.successfulPuts() {
			if p.pod == pod {
				n++
			}
		}
		return n
	}
	waitForUploads := func(pod string, n int) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for uploadsOf(pod) < n && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if got := uploadsOf(pod); got < n {
			t.Fatalf("%s: %d uploads, want at least %d", pod, got, n)
		}
	}
	expectBuilds := func(step string, want int) {
		t.Helper()
		if got := counter.count(); got != want {
			t.Fatalf("%s: %d S3 clients built, want %d", step, got, want)
		}
	}
	editA := func(mutate func(*profilingv1alpha1.S3Configuration)) {
		t.Helper()
		e.editSpec(client.ObjectKeyFromObject(configA), func(s *profilingv1alpha1.ProfilingConfigSpec) { mutate(&s.S3Config) })
		e.reconcile(configA)
	}

	e.reconcile(configA)
	e.reconcile(configB)
	waitForUploads("a-0", 3)
	waitForUploads("b-0", 3)
	expectBuilds("two configs, several captures each", 2)

	editA(func(s *profilingv1alpha1.S3Configuration) { s.Bucket = "" }) // invalid
	editA(func(s *profilingv1alpha1.S3Configuration) { s.Bucket = "test-bucket" })
	waitForUploads("a-0", uploadsOf("a-0")+2)
	expectBuilds("config-a invalid, then fixed", 3)

	if err := e.api.Delete(context.Background(), configB); err != nil {
		t.Fatal(err)
	}
	e.reconcile(configB)
	recreated := newConfig("config-b", "team-b")
	if err := e.api.Create(context.Background(), recreated); err != nil {
		t.Fatal(err)
	}
	e.reconcile(recreated)
	waitForUploads("b-0", uploadsOf("b-0")+2)
	expectBuilds("config-b deleted and created again", 4)
}
