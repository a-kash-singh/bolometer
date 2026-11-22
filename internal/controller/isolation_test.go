package controller

// Regression tests for per-ProfilingConfig pod tracking.
//
// These tests deliberately use only APIs that also exist in the original
// controller (NewProfilingConfigReconciler, Reconcile, the PodWatcher's
// GetTrackedPods/UpdateLastProfileTime and its lastProfileTime map), so this
// file compiles and runs against the original code as well. They observe the
// monitors from the outside:
//
//   - threshold checks, through a recording metrics client;
//   - capture attempts, through a test API server that records every pod
//     port-forward request and completes the SPDY upgrade. By default it
//     then answers pprof requests with 503, so each capture fails fast
//     without touching S3; with serveProfiles it answers with a small
//     profile, and captures upload to a fake S3 endpoint (s3Server). It can
//     also fail chosen endpoints, stall chosen pods' port-forwards, or break
//     their connections;
//   - status writes, through the fake API (serializeWrites, newLaggingCache).

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/httpstream"
	spdystream "k8s.io/apimachinery/pkg/util/httpstream/spdy"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	metricsapi "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"
	metricsv1beta1 "k8s.io/metrics/pkg/client/clientset/versioned/typed/metrics/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	profilingv1alpha1 "github.com/a-kash-singh/bolometer/api/v1alpha1"
)

// ---------------------------------------------------------------------------
// Test environment
// ---------------------------------------------------------------------------

// isoEnv is a reconciler built by NewProfilingConfigReconciler on top of
// fake pods, a recording metrics client and a recording port-forward server.
type isoEnv struct {
	t       *testing.T
	r       *ProfilingConfigReconciler
	pods    *fake.Clientset
	metrics *recordingMetrics
	pprof   *pprofServer
	// api reads ProfilingConfigs directly, bypassing any simulated cache lag.
	api client.Client
}

// isoOptions tweaks the test environment.
type isoOptions struct {
	// cacheLag makes the reconciler read ProfilingConfigs from a snapshot
	// that catches up with writes only after this delay, like the manager's
	// informer-backed client.
	cacheLag time.Duration
}

func newIsoEnv(t *testing.T, objs ...client.Object) *isoEnv {
	t.Helper()
	return newIsoEnvWith(t, isoOptions{}, objs...)
}

func newIsoEnvWith(t *testing.T, opts isoOptions, objs ...client.Object) *isoEnv {
	t.Helper()

	scheme := runtime.NewScheme()
	_ = profilingv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	api := serializeWrites(fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&profilingv1alpha1.ProfilingConfig{}).
		Build())
	var ctrlClient client.Client = api
	if opts.cacheLag > 0 {
		ctrlClient = newLaggingCache(api, opts.cacheLag)
	}

	pprof := newPprofServer(t)
	restConfig := &rest.Config{Host: pprof.srv.URL}
	apiClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		t.Fatal(err)
	}
	pods := fake.NewSimpleClientset()
	metrics := &recordingMetrics{}

	r := NewProfilingConfigReconciler(
		ctrlClient,
		scheme,
		&portForwardingClientset{Clientset: pods, rest: apiClient.CoreV1().RESTClient()},
		metrics,
		restConfig,
	)
	t.Cleanup(r.baseCancel)

	return &isoEnv{t: t, r: r, pods: pods, metrics: metrics, pprof: pprof, api: api}
}

func (e *isoEnv) addPod(pod *corev1.Pod) {
	e.t.Helper()
	if _, err := e.pods.CoreV1().Pods(pod.Namespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		e.t.Fatalf("create pod %s/%s: %v", pod.Namespace, pod.Name, err)
	}
}

func (e *isoEnv) updatePod(pod *corev1.Pod) {
	e.t.Helper()
	if _, err := e.pods.CoreV1().Pods(pod.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		e.t.Fatalf("update pod %s/%s: %v", pod.Namespace, pod.Name, err)
	}
}

func (e *isoEnv) deletePod(namespace, name string) {
	e.t.Helper()
	if err := e.pods.CoreV1().Pods(namespace).Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
		e.t.Fatalf("delete pod %s/%s: %v", namespace, name, err)
	}
}

func (e *isoEnv) deleteConfig(config *profilingv1alpha1.ProfilingConfig) {
	e.t.Helper()
	if err := e.r.Delete(context.Background(), config); err != nil {
		e.t.Fatalf("delete config: %v", err)
	}
}

// editSpec changes a config's spec through the API, retrying on conflicts
// (status writes by the controller can land between the read and the
// update), as kubectl does.
func (e *isoEnv) editSpec(key client.ObjectKey, mutate func(*profilingv1alpha1.ProfilingConfigSpec)) {
	e.t.Helper()
	for attempt := 0; ; attempt++ {
		latest := &profilingv1alpha1.ProfilingConfig{}
		if err := e.api.Get(context.Background(), key, latest); err != nil {
			e.t.Fatal(err)
		}
		mutate(&latest.Spec)
		err := e.api.Update(context.Background(), latest)
		if err == nil {
			return
		}
		if !apierrors.IsConflict(err) || attempt == 10 {
			e.t.Fatalf("update config: %v", err)
		}
	}
}

func (e *isoEnv) reconcile(config *profilingv1alpha1.ProfilingConfig) {
	e.t.Helper()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: config.Namespace, Name: config.Name}}
	if _, err := e.r.Reconcile(context.Background(), req); err != nil {
		e.t.Fatalf("reconcile %s: %v", req.NamespacedName, err)
	}
}

// trackedPods returns the sorted namespace/name of every tracked pod.
func (e *isoEnv) trackedPods() []string {
	seen := map[string]bool{}
	var out []string
	for _, tracked := range e.r.podWatcher.GetTrackedPods() {
		key := tracked.Pod.Namespace + "/" + tracked.Pod.Name
		if !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// hasCooldownState reports whether the pod watcher still holds cooldown
// state for the pod.
func (e *isoEnv) hasCooldownState(namespace, name string) bool {
	pw := e.r.podWatcher
	pw.mu.RLock()
	defer pw.mu.RUnlock()
	_, ok := pw.lastProfileTime[namespace+"/"+name]
	return ok
}

// waitForCaptures waits until the pod has had at least n capture attempts.
// With a 1s schedule, n=2 guarantees that a complete run over every pod the
// monitor considers has finished in between.
func (e *isoEnv) waitForCaptures(namespace, name string, n int) {
	e.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for e.pprof.captures(namespace, name) < n {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %d capture attempts of %s/%s; attempts so far: %v",
				n, namespace, name, e.pprof.all())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// isoConfig returns a ProfilingConfig selecting app=<app> pods in namespace,
// with both schedules effectively off; tests enable what they need.
func isoConfig(name, namespace, app string) *profilingv1alpha1.ProfilingConfig {
	config := createTestProfilingConfig(name, namespace)
	config.Spec.Selector.LabelSelector = map[string]string{"app": app}
	config.Spec.Thresholds.CheckIntervalSeconds = 3600
	return config
}

// isoPod returns a running, profiling-enabled pod labelled app=<app>, with a
// 100m CPU request so that the recorded 200m usage exceeds any threshold.
func isoPod(name, namespace, app string) *corev1.Pod {
	pod := createTestPod(name, namespace, true)
	pod.Labels["app"] = app
	pod.Spec.Containers[0].Resources.Requests = corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("100m"),
	}
	return pod
}

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// portForwardingClientset serves pods from a fake clientset, but hands out a
// real REST client so the profiler's port-forward requests reach pprofServer.
type portForwardingClientset struct {
	*fake.Clientset
	rest rest.Interface
}

func (c *portForwardingClientset) CoreV1() corev1client.CoreV1Interface {
	return portForwardingCoreV1{CoreV1Interface: c.Clientset.CoreV1(), rest: c.rest}
}

type portForwardingCoreV1 struct {
	corev1client.CoreV1Interface
	rest rest.Interface
}

func (c portForwardingCoreV1) RESTClient() rest.Interface { return c.rest }

// pprofServer records pod port-forward requests. It completes the SPDY
// upgrade, then reports an error on every forwarded connection, so the
// profiler's pprof request fails quickly.
type pprofServer struct {
	srv *httptest.Server

	closing chan struct{}

	mu           sync.Mutex
	attempts     map[string]int         // namespace/name -> port-forward requests
	attemptTimes map[string][]time.Time // namespace/name -> when they arrived
	conns        []httpstream.Connection
	// stalls makes port-forwards to these pods (namespace/name) unresponsive:
	// the request hangs for the given time and is then rejected.
	stalls map[string]time.Duration
	// stalled counts port-forwards currently stalled, per namespace and in
	// total, and the maximum seen.
	stalled, maxStalled           map[string]int
	stalledTotal, maxStalledTotal int
	// serveProfiles makes forwarded pprof requests succeed. Otherwise they
	// are answered with 503 Service Unavailable.
	serveProfiles bool
	// breakConns makes the next n port-forward connections to a pod
	// (namespace/name) fail the way the kubelet reports a pod that refuses
	// the connection: an error on the error stream, the data stream closed.
	breakConns map[string]int
	// failEndpoints makes these pprof paths (e.g. "/debug/pprof/profile")
	// answer 500 Internal Server Error when profiles are served.
	failEndpoints map[string]bool
	// requests counts forwarded pprof requests per namespace/name and path.
	requests map[string]map[string]int
}

func newPprofServer(t *testing.T) *pprofServer {
	s := &pprofServer{
		attempts:     map[string]int{},
		attemptTimes: map[string][]time.Time{},
		stalls:       map[string]time.Duration{},
		stalled:      map[string]int{},
		maxStalled:   map[string]int{},
		closing:      make(chan struct{}),
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(func() {
		close(s.closing)
		s.mu.Lock()
		for _, c := range s.conns {
			c.Close()
		}
		s.mu.Unlock()
		s.srv.Close()
	})
	return s
}

func (s *pprofServer) serve(w http.ResponseWriter, req *http.Request) {
	// /api/v1/namespaces/{ns}/pods/{name}/portforward
	parts := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
	if len(parts) != 7 || parts[2] != "namespaces" || parts[4] != "pods" || parts[6] != "portforward" {
		http.Error(w, "unexpected request "+req.URL.Path, http.StatusNotFound)
		return
	}
	namespace, pod := parts[3], parts[5]
	s.mu.Lock()
	s.attempts[namespace+"/"+pod]++
	s.attemptTimes[namespace+"/"+pod] = append(s.attemptTimes[namespace+"/"+pod], time.Now())
	stall, stalled := s.stalls[namespace+"/"+pod]
	serveProfiles := s.serveProfiles
	broken := s.breakConns[namespace+"/"+pod] > 0
	if broken {
		s.breakConns[namespace+"/"+pod]--
	}
	if stalled {
		s.stalled[namespace]++
		s.stalledTotal++
		if s.stalled[namespace] > s.maxStalled[namespace] {
			s.maxStalled[namespace] = s.stalled[namespace]
		}
		if s.stalledTotal > s.maxStalledTotal {
			s.maxStalledTotal = s.stalledTotal
		}
	}
	s.mu.Unlock()

	if stalled {
		select {
		case <-time.After(stall):
		case <-s.closing:
		}
		s.mu.Lock()
		s.stalled[namespace]--
		s.stalledTotal--
		s.mu.Unlock()
		http.Error(w, "pod is not responding", http.StatusServiceUnavailable)
		return
	}

	if _, err := httpstream.Handshake(req, w, []string{"portforward.k8s.io"}); err != nil {
		return
	}
	conn := spdystream.NewResponseUpgrader().UpgradeResponse(w, req,
		func(stream httpstream.Stream, replySent <-chan struct{}) error {
			go func() {
				// The stream may only be used once its reply frame has been
				// sent; data or a close that overtakes the reply can be lost,
				// leaving the client waiting on the stream.
				<-replySent
				defer stream.Close()
				switch stream.Headers().Get(corev1.StreamType) {
				case corev1.StreamTypeError:
					if broken {
						_, _ = stream.Write([]byte("error forwarding port 6060 to pod: connection refused"))
					}
				case corev1.StreamTypeData:
					if !broken {
						s.serveProfile(stream, namespace, pod, serveProfiles)
					}
				}
			}()
			return nil
		})
	if conn == nil {
		return
	}
	s.mu.Lock()
	s.conns = append(s.conns, conn)
	s.mu.Unlock()
}

// serveProfile answers one forwarded pprof HTTP request: with a small profile
// if serveProfiles, with 503 otherwise, or with 500 for a failing endpoint.
func (s *pprofServer) serveProfile(stream io.ReadWriter, namespace, pod string, serveProfiles bool) {
	req, err := http.ReadRequest(bufio.NewReader(stream))
	if err != nil {
		return
	}
	s.mu.Lock()
	if s.requests == nil {
		s.requests = map[string]map[string]int{}
	}
	if s.requests[namespace+"/"+pod] == nil {
		s.requests[namespace+"/"+pod] = map[string]int{}
	}
	s.requests[namespace+"/"+pod][req.URL.Path]++
	fail := s.failEndpoints[req.URL.Path]
	s.mu.Unlock()
	if !serveProfiles {
		msg := "pprof is not served by the test API server"
		_, _ = fmt.Fprintf(stream, "HTTP/1.1 503 Service Unavailable\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(msg), msg)
		return
	}
	if fail {
		msg := "profiling failed"
		_, _ = fmt.Fprintf(stream, "HTTP/1.1 500 Internal Server Error\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(msg), msg)
		return
	}
	body := fmt.Sprintf("profile of %s: %s", pod, req.URL.RequestURI())
	_, _ = fmt.Fprintf(stream, "HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
}

// breakConnections makes the next n port-forward connections to the pod fail
// as the kubelet reports a pod that refuses the connection.
func (s *pprofServer) breakConnections(namespace, name string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.breakConns == nil {
		s.breakConns = map[string]int{}
	}
	s.breakConns[namespace+"/"+name] = n
}

// failEndpoint makes requests to a pprof path fail with 500.
func (s *pprofServer) failEndpoint(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failEndpoints == nil {
		s.failEndpoints = map[string]bool{}
	}
	s.failEndpoints[path] = true
}

// endpointRequests returns how often each pprof path was requested from the pod.
func (s *pprofServer) endpointRequests(namespace, name string) map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int{}
	for path, n := range s.requests[namespace+"/"+name] {
		out[path] = n
	}
	return out
}

// stall makes port-forwards to the pod hang for d and then fail.
func (s *pprofServer) stall(namespace, name string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stalls[namespace+"/"+name] = d
}

func (s *pprofServer) captures(namespace, name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts[namespace+"/"+name]
}

// attemptTimesOf returns when port-forwards to the pod were requested.
func (s *pprofServer) attemptTimesOf(namespace, name string) []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.attemptTimes[namespace+"/"+name]...)
}

// maxConcurrentStalled returns the most port-forwards that were stalled at
// once in the namespace ("" for all namespaces together).
func (s *pprofServer) maxConcurrentStalled(namespace string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if namespace == "" {
		return s.maxStalledTotal
	}
	return s.maxStalled[namespace]
}

func (s *pprofServer) all() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int{}
	for k, v := range s.attempts {
		out[k] = v
	}
	return out
}

// recordingMetrics is a metrics clientset that reports 200m CPU usage for
// every pod (unless overridden with setCPU) and records when each pod was
// queried.
type recordingMetrics struct {
	metricsv.Interface // only MetricsV1beta1 is used

	mu      sync.Mutex
	queried map[string][]time.Time
	cpu     map[string]string
}

// setCPU overrides the CPU usage reported for a pod, e.g. "10m".
func (m *recordingMetrics) setCPU(namespace, name, usage string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cpu == nil {
		m.cpu = map[string]string{}
	}
	m.cpu[namespace+"/"+name] = usage
}

// queryTimes returns when the pod's metrics were queried.
func (m *recordingMetrics) queryTimes(namespace, name string) []time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]time.Time(nil), m.queried[namespace+"/"+name]...)
}

func (m *recordingMetrics) MetricsV1beta1() metricsv1beta1.MetricsV1beta1Interface {
	return recordingMetricsV1beta1{m: m}
}

func (m *recordingMetrics) queries(namespace, name string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.queried[namespace+"/"+name])
}

type recordingMetricsV1beta1 struct {
	metricsv1beta1.MetricsV1beta1Interface // only PodMetricses is used
	m                                      *recordingMetrics
}

func (c recordingMetricsV1beta1) PodMetricses(namespace string) metricsv1beta1.PodMetricsInterface {
	return recordingPodMetrics{m: c.m, namespace: namespace}
}

type recordingPodMetrics struct {
	metricsv1beta1.PodMetricsInterface // only Get is used
	m                                  *recordingMetrics
	namespace                          string
}

func (c recordingPodMetrics) Get(_ context.Context, name string, _ metav1.GetOptions) (*metricsapi.PodMetrics, error) {
	c.m.mu.Lock()
	if c.m.queried == nil {
		c.m.queried = map[string][]time.Time{}
	}
	key := c.namespace + "/" + name
	c.m.queried[key] = append(c.m.queried[key], time.Now())
	usage := "200m"
	if u, ok := c.m.cpu[key]; ok {
		usage = u
	}
	c.m.mu.Unlock()

	return &metricsapi.PodMetrics{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.namespace},
		Containers: []metricsapi.ContainerMetrics{{
			Name:  "test-container",
			Usage: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(usage)},
		}},
	}, nil
}

// ---------------------------------------------------------------------------
// Isolation between ProfilingConfigs
// ---------------------------------------------------------------------------

// A config's threshold monitor must only check and capture the pods that
// config selected, not pods tracked for another config.
func TestIsolation_ThresholdMonitorOnlyUsesItsOwnPods(t *testing.T) {
	t.Parallel()
	configA := isoConfig("config-a", "team-a", "api")
	configA.Spec.Thresholds.CheckIntervalSeconds = 1
	configB := isoConfig("config-b", "team-b", "api") // B's own schedules never fire in this test

	e := newIsoEnv(t, configA, configB)
	e.addPod(isoPod("a-0", "team-a", "api"))
	e.addPod(isoPod("b-0", "team-b", "api"))
	e.reconcile(configA)
	e.reconcile(configB)

	e.waitForCaptures("team-a", "a-0", 2)

	if n := e.metrics.queries("team-b", "b-0"); n != 0 {
		t.Errorf("config-a's threshold monitor checked metrics of config-b's pod team-b/b-0 %d times", n)
	}
	if n := e.pprof.captures("team-b", "b-0"); n != 0 {
		t.Errorf("config-a's threshold monitor captured config-b's pod team-b/b-0 %d times", n)
	}
}

// A config's on-demand monitor must only capture the pods that config
// selected, even when another config selects pods in the same namespace.
func TestIsolation_OnDemandMonitorOnlyCapturesItsOwnPods(t *testing.T) {
	t.Parallel()
	configA := isoConfig("config-a", "shared", "frontend")
	configA.Spec.OnDemand = &profilingv1alpha1.OnDemandConfig{Enabled: true, IntervalSeconds: 1}
	configB := isoConfig("config-b", "shared", "backend")

	e := newIsoEnv(t, configA, configB)
	e.addPod(isoPod("frontend-0", "shared", "frontend"))
	e.addPod(isoPod("backend-0", "shared", "backend"))
	e.reconcile(configA)
	e.reconcile(configB)

	e.waitForCaptures("shared", "frontend-0", 2)

	if n := e.pprof.captures("shared", "backend-0"); n != 0 {
		t.Errorf("config-a's on-demand monitor captured config-b's pod shared/backend-0 %d times", n)
	}
}

// ---------------------------------------------------------------------------
// Dropping pods from tracking
// ---------------------------------------------------------------------------

func TestTracking_DeletedPodIsDropped(t *testing.T) {
	t.Parallel()
	config := isoConfig("config", "default", "api")
	config.Spec.OnDemand = &profilingv1alpha1.OnDemandConfig{Enabled: true, IntervalSeconds: 1}

	e := newIsoEnv(t, config)
	e.addPod(isoPod("api-0", "default", "api"))
	e.addPod(isoPod("api-1", "default", "api"))
	e.reconcile(config)
	e.r.podWatcher.UpdateLastProfileTime(isoPod("api-1", "default", "api"))

	e.deletePod("default", "api-1")
	e.reconcile(config)

	if got, want := e.trackedPods(), []string{"default/api-0"}; !equalStrings(got, want) {
		t.Errorf("tracked pods after deleting api-1: got %v, want %v", got, want)
	}
	if e.hasCooldownState("default", "api-1") {
		t.Errorf("cooldown state of deleted pod default/api-1 is still held")
	}

	e.waitForCaptures("default", "api-0", 2)
	if n := e.pprof.captures("default", "api-1"); n != 0 {
		t.Errorf("deleted pod default/api-1 was captured %d times", n)
	}
}

func TestTracking_PodThatStopsMatchingIsDropped(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*corev1.Pod){
		"label no longer matches":  func(p *corev1.Pod) { p.Labels["app"] = "other" },
		"annotation removed":       func(p *corev1.Pod) { delete(p.Annotations, ProfilingEnabledAnnotation) },
		"annotation set to false":  func(p *corev1.Pod) { p.Annotations[ProfilingEnabledAnnotation] = "false" },
		"pod no longer running":    func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded },
		"pod failed":               func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed },
		"still matching (control)": func(p *corev1.Pod) { p.Labels["unrelated"] = "change" },
	}
	for name, mutate := range cases {
		mutate := mutate
		control := name == "still matching (control)"
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			config := isoConfig("config", "default", "api")
			e := newIsoEnv(t, config)
			e.addPod(isoPod("api-0", "default", "api"))
			e.addPod(isoPod("api-1", "default", "api"))
			e.reconcile(config)
			e.r.podWatcher.UpdateLastProfileTime(isoPod("api-1", "default", "api"))

			pod := isoPod("api-1", "default", "api")
			mutate(pod)
			e.updatePod(pod)
			e.reconcile(config)

			want := []string{"default/api-0"}
			if control {
				want = []string{"default/api-0", "default/api-1"}
			}
			if got := e.trackedPods(); !equalStrings(got, want) {
				t.Errorf("tracked pods: got %v, want %v", got, want)
			}
			if held := e.hasCooldownState("default", "api-1"); held != control {
				t.Errorf("cooldown state for default/api-1 held = %v, want %v", held, control)
			}
		})
	}
}

// Deleting a config must drop the pods it tracked, so no other config's
// monitor keeps profiling them.
func TestTracking_DeletedConfigDropsItsPods(t *testing.T) {
	t.Parallel()
	configA := isoConfig("config-a", "team-a", "api")
	configB := isoConfig("config-b", "team-b", "api")
	configB.Spec.OnDemand = &profilingv1alpha1.OnDemandConfig{Enabled: true, IntervalSeconds: 1}

	e := newIsoEnv(t, configA, configB)
	e.addPod(isoPod("a-0", "team-a", "api"))
	e.addPod(isoPod("b-0", "team-b", "api"))
	e.reconcile(configA)
	e.reconcile(configB)
	e.r.podWatcher.UpdateLastProfileTime(isoPod("a-0", "team-a", "api"))

	e.deleteConfig(configA)
	e.reconcile(configA)

	if got, want := e.trackedPods(), []string{"team-b/b-0"}; !equalStrings(got, want) {
		t.Errorf("tracked pods after deleting config-a: got %v, want %v", got, want)
	}
	if e.hasCooldownState("team-a", "a-0") {
		t.Errorf("cooldown state of team-a/a-0 is still held after its config was deleted")
	}

	e.waitForCaptures("team-b", "b-0", 2)
	if n := e.pprof.captures("team-a", "a-0"); n != 0 {
		t.Errorf("pod team-a/a-0 of deleted config-a was captured %d times", n)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Atomic writes
// ---------------------------------------------------------------------------

// serializeWrites makes the fake client's writes atomic, as they are in the
// API server. The controller-runtime fake client (v0.18) checks the
// resourceVersion and stores the object in separate steps, without a lock.
// Two concurrent updates (say a spec edit and a status write based on the
// object as it was before the edit) can then both pass the check, and the
// later one silently overwrites the other, spec included. The API server
// compares and stores atomically, so this cannot happen there.
func serializeWrites(c client.WithWatch) client.WithWatch {
	var mu sync.Mutex
	return interceptor.NewClient(c, interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			mu.Lock()
			defer mu.Unlock()
			return cl.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			mu.Lock()
			defer mu.Unlock()
			return cl.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			mu.Lock()
			defer mu.Unlock()
			return cl.Patch(ctx, obj, patch, opts...)
		},
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			mu.Lock()
			defer mu.Unlock()
			return cl.Delete(ctx, obj, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			mu.Lock()
			defer mu.Unlock()
			return cl.SubResource(sub).Update(ctx, obj, opts...)
		},
		SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			mu.Lock()
			defer mu.Unlock()
			return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
}

// ---------------------------------------------------------------------------
// Lagging cache
// ---------------------------------------------------------------------------

// newLaggingCache returns a client whose ProfilingConfig reads come from a
// snapshot that is refreshed lag after each write, like the informer cache
// behind the manager's client. Writes go straight through, so a write based
// on a stale read fails with a conflict, as it would against the API server.
func newLaggingCache(api client.WithWatch, lag time.Duration) client.WithWatch {
	c := &laggingCache{lag: lag, snap: map[client.ObjectKey]*profilingv1alpha1.ProfilingConfig{}}
	after := func(obj client.Object, err error) error {
		if _, ok := obj.(*profilingv1alpha1.ProfilingConfig); ok {
			c.refreshLater(api, client.ObjectKeyFromObject(obj))
		}
		return err
	}
	return interceptor.NewClient(api, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			pc, ok := obj.(*profilingv1alpha1.ProfilingConfig)
			if !ok {
				return cl.Get(ctx, key, obj, opts...)
			}
			return c.get(ctx, cl, key, pc)
		},
		Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			return after(obj, cl.Update(ctx, obj, opts...))
		},
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			return after(obj, cl.Patch(ctx, obj, patch, opts...))
		},
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			return after(obj, cl.Delete(ctx, obj, opts...))
		},
		SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			return after(obj, cl.SubResource(sub).Update(ctx, obj, opts...))
		},
		SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			return after(obj, cl.SubResource(sub).Patch(ctx, obj, patch, opts...))
		},
	})
}

type laggingCache struct {
	lag  time.Duration
	mu   sync.Mutex
	snap map[client.ObjectKey]*profilingv1alpha1.ProfilingConfig
}

func (c *laggingCache) get(ctx context.Context, api client.Client, key client.ObjectKey, obj *profilingv1alpha1.ProfilingConfig) error {
	c.mu.Lock()
	cached, ok := c.snap[key]
	c.mu.Unlock()
	if ok {
		if cached == nil {
			return apierrors.NewNotFound(profilingv1alpha1.GroupVersion.WithResource("profilingconfigs").GroupResource(), key.Name)
		}
		cached.DeepCopyInto(obj)
		return nil
	}
	if err := api.Get(ctx, key, obj); err != nil {
		return err
	}
	c.mu.Lock()
	c.snap[key] = obj.DeepCopy()
	c.mu.Unlock()
	return nil
}

func (c *laggingCache) refreshLater(api client.Client, key client.ObjectKey) {
	time.AfterFunc(c.lag, func() {
		fresh := &profilingv1alpha1.ProfilingConfig{}
		err := api.Get(context.Background(), key, fresh)
		c.mu.Lock()
		defer c.mu.Unlock()
		switch {
		case err == nil:
			c.snap[key] = fresh
		case apierrors.IsNotFound(err):
			c.snap[key] = nil
		}
	})
}

// ---------------------------------------------------------------------------
// Fake S3
// ---------------------------------------------------------------------------

// s3Server is a minimal S3 endpoint accepting path-style PutObject requests.
type s3Server struct {
	srv *httptest.Server

	mu   sync.Mutex
	puts []s3Put
	// conns counts the TCP connections clients opened to the server.
	conns int
	// reject decides whether to fail a PutObject (with AccessDenied, which
	// the SDK does not retry). attempt counts earlier PUTs of the same pod and
	// profile type.
	reject func(pod, profileType string, attempt int) bool
}

type s3Put struct {
	key, pod, profileType string
	region                string // signing region, from the Authorization header
	ok                    bool
}

func newS3Server(t *testing.T) *s3Server {
	s := &s3Server{}
	s.srv = httptest.NewUnstartedServer(http.HandlerFunc(s.serve))
	s.srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			s.mu.Lock()
			s.conns++
			s.mu.Unlock()
		}
	}
	s.srv.Start()
	t.Cleanup(s.srv.Close)
	return s
}

// connections returns how many TCP connections clients have opened.
func (s *s3Server) connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns
}

// successfulPuts returns the successful PutObject requests so far.
func (s *s3Server) successfulPuts() []s3Put {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []s3Put
	for _, p := range s.puts {
		if p.ok {
			out = append(out, p)
		}
	}
	return out
}

// signingRegion extracts the region from a SigV4 Authorization header:
// "AWS4-HMAC-SHA256 Credential=<key>/<date>/<region>/s3/aws4_request, ...".
func signingRegion(authorization string) string {
	_, cred, ok := strings.Cut(authorization, "Credential=")
	if !ok {
		return ""
	}
	parts := strings.Split(strings.SplitN(cred, ",", 2)[0], "/")
	if len(parts) < 3 {
		return ""
	}
	return parts[2]
}

func (s *s3Server) serve(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPut {
		http.Error(w, "only PutObject is supported", http.StatusNotImplemented)
		return
	}
	_, _ = io.Copy(io.Discard, req.Body)
	put := s3Put{
		key:         strings.TrimPrefix(req.URL.Path, "/"),
		pod:         req.Header.Get("X-Amz-Meta-Pod-Name"),
		profileType: req.Header.Get("X-Amz-Meta-Profile-Type"),
		region:      signingRegion(req.Header.Get("Authorization")),
	}

	s.mu.Lock()
	attempt := 0
	for _, p := range s.puts {
		if p.pod == put.pod && p.profileType == put.profileType {
			attempt++
		}
	}
	put.ok = s.reject == nil || !s.reject(put.pod, put.profileType, attempt)
	s.puts = append(s.puts, put)
	s.mu.Unlock()

	if !put.ok {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>Access Denied</Message><RequestId>test</RequestId></Error>`)
		return
	}
	w.Header().Set("ETag", `"test-etag"`)
	w.WriteHeader(http.StatusOK)
}

// counts returns the number of PutObject requests and of successful ones.
func (s *s3Server) counts() (attempts, succeeded int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.puts {
		if p.ok {
			succeeded++
		}
	}
	return len(s.puts), succeeded
}

// TestMain points the AWS SDK at static test credentials for the whole test
// binary, and away from the host's AWS configuration, instance metadata and
// IRSA, so that end-to-end captures upload to the fake S3 endpoints with
// nothing leaking in from the environment. Setting this once, rather than
// with t.Setenv in each test, lets those tests run in parallel: nothing
// changes the environment while tests run.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "bolometer-test-aws")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	none := filepath.Join(dir, "none")
	for key, value := range map[string]string{
		"AWS_ACCESS_KEY_ID":           "test",
		"AWS_SECRET_ACCESS_KEY":       "test",
		"AWS_CONFIG_FILE":             none,
		"AWS_SHARED_CREDENTIALS_FILE": none,
		"AWS_EC2_METADATA_DISABLED":   "true",
	} {
		os.Setenv(key, value)
	}
	for _, key := range []string{"AWS_SESSION_TOKEN", "AWS_PROFILE", "AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE"} {
		os.Unsetenv(key)
	}

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
