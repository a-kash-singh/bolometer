package controller

// Regression tests for the per-config limit on concurrent captures.
//
// Like isolation_test.go (whose test environment they use), these only rely
// on APIs that also exist in the original controller, so they can be copied
// onto the original code together with isolation_test.go.

import (
	"fmt"
	"testing"
	"time"

	profilingv1alpha1 "github.com/a-kash-singh/bolometer/api/v1alpha1"
)

const captureLimitPerConfig = 4

// addStalledPods adds n pods over the CPU threshold whose port-forwards hang
// for stall and then fail, and returns their names.
func addStalledPods(e *isoEnv, namespace, prefix string, n int, stall time.Duration) []string {
	var names []string
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("%s-%d", prefix, i)
		e.addPod(isoPod(name, namespace, "api"))
		e.pprof.stall(namespace, name, stall)
		names = append(names, name)
	}
	return names
}

// waitAllAttempted waits until every pod has had a capture attempt, and
// reports the pods that never did.
func waitAllAttempted(e *isoEnv, namespace string, pods []string, timeout time.Duration) (missing []string) {
	deadline := time.Now().Add(timeout)
	for {
		missing = missing[:0]
		for _, name := range pods {
			if e.pprof.captures(namespace, name) == 0 {
				missing = append(missing, name)
			}
		}
		if len(missing) == 0 || time.Now().After(deadline) {
			return missing
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// With more pods over the threshold than the limit, exactly the limit is
// captured at once, and the rest are captured on later passes.
func TestCaptureLimit_ThresholdCapturesAreCappedPerConfig(t *testing.T) {
	t.Parallel()
	config := isoConfig("config", "default", "api")
	config.Spec.Thresholds.CheckIntervalSeconds = 1
	e := newIsoEnv(t, config)
	pods := addStalledPods(e, "default", "api", 10, 1200*time.Millisecond)

	e.reconcile(config)

	if missing := waitAllAttempted(e, "default", pods, 15*time.Second); len(missing) > 0 {
		t.Errorf("pods never captured: %v (attempts: %v)", missing, e.pprof.all())
	}
	if got := e.pprof.maxConcurrentStalled("default"); got != captureLimitPerConfig {
		t.Errorf("max concurrent captures = %d, want exactly the limit of %d", got, captureLimitPerConfig)
	}
}

// The limit applies to each config separately.
func TestCaptureLimit_AppliesToEachConfigSeparately(t *testing.T) {
	t.Parallel()
	configA := isoConfig("config-a", "team-a", "api")
	configA.Spec.Thresholds.CheckIntervalSeconds = 1
	configB := isoConfig("config-b", "team-b", "api")
	configB.Spec.Thresholds.CheckIntervalSeconds = 1
	e := newIsoEnv(t, configA, configB)
	podsA := addStalledPods(e, "team-a", "a", 6, 1500*time.Millisecond)
	podsB := addStalledPods(e, "team-b", "b", 6, 1500*time.Millisecond)

	e.reconcile(configA)
	e.reconcile(configB)

	missingA := waitAllAttempted(e, "team-a", podsA, 15*time.Second)
	missingB := waitAllAttempted(e, "team-b", podsB, time.Second)
	if len(missingA)+len(missingB) > 0 {
		t.Errorf("pods never captured: %v %v (attempts: %v)", missingA, missingB, e.pprof.all())
	}
	for _, ns := range []string{"team-a", "team-b"} {
		if got := e.pprof.maxConcurrentStalled(ns); got != captureLimitPerConfig {
			t.Errorf("%s: max concurrent captures = %d, want exactly %d", ns, got, captureLimitPerConfig)
		}
	}
	if got := e.pprof.maxConcurrentStalled(""); got != 2*captureLimitPerConfig {
		t.Errorf("max concurrent captures across both configs = %d, want %d", got, 2*captureLimitPerConfig)
	}
}

// Threshold and on-demand captures of a config share its limit.
func TestCaptureLimit_SharedByThresholdAndOnDemand(t *testing.T) {
	t.Parallel()
	config := isoConfig("config", "default", "api")
	config.Spec.Thresholds.CheckIntervalSeconds = 1
	config.Spec.OnDemand = &profilingv1alpha1.OnDemandConfig{Enabled: true, IntervalSeconds: 1}
	e := newIsoEnv(t, config)
	pods := addStalledPods(e, "default", "api", 10, 1200*time.Millisecond)

	e.reconcile(config)

	if missing := waitAllAttempted(e, "default", pods, 15*time.Second); len(missing) > 0 {
		t.Errorf("pods never captured: %v (attempts: %v)", missing, e.pprof.all())
	}
	if got := e.pprof.maxConcurrentStalled("default"); got != captureLimitPerConfig {
		t.Errorf("max concurrent captures = %d, want exactly the limit of %d", got, captureLimitPerConfig)
	}
}
