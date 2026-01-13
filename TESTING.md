# Testing Guide for Profiling Operator

This guide covers various testing approaches for the K8s Profiling Operator.

## Table of Contents

1. [Prerequisites](#prerequisites)
2. [Unit Testing](#unit-testing)
3. [Local Testing](#local-testing)
4. [Integration Testing](#integration-testing)
5. [End-to-End Testing](#end-to-end-testing)
6. [Testing Checklist](#testing-checklist)

## Prerequisites

### Required Tools

```bash
# Go 1.23+
go version

# Docker
docker version

# Kubernetes cluster (choose one):
# - kind (Kubernetes in Docker)
# - minikube
# - k3d
# - EKS/GKE/AKS

# kubectl
kubectl version

# AWS CLI (for S3 testing)
aws --version

# Optional: helm
helm version
```

### Install kind (for local testing)

```bash
# macOS
brew install kind

# Linux
curl -Lo ./kind https://kind.sigs.k8s.io/dl/v0.20.0/kind-linux-amd64
chmod +x ./kind
sudo mv ./kind /usr/local/bin/kind

# Create cluster
kind create cluster --name profiling-test
```

### Install metrics-server

```bash
# For kind/minikube (with TLS disabled)
kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml

# Patch for local testing (disables TLS verification)
kubectl patch deployment metrics-server -n kube-system --type='json' \
  -p='[{"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--kubelet-insecure-tls"}]'

# Wait for metrics-server to be ready
kubectl wait --for=condition=available --timeout=300s deployment/metrics-server -n kube-system

# Verify
kubectl top nodes
```

## Unit Testing

### Run Unit Tests

```bash
cd /Users/akashsingh/Code/k8s-operator

# Run all tests
go test ./... -v

# Run with coverage
go test ./... -coverprofile=coverage.out
go tool cover -html=coverage.out -o coverage.html

# Run specific package
go test ./internal/metrics -v
go test ./internal/uploader -v
go test ./internal/profiler -v

# Run with race detector
go test ./... -race
```

### Create Unit Tests

Create `internal/metrics/collector_test.go`:

```go
package metrics

import (
	"testing"
	
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/metrics/pkg/apis/metrics/v1beta1"
)

func TestCheckThresholds(t *testing.T) {
	tests := []struct {
		name           string
		cpuPercent     float64
		memPercent     float64
		cpuThreshold   int
		memThreshold   int
		expectExceeded bool
	}{
		{
			name:           "CPU exceeds threshold",
			cpuPercent:     85,
			memPercent:     50,
			cpuThreshold:   80,
			memThreshold:   90,
			expectExceeded: true,
		},
		{
			name:           "Memory exceeds threshold",
			cpuPercent:     50,
			memPercent:     95,
			cpuThreshold:   80,
			memThreshold:   90,
			expectExceeded: true,
		},
		{
			name:           "Within thresholds",
			cpuPercent:     70,
			memPercent:     80,
			cpuThreshold:   80,
			memThreshold:   90,
			expectExceeded: false,
		},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pm := &PodMetrics{
				CPUUsagePercent:    tt.cpuPercent,
				MemoryUsagePercent: tt.memPercent,
			}
			
			exceeded, _ := pm.CheckThresholds(tt.cpuThreshold, tt.memThreshold)
			
			if exceeded != tt.expectExceeded {
				t.Errorf("expected exceeded=%v, got %v", tt.expectExceeded, exceeded)
			}
		})
	}
}
```

## Local Testing

### Option 1: Run Operator Outside Cluster

This is the fastest way to test during development.

```bash
# Set kubeconfig
export KUBECONFIG=~/.kube/config

# Run operator locally
cd /Users/akashsingh/Code/k8s-operator
go run cmd/main.go

# In another terminal, install CRDs
kubectl apply -f config/crd/

# Deploy test application
kubectl apply -f examples/target-app.yaml

# Create ProfilingConfig
cat <<EOF | kubectl apply -f -
apiVersion: profiling.io/v1alpha1
kind: ProfilingConfig
metadata:
  name: test-profiling
  namespace: demo
spec:
  selector:
    namespace: demo
    labelSelector:
      app: demo-go-app
  thresholds:
    cpuThresholdPercent: 50  # Low threshold for testing
    memoryThresholdPercent: 50
    checkIntervalSeconds: 10
    cooldownSeconds: 60
  s3Config:
    bucket: test-profiling-bucket
    prefix: test
    region: us-west-2
  profileTypes:
  - heap
  - cpu
EOF
```

### Option 2: Run in Cluster with LocalStack (S3 Mock)

For testing without AWS:

```bash
# Deploy LocalStack for S3 mocking
kubectl create namespace localstack

cat <<EOF | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: localstack
  namespace: localstack
spec:
  replicas: 1
  selector:
    matchLabels:
      app: localstack
  template:
    metadata:
      labels:
        app: localstack
    spec:
      containers:
      - name: localstack
        image: localstack/localstack:latest
        ports:
        - containerPort: 4566
        env:
        - name: SERVICES
          value: s3
        - name: DEBUG
          value: "1"
---
apiVersion: v1
kind: Service
metadata:
  name: localstack
  namespace: localstack
spec:
  selector:
    app: localstack
  ports:
  - port: 4566
    targetPort: 4566
EOF

# Wait for LocalStack
kubectl wait --for=condition=available --timeout=300s deployment/localstack -n localstack

# Create S3 bucket in LocalStack
kubectl run aws-cli --rm -i --tty --image=amazon/aws-cli -- \
  --endpoint-url=http://localstack.localstack.svc.cluster.local:4566 \
  s3 mb s3://test-bucket

# Update ProfilingConfig to use LocalStack
# Set endpoint: http://localstack.localstack.svc.cluster.local:4566
```

## Integration Testing

### Deploy Sample Application

Build and deploy the sample Go app:

```bash
cd /Users/akashsingh/Code/k8s-operator/examples/sample-app

# Build image
docker build -t demo-go-app:test .

# Load into kind
kind load docker-image demo-go-app:test --name profiling-test

# Deploy
kubectl apply -f ../target-app.yaml

# Verify pod is running
kubectl get pods -n demo
kubectl logs -n demo -l app=demo-go-app
```

### Test pprof Endpoint

```bash
# Port-forward to test app
kubectl port-forward -n demo deployment/demo-go-app 6060:6060 &

# Test pprof endpoints
curl http://localhost:6060/debug/pprof/
curl http://localhost:6060/debug/pprof/heap -o heap.pprof
curl http://localhost:6060/debug/pprof/profile?seconds=5 -o cpu.pprof

# Verify profiles
file heap.pprof
file cpu.pprof

# Kill port-forward
pkill -f "port-forward.*6060"
```

### Deploy Operator

```bash
cd /Users/akashsingh/Code/k8s-operator

# Build operator image
make docker-build IMG=profiling-operator:test

# Load into kind
kind load docker-image profiling-operator:test --name profiling-test

# Install CRDs
kubectl apply -f config/crd/

# Install RBAC
kubectl apply -f config/rbac/

# Update deployment image
kubectl apply -f config/manager/

# Check operator logs
kubectl logs -n profiling-system -l app=profiling-operator -f
```

## End-to-End Testing

### Complete Test Workflow

#### 1. Setup Test Environment

```bash
# Create test namespace
kubectl create namespace profiling-test

# Deploy sample app with annotation
cat <<EOF | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-app
  namespace: profiling-test
spec:
  replicas: 2
  selector:
    matchLabels:
      app: test-app
  template:
    metadata:
      labels:
        app: test-app
        app.kubernetes.io/name: test-app
      annotations:
        profiling.io/enabled: "true"
        profiling.io/port: "6060"
    spec:
      containers:
      - name: app
        image: demo-go-app:test
        resources:
          requests:
            cpu: 100m
            memory: 128Mi
          limits:
            cpu: 500m
            memory: 512Mi
        ports:
        - containerPort: 8080
        - containerPort: 6060
EOF

# Wait for pods
kubectl wait --for=condition=ready --timeout=120s pod -l app=test-app -n profiling-test
```

#### 2. Configure S3 (or LocalStack)

For AWS:
```bash
# Create S3 bucket
aws s3 mb s3://profiling-operator-test --region us-west-2

# Set up IRSA (if on EKS)
# See docs/IRSA_SETUP.md
```

For LocalStack:
```bash
# Already deployed above
# Create bucket
kubectl run aws-cli --rm -i --tty --image=amazon/aws-cli -- \
  --endpoint-url=http://localstack.localstack.svc.cluster.local:4566 \
  s3 mb s3://test-bucket
```

#### 3. Create ProfilingConfig

```bash
cat <<EOF | kubectl apply -f -
apiVersion: profiling.io/v1alpha1
kind: ProfilingConfig
metadata:
  name: test-config
  namespace: profiling-test
spec:
  selector:
    namespace: profiling-test
    labelSelector:
      app: test-app
  thresholds:
    cpuThresholdPercent: 30  # Low for easier triggering
    memoryThresholdPercent: 30
    checkIntervalSeconds: 10
    cooldownSeconds: 60
  s3Config:
    bucket: test-bucket
    prefix: test-profiles
    region: us-west-2
    # For LocalStack:
    endpoint: http://localstack.localstack.svc.cluster.local:4566
  profileTypes:
  - heap
  - cpu
EOF
```

#### 4. Generate Load

```bash
# Port-forward to app
kubectl port-forward -n profiling-test deployment/test-app 8080:8080 &

# Generate CPU/memory load
for i in {1..50}; do
  curl http://localhost:8080/load &
done

# Wait a bit
sleep 5

# Kill background jobs
pkill -f "port-forward.*8080"
jobs -l | awk '{print $2}' | xargs kill 2>/dev/null || true
```

#### 5. Verify Profiling

```bash
# Check ProfilingConfig status
kubectl get profilingconfig -n profiling-test
kubectl describe profilingconfig test-config -n profiling-test

# Check operator logs
kubectl logs -n profiling-system -l app=profiling-operator --tail=100

# Look for:
# - "Found matching pods"
# - "Threshold exceeded"
# - "Capturing profile"
# - "Profile captured"
# - "Uploaded to S3"

# Check metrics
kubectl top pods -n profiling-test

# Verify S3 uploads
aws s3 ls s3://test-bucket/test-profiles/ --recursive
# or for LocalStack:
kubectl run aws-cli --rm -i --tty --image=amazon/aws-cli -- \
  --endpoint-url=http://localstack.localstack.svc.cluster.local:4566 \
  s3 ls s3://test-bucket/test-profiles/ --recursive
```

#### 6. Test On-Demand Profiling

```bash
# Update ProfilingConfig to enable on-demand
kubectl patch profilingconfig test-config -n profiling-test --type=merge -p '
{
  "spec": {
    "onDemand": {
      "enabled": true,
      "intervalSeconds": 35
    }
  }
}'

# Wait and observe logs
kubectl logs -n profiling-system -l app=profiling-operator -f

# Should see:
# - "On-demand profiling" messages every ~35 seconds
# - Regular profile captures and uploads

# Wait 2 minutes, then check S3
sleep 120
aws s3 ls s3://test-bucket/test-profiles/ --recursive
```

#### 7. Test Profile Download and Analysis

```bash
# Download profiles
TODAY=$(date +%Y-%m-%d)
aws s3 sync s3://test-bucket/test-profiles/${TODAY}/ ./test-profiles/

# List downloaded profiles
ls -lh test-profiles/

# Analyze with pprof
cd test-profiles/test-app/
go tool pprof -top *-heap.pprof
go tool pprof -top *-cpu.pprof

# Interactive analysis
go tool pprof -http=:8081 *-cpu.pprof
# Open browser to http://localhost:8081
```

## Testing Checklist

### Functional Testing

- [ ] Operator starts successfully
- [ ] CRDs are installed correctly
- [ ] Pod discovery works with annotations
- [ ] Label selector filtering works
- [ ] Metrics collection from metrics-server works
- [ ] CPU threshold detection works
- [ ] Memory threshold detection works
- [ ] Cooldown period is respected
- [ ] pprof endpoint connection works
- [ ] Heap profile capture works
- [ ] CPU profile capture works
- [ ] Goroutine profile capture works
- [ ] Mutex profile capture works
- [ ] S3 upload works
- [ ] S3 key structure is correct (date/service-name)
- [ ] Service name extraction from labels works
- [ ] Metadata tags are added correctly
- [ ] On-demand profiling works
- [ ] On-demand interval is respected
- [ ] Status updates in CRD work
- [ ] Multiple pods can be profiled
- [ ] Multiple ProfilingConfigs can coexist

### Error Handling

- [ ] Handles missing pprof endpoint gracefully
- [ ] Handles S3 upload failures
- [ ] Handles metrics-server unavailable
- [ ] Handles pod deletion during profiling
- [ ] Handles invalid configuration
- [ ] Handles port-forward failures
- [ ] Logs errors appropriately
- [ ] Emits Kubernetes events

### Performance

- [ ] Low CPU usage when idle
- [ ] Reasonable memory usage
- [ ] Doesn't overwhelm metrics-server
- [ ] Doesn't create too many goroutines
- [ ] Cooldown prevents excessive profiling

### Security

- [ ] RBAC permissions are minimal
- [ ] IRSA authentication works
- [ ] No credentials in logs
- [ ] Runs as non-root user
- [ ] Capabilities are dropped

## Automated Testing Script

Create `test-e2e.sh`:

```bash
#!/bin/bash
set -e

echo "=== E2E Test for Profiling Operator ==="

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
NC='\033[0m'

# Configuration
NAMESPACE="profiling-test"
OPERATOR_NS="profiling-system"

echo "1. Creating test namespace..."
kubectl create namespace $NAMESPACE --dry-run=client -o yaml | kubectl apply -f -

echo "2. Deploying sample app..."
kubectl apply -f examples/target-app.yaml

echo "3. Waiting for pods..."
kubectl wait --for=condition=ready --timeout=120s pod -l app=demo-go-app -n demo

echo "4. Testing pprof endpoint..."
POD=$(kubectl get pod -n demo -l app=demo-go-app -o jsonpath='{.items[0].metadata.name}')
kubectl port-forward -n demo $POD 6060:6060 &
PF_PID=$!
sleep 2
if curl -s http://localhost:6060/debug/pprof/ > /dev/null; then
    echo -e "${GREEN}✓ pprof endpoint accessible${NC}"
else
    echo -e "${RED}✗ pprof endpoint not accessible${NC}"
    kill $PF_PID
    exit 1
fi
kill $PF_PID

echo "5. Installing operator..."
kubectl apply -f config/crd/
kubectl apply -f config/rbac/
kubectl apply -f config/manager/

echo "6. Waiting for operator..."
kubectl wait --for=condition=available --timeout=300s deployment/profiling-operator -n $OPERATOR_NS

echo "7. Creating ProfilingConfig..."
kubectl apply -f config/samples/profiling_v1alpha1_profilingconfig.yaml

echo "8. Checking status..."
sleep 30
kubectl get profilingconfig -A
kubectl describe profilingconfig -n default

echo "9. Checking operator logs..."
kubectl logs -n $OPERATOR_NS -l app=profiling-operator --tail=50

echo -e "${GREEN}=== E2E Test Complete ===${NC}"
echo "Check S3 bucket for uploaded profiles"
```

Make it executable and run:

```bash
chmod +x test-e2e.sh
./test-e2e.sh
```

## Troubleshooting Tests

### Common Issues

**Metrics not available:**
```bash
# Check metrics-server
kubectl get deployment metrics-server -n kube-system
kubectl logs -n kube-system deployment/metrics-server

# For kind/minikube, ensure insecure TLS is enabled
```

**Profiles not captured:**
```bash
# Check pprof endpoint manually
kubectl port-forward <pod> 6060:6060
curl http://localhost:6060/debug/pprof/

# Check operator has permission
kubectl auth can-i create pods/portforward --as=system:serviceaccount:profiling-system:profiling-operator
```

**S3 upload fails:**
```bash
# Check credentials (IRSA)
kubectl get sa profiling-operator -n profiling-system -o yaml

# Check operator logs for AWS errors
kubectl logs -n profiling-system -l app=profiling-operator | grep -i s3
```

## Cleanup

```bash
# Delete test resources
kubectl delete namespace profiling-test
kubectl delete namespace demo
kubectl delete namespace localstack

# Delete operator
kubectl delete -f config/manager/
kubectl delete -f config/rbac/
kubectl delete -f config/crd/

# Delete kind cluster
kind delete cluster --name profiling-test
```

## Continuous Integration

Example GitHub Actions workflow:

```yaml
name: Test

on: [push, pull_request]

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
    - uses: actions/checkout@v3
    
    - name: Set up Go
      uses: actions/setup-go@v4
      with:
        go-version: '1.23'
    
    - name: Run unit tests
      run: make test
    
    - name: Build
      run: make build
    
    - name: Docker build
      run: make docker-build IMG=profiling-operator:test
```

## Next Steps

- Add more unit tests for edge cases
- Create integration test suite
- Set up CI/CD pipeline
- Add performance benchmarks
- Create load testing scenarios


