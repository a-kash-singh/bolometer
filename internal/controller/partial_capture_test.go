package controller

// Regression test for keeping the profiles of a partly failed capture.
//
// Like isolation_test.go (whose test environment it uses), this only relies
// on APIs that also exist in the original controller. The capture runs end
// to end: real profiler over a port-forward to the test API server, and real
// S3 uploader against a fake S3 endpoint.

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	profilingv1alpha1 "github.com/a-kash-singh/bolometer/api/v1alpha1"
)

// When one profile type fails, the others must still be captured and
// uploaded: the types before the failure are kept, the types after it are
// still tried, and the failed type is not retried. The capture counts as a
// success, so the pod goes into cooldown, and the status counters count
// exactly the profiles captured and uploaded.
func TestPartialCapture_CPUFailsHeapAndGoroutineStillUploaded(t *testing.T) {
	t.Parallel()

	s3 := newS3Server(t)
	config := isoConfig("config", "default", "api")
	config.Spec.Thresholds.CheckIntervalSeconds = 1 // cooldown stays at 300s
	config.Spec.ProfileTypes = []string{"heap", "cpu", "goroutine"}
	config.Spec.S3Config.Endpoint = s3.srv.URL
	e := newIsoEnv(t, config)
	e.pprof.serveProfiles = true
	e.pprof.failEndpoint("/debug/pprof/profile") // the CPU profile fails
	e.addPod(isoPod("api-0", "default", "api"))  // over the threshold

	e.reconcile(config)

	const wantUploaded = 2 // heap and goroutine
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := s3.counts(); ok >= wantUploaded || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Three more checks happen in this time. The pod must be in cooldown
	// and must not be captured again.
	time.Sleep(3 * time.Second)

	s3.mu.Lock()
	var uploaded []string
	for _, p := range s3.puts {
		if p.ok {
			uploaded = append(uploaded, p.profileType)
		}
	}
	s3.mu.Unlock()
	sort.Strings(uploaded)
	if fmt.Sprint(uploaded) != "[goroutine heap]" {
		t.Errorf("profiles uploaded to S3: %v, want [goroutine heap] (the CPU profile failed)", uploaded)
	}

	wantRequests := map[string]int{"/debug/pprof/heap": 1, "/debug/pprof/profile": 1, "/debug/pprof/goroutine": 1}
	if got := e.pprof.endpointRequests("default", "api-0"); fmt.Sprint(got) != fmt.Sprint(wantRequests) {
		t.Errorf("pprof requests: %v, want each type requested once (no retry of the failed CPU profile, no recapture during cooldown): %v",
			got, wantRequests)
	}
	if n := e.pprof.captures("default", "api-0"); n != 1 {
		t.Errorf("pod captured %d times, want 1: a partly successful capture starts the cooldown", n)
	}

	var status profilingv1alpha1.ProfilingConfigStatus
	deadline = time.Now().Add(5 * time.Second)
	for {
		latest := &profilingv1alpha1.ProfilingConfig{}
		if err := e.api.Get(context.Background(), client.ObjectKeyFromObject(config), latest); err != nil {
			t.Fatal(err)
		}
		status = latest.Status
		if (status.TotalProfiles == 2 && status.TotalUploads == 2) || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status.TotalProfiles != 2 || status.TotalUploads != 2 {
		t.Errorf("status: totalProfiles=%d totalUploads=%d, want 2 and 2 (heap and goroutine; not the failed CPU profile)",
			status.TotalProfiles, status.TotalUploads)
	}
	if status.LastProfileTime == nil {
		t.Errorf("status.lastProfileTime not set")
	}
}

// If every profile type fails, nothing was captured: the capture fails, no
// cooldown starts, nothing is counted, and the pod is retried on the next
// pass. (This holds for the original controller too.)
func TestPartialCapture_AllTypesFailIsAFailedCapture(t *testing.T) {
	t.Parallel()

	s3 := newS3Server(t)
	config := isoConfig("config", "default", "api")
	config.Spec.Thresholds.CheckIntervalSeconds = 1
	config.Spec.ProfileTypes = []string{"heap", "cpu"}
	config.Spec.S3Config.Endpoint = s3.srv.URL
	e := newIsoEnv(t, config)
	e.pprof.serveProfiles = true
	e.pprof.failEndpoint("/debug/pprof/heap")
	e.pprof.failEndpoint("/debug/pprof/profile")
	e.addPod(isoPod("api-0", "default", "api"))

	e.reconcile(config)
	time.Sleep(3500 * time.Millisecond)

	if n := e.pprof.captures("default", "api-0"); n < 2 {
		t.Errorf("pod captured %d times in 3.5s, want it retried on later passes (no cooldown after a failed capture)", n)
	}
	if attempts, _ := s3.counts(); attempts != 0 {
		t.Errorf("%d uploads attempted, want none", attempts)
	}
	latest := &profilingv1alpha1.ProfilingConfig{}
	if err := e.api.Get(context.Background(), client.ObjectKeyFromObject(config), latest); err != nil {
		t.Fatal(err)
	}
	if st := latest.Status; st.TotalProfiles != 0 || st.TotalUploads != 0 || st.LastProfileTime != nil {
		t.Errorf("status: totalProfiles=%d totalUploads=%d lastProfileTime=%v, want nothing counted",
			st.TotalProfiles, st.TotalUploads, st.LastProfileTime)
	}
}

// When the connection to the pod fails during a capture (the kubelet reports
// an error such as "connection refused" on the port-forward), client-go tears
// down the whole port-forward. The remaining profile types must still be
// tried, over a fresh port-forward: over the one being torn down they fail,
// or hang until client-go's 30s stream timeout. The failed type is not
// retried.
func TestPartialCapture_LostConnectionDoesNotLoseRemainingTypes(t *testing.T) {
	t.Parallel()

	s3 := newS3Server(t)
	config := isoConfig("config", "default", "api")
	config.Spec.Thresholds.CheckIntervalSeconds = 1 // cooldown stays at 300s
	config.Spec.ProfileTypes = []string{"heap", "cpu", "goroutine"}
	config.Spec.S3Config.Endpoint = s3.srv.URL
	e := newIsoEnv(t, config)
	e.pprof.serveProfiles = true
	e.pprof.breakConnections("default", "api-0", 1) // the connection used for heap fails
	e.addPod(isoPod("api-0", "default", "api"))

	e.reconcile(config)

	deadline := time.Now().Add(10 * time.Second)
	for len(s3.successfulPuts()) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	var uploaded []string
	for _, p := range s3.successfulPuts() {
		uploaded = append(uploaded, p.profileType)
	}
	sort.Strings(uploaded)
	if fmt.Sprint(uploaded) != "[cpu goroutine]" {
		t.Fatalf("profiles uploaded: %v, want [cpu goroutine] (heap was lost with the connection); pprof requests %v",
			uploaded, e.pprof.endpointRequests("default", "api-0"))
	}
	if got := e.pprof.endpointRequests("default", "api-0"); got["/debug/pprof/heap"] != 0 {
		t.Errorf("heap was requested again over a new connection (%v); the failed type must not be retried", got)
	}
}
