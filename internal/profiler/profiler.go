package profiler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

const (
	// DefaultPprofPort is the default pprof port
	DefaultPprofPort = 6060

	// PprofPortAnnotation is the annotation key for custom pprof port
	PprofPortAnnotation = "bolometer.io/port"

	// portForwardReadyTimeout bounds how long to wait for a port-forward to
	// become ready.
	portForwardReadyTimeout = 10 * time.Second
)

// Profiler captures pprof profiles from Go applications
type Profiler struct {
	clientset  kubernetes.Interface
	restConfig *rest.Config
}

// NewProfiler creates a new profiler
func NewProfiler(clientset kubernetes.Interface, restConfig *rest.Config) *Profiler {
	return &Profiler{
		clientset:  clientset,
		restConfig: restConfig,
	}
}

// Profile represents a captured profile
type Profile struct {
	Type      string
	Data      []byte
	Timestamp time.Time
}

// CaptureProfiles captures the specified profile types from a pod.
//
// Each type is tried once, in order. A type that fails does not stop the
// others: the profiles that were captured are returned together with an
// error naming the types that failed. Callers must therefore use the
// returned profiles even when the error is non-nil. If the port-forward
// cannot be set up, nothing is captured.
//
// If a request fails below HTTP (connection refused, reset, EOF), the next
// type gets a fresh port-forward: when the pod side reports an error,
// client-go tears down the whole port-forward, and a request sent over it
// meanwhile can hang until client-go's 30s stream timeout. An HTTP error from
// the pprof endpoint leaves the connection in use.
func (p *Profiler) CaptureProfiles(ctx context.Context, pod *corev1.Pod, profileTypes []string) ([]Profile, error) {
	port := p.getPprofPort(pod)

	// Create port-forward to the pod. On success it is ready to use.
	localPort, stopChan, err := p.setupPortForward(ctx, pod, port, portForwardReadyTimeout)
	if err != nil {
		return nil, fmt.Errorf("failed to setup port forward: %w", err)
	}
	defer func() {
		if stopChan != nil {
			close(stopChan)
		}
	}()

	// Capture each profile type
	var profiles []Profile
	var errs []error
	for i, profileType := range profileTypes {
		if err := ctx.Err(); err != nil {
			// Cancelled or timed out: the remaining types would fail too.
			errs = append(errs, fmt.Errorf("%s not captured: %w", strings.Join(profileTypes[i:], ", "), err))
			break
		}
		if stopChan == nil {
			// The previous request lost its connection; forward anew.
			localPort, stopChan, err = p.setupPortForward(ctx, pod, port, portForwardReadyTimeout)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s not captured: failed to setup port forward: %w", strings.Join(profileTypes[i:], ", "), err))
				break
			}
		}
		profile, err := p.captureProfile(ctx, localPort, profileType)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to capture %s profile: %w", profileType, err))
			var status *statusError
			if !errors.As(err, &status) {
				close(stopChan)
				stopChan = nil
			}
			continue
		}
		profiles = append(profiles, profile)
	}

	return profiles, errors.Join(errs...)
}

// statusError is a pprof endpoint answering with a status other than 200 OK.
// The connection to the pod is fine.
type statusError struct {
	code int
}

func (e *statusError) Error() string {
	return fmt.Sprintf("unexpected status code: %d", e.code)
}

// setupPortForward creates a port-forward to the pod and waits until it is
// ready. It gives up when forwarding fails, readyTimeout elapses or ctx is
// cancelled. On success the caller must close the returned stop channel.
func (p *Profiler) setupPortForward(ctx context.Context, pod *corev1.Pod, remotePort int, readyTimeout time.Duration) (int, chan struct{}, error) {
	// Use a local port (0 means choose automatically)
	localPort := 0

	// Create the port-forward request
	req := p.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(pod.Namespace).
		Name(pod.Name).
		SubResource("portforward")

	transport, upgrader, err := spdy.RoundTripperFor(p.restConfig)
	if err != nil {
		return 0, nil, err
	}

	stopChan := make(chan struct{}, 1)
	readyChan := make(chan struct{})

	ports := []string{fmt.Sprintf("%d:%d", localPort, remotePort)}

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, req.URL())

	// The forwarder writes progress messages from several goroutines at once,
	// so the writers must be goroutine-safe (bytes.Buffer is not). Nothing
	// reads them; failures are reported by ForwardPorts and GetPorts.
	fw, err := portforward.New(dialer, ports, stopChan, readyChan, io.Discard, io.Discard)
	if err != nil {
		return 0, nil, err
	}

	// ForwardPorts returns without ever closing readyChan when the dial or
	// listen fails, so waiting on readyChan alone can block forever.
	forwardErr := make(chan error, 1)
	go func() {
		forwardErr <- fw.ForwardPorts()
	}()

	readyTimer := time.NewTimer(readyTimeout)
	defer readyTimer.Stop()

	select {
	case <-readyChan:
	case err := <-forwardErr:
		if err == nil {
			err = fmt.Errorf("port forward stopped before becoming ready")
		}
		close(stopChan)
		return 0, nil, err
	case <-readyTimer.C:
		close(stopChan)
		return 0, nil, fmt.Errorf("timeout waiting for port forward")
	case <-ctx.Done():
		close(stopChan)
		return 0, nil, ctx.Err()
	}

	// Get the actual local port that was chosen
	forwardedPorts, err := fw.GetPorts()
	if err != nil {
		close(stopChan)
		return 0, nil, err
	}

	if len(forwardedPorts) == 0 {
		close(stopChan)
		return 0, nil, fmt.Errorf("no ports forwarded")
	}

	actualLocalPort := int(forwardedPorts[0].Local)

	return actualLocalPort, stopChan, nil
}

// captureProfile captures a specific profile type
func (p *Profiler) captureProfile(ctx context.Context, localPort int, profileType string) (Profile, error) {
	endpoint := p.getProfileEndpoint(profileType)
	url := fmt.Sprintf("http://localhost:%d%s", localPort, endpoint)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return Profile{}, err
	}

	client := &http.Client{
		Timeout: 60 * time.Second, // CPU profiling can take up to 30 seconds
	}

	resp, err := client.Do(req)
	if err != nil {
		return Profile{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Profile{}, &statusError{code: resp.StatusCode}
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return Profile{}, err
	}

	return Profile{
		Type:      profileType,
		Data:      data,
		Timestamp: time.Now(),
	}, nil
}

// getProfileEndpoint returns the pprof endpoint for a profile type
func (p *Profiler) getProfileEndpoint(profileType string) string {
	switch profileType {
	case "heap":
		return "/debug/pprof/heap"
	case "cpu":
		return "/debug/pprof/profile?seconds=30"
	case "goroutine":
		return "/debug/pprof/goroutine"
	case "mutex":
		return "/debug/pprof/mutex"
	case "block":
		return "/debug/pprof/block"
	case "threadcreate":
		return "/debug/pprof/threadcreate"
	default:
		return fmt.Sprintf("/debug/pprof/%s", profileType)
	}
}

// getPprofPort gets the pprof port from pod annotations or uses default
func (p *Profiler) getPprofPort(pod *corev1.Pod) int {
	if pod.Annotations == nil {
		return DefaultPprofPort
	}

	portStr, ok := pod.Annotations[PprofPortAnnotation]
	if !ok {
		return DefaultPprofPort
	}

	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return DefaultPprofPort
	}

	return port
}
