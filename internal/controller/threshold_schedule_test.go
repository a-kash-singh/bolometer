package controller

// Regression test for threshold checks being held up by captures.
//
// Like isolation_test.go (whose test environment it uses), this only relies
// on APIs that also exist in the original controller, so the two files can
// be copied onto the original code to see the test fail there.

import (
	"strings"
	"testing"
	"time"
)

// A pod whose port-forward hangs must not hold up the threshold checks of
// the other pods in the config: they stay on their schedule, and the
// unresponsive pod is retried on a later pass once its attempt has failed.
func TestThreshold_UnresponsivePodsDoNotDelayOtherPodsChecks(t *testing.T) {
	t.Parallel()
	const (
		interval = time.Second
		// Port-forwards to the unresponsive pods hang this long, then fail.
		// (In production an unreachable pod takes up to 10s to time out.)
		stall = 1500 * time.Millisecond
		// Allowed lateness of a check beyond its interval. Generous for
		// loaded CI machines (e.g. -race with many parallel tests); a delayed
		// pass waits out both stalls, giving gaps of 3s or more.
		slack = time.Second
	)

	config := isoConfig("config", "default", "api")
	config.Spec.Thresholds.CheckIntervalSeconds = int(interval / time.Second)
	e := newIsoEnv(t, config)

	unresponsive := []string{"stuck-0", "stuck-1"}
	healthy := []string{"healthy-0", "healthy-1", "healthy-2"}
	for _, name := range unresponsive {
		e.addPod(isoPod(name, "default", "api")) // 200m of 100m requested: over threshold
		e.pprof.stall("default", name, stall)
	}
	for _, name := range healthy {
		e.addPod(isoPod(name, "default", "api"))
		e.metrics.setCPU("default", name, "10m") // below threshold: checked, not captured
	}

	start := time.Now()
	e.reconcile(config)

	// Each unresponsive pod is attempted on the first pass, fails after the
	// stall, and must be attempted again on a later pass.
	deadline := start.Add(10 * time.Second)
	for time.Now().Before(deadline) && !(e.pprof.captures("default", "stuck-0") >= 2 && e.pprof.captures("default", "stuck-1") >= 2) {
		time.Sleep(10 * time.Millisecond)
	}
	end := time.Now()

	for _, name := range unresponsive {
		if n := e.pprof.captures("default", name); n < 2 {
			t.Errorf("unresponsive pod %s: %d capture attempts in %v, want it retried after its failed attempt",
				name, n, end.Sub(start).Round(time.Millisecond))
		}
	}

	for _, name := range healthy {
		checks := e.metrics.queryTimes("default", name)
		points := append(append([]time.Time{start}, checks...), end)
		var gaps []string
		var worst time.Duration
		for i := 1; i < len(points); i++ {
			gap := points[i].Sub(points[i-1])
			gaps = append(gaps, gap.Round(10*time.Millisecond).String())
			if gap > worst {
				worst = gap
			}
		}
		if worst > interval+slack {
			t.Errorf("healthy pod %s: %d threshold checks in %v; longest gap between checks %v, want at most %v (interval %v). Gaps: %s",
				name, len(checks), end.Sub(start).Round(time.Millisecond), worst.Round(time.Millisecond),
				interval+slack, interval, strings.Join(gaps, ", "))
		}
	}

	if t.Failed() {
		t.Logf("port-forward attempts: %v", e.pprof.all())
	}
}
