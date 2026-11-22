package profiler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func newTestProfiler(t *testing.T, handler http.HandlerFunc) *Profiler {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	cfg := &rest.Config{Host: srv.URL}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return NewProfiler(cs, cfg)
}

func captureWithDeadline(t *testing.T, p *Profiler, ctx context.Context) error {
	t.Helper()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "app-0", Namespace: "default"}}
	done := make(chan error, 1)
	go func() {
		_, err := p.CaptureProfiles(ctx, pod, []string{"heap"})
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("CaptureProfiles did not return; a failed port-forward must not block the monitor forever")
		return nil
	}
}

// A port-forward the API server rejects (pod gone, RBAC, ...) used to block
// CaptureProfiles forever: ForwardPorts fails without closing readyChan.
func TestCaptureProfiles_PortForwardRejected(t *testing.T) {
	p := newTestProfiler(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	})

	err := captureWithDeadline(t, p, context.Background())
	if err == nil {
		t.Fatal("expected an error for a rejected port-forward")
	}
}

// Cancelling the context (ProfilingConfig deleted, manager stopping) must
// abort a port-forward that is still being set up.
func TestCaptureProfiles_ContextCancelledDuringPortForwardSetup(t *testing.T) {
	release := make(chan struct{})
	p := newTestProfiler(t, func(w http.ResponseWriter, r *http.Request) {
		<-release // API server never answers the upgrade
	})
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := captureWithDeadline(t, p, ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline error, got %v", err)
	}
}
