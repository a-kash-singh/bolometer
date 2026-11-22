package controller

// Regression test for the ProfilingConfig status counters.
//
// Like isolation_test.go (whose test environment it uses), this only relies
// on APIs that also exist in the original controller. Captures run end to
// end: real profiler over a port-forward to the test API server, and real S3
// uploader against a fake S3 endpoint.

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	profilingv1alpha1 "github.com/a-kash-singh/bolometer/api/v1alpha1"
)

// Status counters must match what actually happened: totalProfiles counts
// the profiles captured, totalUploads the profiles S3 accepted. No increment
// may be lost when several captures finish together and the controller reads
// through a lagging cache (conflicting writes must be retried, not dropped).
func TestStatusCounters_MatchCapturedProfilesAndSuccessfulUploads(t *testing.T) {
	t.Parallel()

	s3 := newS3Server(t)
	// The first upload of c-0's CPU profile is rejected; its retry on the
	// next pass succeeds.
	s3.reject = func(pod, profileType string, attempt int) bool {
		return pod == "c-0" && profileType == "cpu" && attempt == 0
	}

	config := isoConfig("config", "default", "api")
	config.Spec.Thresholds.CheckIntervalSeconds = 1 // cooldown stays at 300s
	config.Spec.ProfileTypes = []string{"heap", "cpu"}
	config.Spec.S3Config.Endpoint = s3.srv.URL
	e := newIsoEnvWith(t, isoOptions{cacheLag: 200 * time.Millisecond}, config)
	e.pprof.serveProfiles = true

	for _, name := range []string{"a-0", "b-0", "c-0"} {
		e.addPod(isoPod(name, "default", "api")) // over the CPU threshold
	}

	e.reconcile(config)

	// Pass 1 captures all three pods at about the same time (2 profiles
	// each); c-0's CPU upload fails, so c-0 is not put in cooldown and is
	// captured again on a later pass, which succeeds. In total 8 profiles
	// are captured and 7 of them are uploaded.
	const wantProfiles, wantUploads = 8, 7
	waitUntil := func(timeout time.Duration, cond func() bool) bool {
		deadline := time.Now().Add(timeout)
		for !cond() {
			if time.Now().After(deadline) {
				return false
			}
			time.Sleep(20 * time.Millisecond)
		}
		return true
	}
	if !waitUntil(15*time.Second, func() bool { _, ok := s3.counts(); return ok >= wantUploads }) {
		attempts, ok := s3.counts()
		t.Fatalf("S3 received %d PutObject requests, %d succeeded; want %d successful", attempts, ok, wantUploads)
	}

	status := func() profilingv1alpha1.ProfilingConfigStatus {
		latest := &profilingv1alpha1.ProfilingConfig{}
		if err := e.api.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "config"}, latest); err != nil {
			t.Fatalf("get config: %v", err)
		}
		return latest.Status
	}
	waitUntil(5*time.Second, func() bool {
		st := status()
		return st.TotalProfiles == wantProfiles && st.TotalUploads == wantUploads
	})
	time.Sleep(1500 * time.Millisecond) // nothing else may be counted

	attempts, uploaded := s3.counts()
	st := status()
	if attempts != wantProfiles || uploaded != wantUploads {
		t.Errorf("S3 saw %d uploads, %d successful; want %d and %d", attempts, uploaded, wantProfiles, wantUploads)
	}
	if st.TotalProfiles != wantProfiles {
		t.Errorf("status.totalProfiles = %d, want %d (profiles captured)", st.TotalProfiles, wantProfiles)
	}
	if st.TotalUploads != int64(uploaded) {
		t.Errorf("status.totalUploads = %d, want %d (profiles S3 accepted)", st.TotalUploads, uploaded)
	}
	if st.LastProfileTime == nil {
		t.Errorf("status.lastProfileTime not set")
	}
}
