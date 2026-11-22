package controller

// Regression tests for schedules being reset by reconciles.
//
// The controller requeues every ProfilingConfig every 30s. The original
// controller restarted a config's monitors on every reconcile, so a check
// interval longer than 30s never fired, on-demand profiling at its 35s
// default never ran, and a pod's cooldown was wiped every 30s.
//
// Like isolation_test.go (whose test environment they use), these tests only
// rely on APIs that also exist in the original controller. They run in real
// time and drive Reconcile at the controller's requeue cadence themselves.
// By default they use the same ratios at a compressed scale; set
// BOLOMETER_REAL_TIMING_TESTS=1 to also run them with the real values
// (60s / 35s / 30s), which takes about five minutes.

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	profilingv1alpha1 "github.com/a-kash-singh/bolometer/api/v1alpha1"
)

// requeueDriver calls Reconcile for a config at a fixed cadence, as
// controller-runtime does when Reconcile returns RequeueAfter.
type requeueDriver struct {
	stop chan struct{}
	done sync.WaitGroup
}

func (e *isoEnv) driveRequeue(config *profilingv1alpha1.ProfilingConfig, every time.Duration) *requeueDriver {
	d := &requeueDriver{stop: make(chan struct{})}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(config)}
	d.done.Add(1)
	go func() {
		defer d.done.Done()
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-d.stop:
				return
			case <-ticker.C:
				if _, err := e.r.Reconcile(context.Background(), req); err != nil {
					e.t.Errorf("reconcile: %v", err)
				}
			}
		}
	}()
	e.t.Cleanup(d.Stop)
	return d
}

func (d *requeueDriver) Stop() {
	select {
	case <-d.stop:
	default:
		close(d.stop)
	}
	d.done.Wait()
}

// timingScale is one set of values for the tests below.
type timingScale struct {
	name          string
	requeue       time.Duration // how often the controller reconciles
	checkInterval int           // thresholds.checkIntervalSeconds, longer than requeue
	onDemand      int           // onDemand.intervalSeconds, longer than requeue
	// For the cooldown test: the check interval, the requeue (not a multiple
	// of the check interval, so that checks and reconciles don't race in the
	// original controller) and the cooldown, longer than the requeue.
	cooldownCheck   int
	cooldownRequeue time.Duration
	cooldown        int
}

func timingScales(t *testing.T) []timingScale {
	scales := []timingScale{
		{name: "scaled", requeue: time.Second, checkInterval: 2, onDemand: 2, cooldownCheck: 1, cooldownRequeue: 2 * time.Second, cooldown: 5},
	}
	if os.Getenv("BOLOMETER_REAL_TIMING_TESTS") != "" {
		scales = append(scales, timingScale{name: "real", requeue: 30 * time.Second, checkInterval: 60, onDemand: 35, cooldownCheck: 10, cooldownRequeue: 30 * time.Second, cooldown: 60})
	} else {
		t.Log("set BOLOMETER_REAL_TIMING_TESTS=1 to also run with the real timings (60s check interval, 35s on-demand interval, 30s requeue)")
	}
	return scales
}

func secs(n int) time.Duration { return time.Duration(n) * time.Second }

// A check interval longer than the requeue must still fire every interval.
func TestTiming_CheckIntervalLongerThanRequeueStillFires(t *testing.T) {
	for _, sc := range timingScales(t) {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			config := isoConfig("config", "default", "api")
			config.Spec.Thresholds.CheckIntervalSeconds = sc.checkInterval
			e := newIsoEnv(t, config)
			e.addPod(isoPod("api-0", "default", "api"))
			e.metrics.setCPU("default", "api-0", "10m") // below threshold: checked, not captured

			e.reconcile(config)
			e.driveRequeue(config, sc.requeue)

			// Two and a half intervals: the checks at 1x and 2x the interval
			// must have happened.
			observe := secs(sc.checkInterval) * 5 / 2
			time.Sleep(observe)
			if got := e.metrics.queries("default", "api-0"); got < 2 {
				t.Errorf("%d threshold checks in %v with checkIntervalSeconds=%d while reconciling every %v; want at least 2",
					got, observe, sc.checkInterval, sc.requeue)
			}
		})
	}
}

// On-demand profiling with an interval longer than the requeue must still
// capture every interval.
func TestTiming_OnDemandIntervalLongerThanRequeueStillCaptures(t *testing.T) {
	for _, sc := range timingScales(t) {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			config := isoConfig("config", "default", "api")
			config.Spec.OnDemand = &profilingv1alpha1.OnDemandConfig{Enabled: true, IntervalSeconds: sc.onDemand}
			e := newIsoEnv(t, config)
			e.addPod(isoPod("api-0", "default", "api")) // captures fail fast, which is fine here

			e.reconcile(config)
			e.driveRequeue(config, sc.requeue)

			observe := secs(sc.onDemand) * 5 / 2
			time.Sleep(observe)
			if got := e.pprof.captures("default", "api-0"); got < 2 {
				t.Errorf("%d on-demand captures in %v with intervalSeconds=%d while reconciling every %v; want at least 2",
					got, observe, sc.onDemand, sc.requeue)
			}
		})
	}
}

// After a capture, a pod must not be captured again before cooldownSeconds,
// however many reconciles happen in between.
func TestTiming_CooldownSurvivesRequeue(t *testing.T) {
	for _, sc := range timingScales(t) {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			// Captures must succeed (reach S3) to start a cooldown.
			s3 := newS3Server(t)
			config := isoConfig("config", "default", "api")
			config.Spec.Thresholds.CheckIntervalSeconds = sc.cooldownCheck
			config.Spec.Thresholds.CooldownSeconds = sc.cooldown
			config.Spec.ProfileTypes = []string{"heap"}
			config.Spec.S3Config.Endpoint = s3.srv.URL
			e := newIsoEnv(t, config)
			e.pprof.serveProfiles = true
			e.addPod(isoPod("api-0", "default", "api")) // over the threshold

			e.reconcile(config)
			e.driveRequeue(config, sc.cooldownRequeue)

			// Wait for the first capture, then watch for most of its cooldown.
			deadline := time.Now().Add(secs(sc.cooldownCheck)*3 + 5*time.Second)
			for e.pprof.captures("default", "api-0") == 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			attempts := e.pprof.attemptTimesOf("default", "api-0")
			if len(attempts) == 0 {
				t.Fatalf("pod was never captured")
			}
			first := attempts[0]
			time.Sleep(time.Until(first.Add(secs(sc.cooldown) - secs(sc.cooldownCheck)/2)))

			var early []string
			for _, at := range e.pprof.attemptTimesOf("default", "api-0")[1:] {
				early = append(early, at.Sub(first).Round(100*time.Millisecond).String())
			}
			if len(early) > 0 {
				t.Errorf("pod captured again %s after its capture, within cooldownSeconds=%d, while reconciling every %v",
					strings.Join(early, " and "), sc.cooldown, sc.cooldownRequeue)
			}
		})
	}
}

// A spec edit must reach the running monitor. This holds for the original
// controller too (it restarted the monitor with the new spec).
func TestTiming_SpecEditReachesRunningMonitor(t *testing.T) {
	t.Parallel()

	s3 := newS3Server(t)
	config := isoConfig("config", "default", "api")
	config.Spec.Thresholds.CheckIntervalSeconds = 1
	config.Spec.Thresholds.CPUThresholdPercent = 80
	config.Spec.ProfileTypes = []string{"heap"}
	config.Spec.S3Config.Prefix = "before-edit"
	config.Spec.S3Config.Endpoint = s3.srv.URL
	e := newIsoEnv(t, config)
	e.pprof.serveProfiles = true
	e.addPod(isoPod("api-0", "default", "api"))
	e.metrics.setCPU("default", "api-0", "50m") // 50%: below the 80% threshold

	e.reconcile(config)
	time.Sleep(2500 * time.Millisecond)
	if n := e.pprof.captures("default", "api-0"); n != 0 {
		t.Fatalf("pod captured %d times below the threshold", n)
	}

	// Lower the threshold under the pod's usage and move uploads to a new
	// prefix; the controller reconciles on the edit.
	edited := &profilingv1alpha1.ProfilingConfig{}
	if err := e.api.Get(context.Background(), client.ObjectKeyFromObject(config), edited); err != nil {
		t.Fatal(err)
	}
	edited.Spec.Thresholds.CPUThresholdPercent = 20
	edited.Spec.S3Config.Prefix = "after-edit"
	if err := e.api.Update(context.Background(), edited); err != nil {
		t.Fatal(err)
	}
	e.reconcile(config)

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := s3.counts(); ok > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	s3.mu.Lock()
	puts := append([]s3Put(nil), s3.puts...)
	s3.mu.Unlock()
	if len(puts) == 0 {
		t.Fatalf("no capture after lowering the threshold to 20%% (captures: %v)", e.pprof.all())
	}
	for _, p := range puts {
		if !strings.HasPrefix(p.key, "test-bucket/after-edit/") {
			t.Errorf("upload key %s does not use the edited prefix", p.key)
		}
	}
}
