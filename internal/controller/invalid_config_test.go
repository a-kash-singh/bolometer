package controller

// Regression test for invalid ProfilingConfig edits.
//
// Like isolation_test.go (whose test environment it uses), this only relies
// on APIs that also exist in the original controller, so the two files can
// be copied onto the original code.

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	profilingv1alpha1 "github.com/a-kash-singh/bolometer/api/v1alpha1"
)

// An edit that makes the config invalid must stop its monitor (it must not
// keep profiling with the settings the user replaced), be reported through
// the Ready condition and a Warning event, and not make Reconcile fail (that
// only causes retries and log spam). Fixing the config must restore Ready,
// confirm the fix with a Normal event and resume monitoring.
func TestInvalidConfig_BadEditStopsMonitorAndIsReportedUntilFixed(t *testing.T) {
	t.Parallel()
	config := isoConfig("config", "default", "api")
	config.Spec.OnDemand = &profilingv1alpha1.OnDemandConfig{Enabled: true, IntervalSeconds: 1}
	e := newIsoEnv(t, config)
	e.addPod(isoPod("api-0", "default", "api"))
	key := client.ObjectKeyFromObject(config)

	reconcile := func() error {
		_, err := e.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		return err
	}
	edit := func(mutate func(*profilingv1alpha1.ProfilingConfigSpec)) {
		t.Helper()
		latest := &profilingv1alpha1.ProfilingConfig{}
		if err := e.api.Get(context.Background(), key, latest); err != nil {
			t.Fatal(err)
		}
		mutate(&latest.Spec)
		if err := e.api.Update(context.Background(), latest); err != nil {
			t.Fatal(err)
		}
	}
	ready := func() *metav1.Condition {
		latest := &profilingv1alpha1.ProfilingConfig{}
		if err := e.api.Get(context.Background(), key, latest); err != nil {
			t.Fatal(err)
		}
		return meta.FindStatusCondition(latest.Status.Conditions, "Ready")
	}
	events := func(reason string) []corev1.Event {
		list, err := e.pods.CoreV1().Events("default").List(context.Background(), metav1.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var out []corev1.Event
		for _, ev := range list.Items {
			if ev.Reason == reason && ev.InvolvedObject.Name == "config" {
				out = append(out, ev)
			}
		}
		return out
	}
	waitForEvent := func(reason string) []corev1.Event {
		deadline := time.Now().Add(5 * time.Second)
		for len(events(reason)) == 0 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		return events(reason)
	}

	if err := reconcile(); err != nil {
		t.Fatalf("reconcile of the valid config: %v", err)
	}
	e.waitForCaptures("default", "api-0", 1)

	// The bad edit: no S3 bucket.
	edit(func(s *profilingv1alpha1.ProfilingConfigSpec) { s.S3Config.Bucket = "" })
	if err := reconcile(); err != nil {
		t.Errorf("Reconcile returned %q for the invalid config; want nil once the problem is recorded in the status", err)
	}
	if err := reconcile(); err != nil { // e.g. a later requeue or unrelated event
		t.Errorf("second Reconcile returned %q for the invalid config; want nil", err)
	}

	time.Sleep(300 * time.Millisecond) // let a capture started just before the edit reach the pod
	before := e.pprof.captures("default", "api-0")
	time.Sleep(2500 * time.Millisecond)
	if n := e.pprof.captures("default", "api-0") - before; n > 0 {
		t.Errorf("monitor kept capturing with the replaced settings: %d captures after the invalid edit", n)
	}

	if c := ready(); c == nil || c.Status != metav1.ConditionFalse || c.Reason != "InvalidConfig" ||
		!strings.Contains(c.Message, "s3 bucket is required") {
		t.Errorf("Ready condition after the invalid edit: %+v; want status False, reason InvalidConfig, message naming the missing bucket", c)
	}
	if warnings := waitForEvent("InvalidConfig"); len(warnings) != 1 || warnings[0].Type != corev1.EventTypeWarning ||
		!strings.Contains(warnings[0].Message, "s3 bucket is required") {
		t.Errorf("InvalidConfig events: %+v; want one Warning naming the missing bucket", warnings)
	} else {
		time.Sleep(500 * time.Millisecond) // the second reconcile must not have repeated it
		if w := events("InvalidConfig"); w[0].Count > 1 {
			t.Errorf("InvalidConfig event emitted %d times for one invalid edit; want once", w[0].Count)
		}
	}

	// The fix.
	edit(func(s *profilingv1alpha1.ProfilingConfigSpec) { s.S3Config.Bucket = "fixed-bucket" })
	if err := reconcile(); err != nil {
		t.Fatalf("reconcile of the fixed config: %v", err)
	}

	if c := ready(); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Ready condition after the fix: %+v; want status True", c)
	}
	if fixed := waitForEvent("ConfigValid"); len(fixed) != 1 || fixed[0].Type != corev1.EventTypeNormal {
		t.Errorf("ConfigValid events: %+v; want one Normal event confirming the fix", fixed)
	}
	resumed := e.pprof.captures("default", "api-0")
	deadline := time.Now().Add(5 * time.Second)
	for e.pprof.captures("default", "api-0") == resumed && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if e.pprof.captures("default", "api-0") == resumed {
		t.Errorf("monitoring did not resume after the fix")
	}
}
