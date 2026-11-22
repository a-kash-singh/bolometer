package controller

// Regression test for on-demand captures being held up by a slow pod.
//
// Like isolation_test.go (whose test environment it uses), this only relies
// on APIs that also exist in the original controller.

import (
	"strings"
	"testing"
	"time"

	profilingv1alpha1 "github.com/a-kash-singh/bolometer/api/v1alpha1"
)

// An unresponsive pod must not hold up the on-demand captures of the other
// pods in the config; it is retried on a later run once its attempt failed.
func TestOnDemand_UnresponsivePodDoesNotDelayOtherPods(t *testing.T) {
	t.Parallel()
	const (
		interval = time.Second
		stall    = 2500 * time.Millisecond
		// Allowed lateness of a capture. Generous for loaded CI machines
		// (e.g. -race with many parallel tests); a delayed run shows gaps
		// of at least the stall (2.5s) plus the interval.
		slack = time.Second
	)

	config := isoConfig("config", "default", "api")
	config.Spec.OnDemand = &profilingv1alpha1.OnDemandConfig{Enabled: true, IntervalSeconds: int(interval / time.Second)}
	e := newIsoEnv(t, config)

	e.addPod(isoPod("stuck-0", "default", "api"))
	e.pprof.stall("default", "stuck-0", stall)
	healthy := []string{"healthy-0", "healthy-1", "healthy-2"} // fail fast
	for _, name := range healthy {
		e.addPod(isoPod(name, "default", "api"))
	}

	start := time.Now()
	e.reconcile(config)

	deadline := start.Add(10 * time.Second)
	for time.Now().Before(deadline) && e.pprof.captures("default", "stuck-0") < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	end := time.Now()

	if n := e.pprof.captures("default", "stuck-0"); n < 2 {
		t.Errorf("unresponsive pod stuck-0: %d capture attempts in %v, want it retried after its failed attempt",
			n, end.Sub(start).Round(time.Millisecond))
	}
	for _, name := range healthy {
		attempts := e.pprof.attemptTimesOf("default", name)
		points := append(append([]time.Time{start}, attempts...), end)
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
			t.Errorf("healthy pod %s: %d on-demand captures in %v; longest gap %v, want at most %v. Gaps: %s",
				name, len(attempts), end.Sub(start).Round(time.Millisecond), worst.Round(time.Millisecond),
				interval+slack, strings.Join(gaps, ", "))
		}
	}
}
