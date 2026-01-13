package profiler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
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

// CaptureProfiles captures all specified profile types from a pod
func (p *Profiler) CaptureProfiles(ctx context.Context, pod *corev1.Pod, profileTypes []string) ([]Profile, error) {
	port := p.getPprofPort(pod)

	// Create port-forward to the pod
	localPort, stopChan, readyChan, err := p.setupPortForward(ctx, pod, port)
	if err != nil {
		return nil, fmt.Errorf("failed to setup port forward: %w", err)
	}
	defer close(stopChan)

	// Wait for port-forward to be ready
	select {
	case <-readyChan:
		// Port-forward is ready
	case <-time.After(10 * time.Second):
		return nil, fmt.Errorf("timeout waiting for port forward")
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// Capture each profile type
	var profiles []Profile
	for _, profileType := range profileTypes {
		profile, err := p.captureProfile(ctx, localPort, profileType)
		if err != nil {
			return nil, fmt.Errorf("failed to capture %s profile: %w", profileType, err)
		}
		profiles = append(profiles, profile)
	}

	return profiles, nil
}

// setupPortForward creates a port-forward to the pod
func (p *Profiler) setupPortForward(ctx context.Context, pod *corev1.Pod, remotePort int) (int, chan struct{}, chan struct{}, error) {
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
		return 0, nil, nil, err
	}

	stopChan := make(chan struct{}, 1)
	readyChan := make(chan struct{})

	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}

	ports := []string{fmt.Sprintf("%d:%d", localPort, remotePort)}

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, req.URL())

	fw, err := portforward.New(dialer, ports, stopChan, readyChan, out, errOut)
	if err != nil {
		return 0, nil, nil, err
	}

	go func() {
		if err := fw.ForwardPorts(); err != nil {
			// Log error but don't stop the operation
		}
	}()

	// Get the actual local port that was chosen
	<-readyChan
	forwardedPorts, err := fw.GetPorts()
	if err != nil {
		close(stopChan)
		return 0, nil, nil, err
	}

	if len(forwardedPorts) == 0 {
		close(stopChan)
		return 0, nil, nil, fmt.Errorf("no ports forwarded")
	}

	actualLocalPort := int(forwardedPorts[0].Local)

	return actualLocalPort, stopChan, readyChan, nil
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
		return Profile{}, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
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
