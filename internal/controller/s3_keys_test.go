package controller

// Regression test for S3 object keys of replicas captured together.
//
// Like isolation_test.go (whose test environment it uses), this only relies
// on APIs that also exist in the original controller.

import (
	"strings"
	"testing"
	"time"
)

// Replicas of one service crossing the threshold together are captured at the
// same moment. Each profile must land in its own S3 object; a shared key
// means one replica's upload silently replaced another's.
func TestS3Keys_ReplicasCapturedTogetherDoNotOverwriteEachOther(t *testing.T) {
	t.Parallel()

	s3 := newS3Server(t)
	config := isoConfig("config", "default", "web")
	config.Spec.Thresholds.CheckIntervalSeconds = 1
	config.Spec.ProfileTypes = []string{"heap", "cpu"}
	config.Spec.S3Config.Endpoint = s3.srv.URL
	e := newIsoEnv(t, config)
	e.pprof.serveProfiles = true

	replicas := []string{"web-7d8f9c5b6d-abcde", "web-7d8f9c5b6d-fghij", "web-7d8f9c5b6d-klmno"}
	for _, name := range replicas {
		e.addPod(isoPod(name, "default", "web")) // same service, over the threshold
	}

	e.reconcile(config)

	want := len(replicas) * len(config.Spec.ProfileTypes)
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, ok := s3.counts(); ok >= want || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	s3.mu.Lock()
	puts := append([]s3Put(nil), s3.puts...)
	s3.mu.Unlock()
	if len(puts) < want {
		t.Fatalf("S3 received %d uploads, want %d", len(puts), want)
	}
	owner := map[string]string{} // key -> pod that uploaded it
	for _, p := range puts {
		if other, dup := owner[p.key]; dup && other != p.pod {
			t.Errorf("pods %s and %s uploaded to the same key %s; one profile was overwritten", other, p.pod, p.key)
		}
		owner[p.key] = p.pod
		if !strings.Contains(p.key, p.pod) {
			t.Errorf("key %s does not identify pod %s", p.key, p.pod)
		}
	}
}
