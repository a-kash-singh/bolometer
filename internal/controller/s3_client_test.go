package controller

// Regression tests for the S3 client a ProfilingConfig uploads through.
//
// Like isolation_test.go (whose test environment they use), these only rely
// on APIs that also exist in the original controller. Captures run end to
// end: real profiler over a port-forward to the test API server, and real S3
// uploader against fake S3 endpoints.

import (
	"context"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	profilingv1alpha1 "github.com/a-kash-singh/bolometer/api/v1alpha1"
)

// waitForPuts waits until s3 has accepted at least n uploads.
func waitForPuts(t *testing.T, s3 *s3Server, n int, timeout time.Duration) []s3Put {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for len(s3.successfulPuts()) < n && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	puts := s3.successfulPuts()
	if len(puts) < n {
		t.Fatalf("S3 at %s accepted %d uploads, want at least %d", s3.srv.URL, len(puts), n)
	}
	return puts
}

// A config's captures must share one S3 client instead of building a new one
// (reloading AWS configuration and credentials, opening a new connection)
// for every capture. Observed here as the connections opened to S3: a
// reused client keeps its connection alive between uploads.
func TestS3Client_CapturesOfAConfigShareOneClient(t *testing.T) {
	t.Parallel()

	s3 := newS3Server(t)
	config := isoConfig("config", "default", "api")
	config.Spec.OnDemand = &profilingv1alpha1.OnDemandConfig{Enabled: true, IntervalSeconds: 1}
	config.Spec.ProfileTypes = []string{"heap"}
	config.Spec.S3Config.Endpoint = s3.srv.URL
	e := newIsoEnv(t, config)
	e.pprof.serveProfiles = true
	e.addPod(isoPod("api-0", "default", "api"))

	e.reconcile(config)

	const captures = 6
	puts := waitForPuts(t, s3, captures, 20*time.Second)
	if conns := s3.connections(); conns > 2 {
		t.Errorf("%d captures opened %d connections to S3; want the config's S3 client (and its connection) reused",
			len(puts), conns)
	}
}

// Edits to the S3 settings must reach the uploads that follow: bucket and
// prefix, region (used to sign requests) and endpoint. This holds for the
// original controller too (it rebuilt everything on every reconcile).
func TestS3Client_EditsToS3SettingsReachUploads(t *testing.T) {
	t.Parallel()

	first, second := newS3Server(t), newS3Server(t)
	config := isoConfig("config", "default", "api")
	config.Spec.OnDemand = &profilingv1alpha1.OnDemandConfig{Enabled: true, IntervalSeconds: 1}
	config.Spec.ProfileTypes = []string{"heap"}
	config.Spec.S3Config = profilingv1alpha1.S3Configuration{
		Bucket: "bucket-a", Prefix: "before", Region: "us-west-2", Endpoint: first.srv.URL,
	}
	e := newIsoEnv(t, config)
	e.pprof.serveProfiles = true
	e.addPod(isoPod("api-0", "default", "api"))
	key := client.ObjectKeyFromObject(config)

	edit := func(mutate func(*profilingv1alpha1.S3Configuration)) {
		t.Helper()
		e.editSpec(key, func(s *profilingv1alpha1.ProfilingConfigSpec) { mutate(&s.S3Config) })
		e.reconcile(config)
	}
	// nextPut waits for an upload to s3 after the first `after` ones that
	// matches want, skipping uploads of captures already in flight.
	nextPut := func(s3 *s3Server, after int, what string, want func(s3Put) bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			puts := s3.successfulPuts()
			for _, p := range puts[min(after, len(puts)):] {
				if want(p) {
					return
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		first.mu.Lock()
		firstPuts := append([]s3Put(nil), first.puts...)
		first.mu.Unlock()
		second.mu.Lock()
		secondPuts := append([]s3Put(nil), second.puts...)
		second.mu.Unlock()
		stored := &profilingv1alpha1.ProfilingConfig{}
		_ = e.api.Get(context.Background(), key, stored)
		t.Fatalf("no upload with %s after the edit; stored S3 settings %+v; port-forwards %v, pprof requests %v, uploads to first %d, to second %d",
			what, stored.Spec.S3Config, e.pprof.all(), e.pprof.endpointRequests("default", "api-0"), len(firstPuts), len(secondPuts))
	}

	e.reconcile(config)
	puts := waitForPuts(t, first, 1, 10*time.Second)
	if p := puts[0]; !strings.HasPrefix(p.key, "bucket-a/before/") || p.region != "us-west-2" {
		t.Fatalf("first upload: key %s region %s, want bucket-a/before/... signed for us-west-2", p.key, p.region)
	}

	n := len(first.successfulPuts())
	edit(func(s *profilingv1alpha1.S3Configuration) { s.Bucket, s.Prefix = "bucket-b", "after" })
	nextPut(first, n, "the edited bucket and prefix", func(p s3Put) bool { return strings.HasPrefix(p.key, "bucket-b/after/") })

	n = len(first.successfulPuts())
	edit(func(s *profilingv1alpha1.S3Configuration) { s.Region = "eu-central-1" })
	nextPut(first, n, "the edited region", func(p s3Put) bool { return p.region == "eu-central-1" })

	edit(func(s *profilingv1alpha1.S3Configuration) { s.Endpoint = second.srv.URL })
	nextPut(second, 0, "the edited endpoint", func(p s3Put) bool {
		return strings.HasPrefix(p.key, "bucket-b/after/") && p.region == "eu-central-1"
	})
}
