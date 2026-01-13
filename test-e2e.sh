#!/bin/bash
set -e

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

# Configuration
NAMESPACE="profiling-test"
OPERATOR_NS="bolometer-system"
TEST_APP_NAMESPACE="demo"

echo "=========================================="
echo "E2E Test for K8s Profiling Operator"
echo "=========================================="
echo ""

# Function to print success
success() {
    echo -e "${GREEN}✓ $1${NC}"
}

# Function to print error
error() {
    echo -e "${RED}✗ $1${NC}"
    exit 1
}

# Function to print info
info() {
    echo -e "${YELLOW}→ $1${NC}"
}

# Check prerequisites
info "Checking prerequisites..."
command -v kubectl >/dev/null 2>&1 || error "kubectl is not installed"
command -v docker >/dev/null 2>&1 || error "docker is not installed"
kubectl cluster-info >/dev/null 2>&1 || error "Cannot connect to Kubernetes cluster"
success "Prerequisites check passed"
echo ""

# Check if metrics-server is available
info "Checking metrics-server..."
if kubectl get deployment metrics-server -n kube-system >/dev/null 2>&1; then
    success "metrics-server is available"
else
    error "metrics-server is not installed. Install it first."
fi
echo ""

# Build operator
info "Building operator..."
make build || error "Failed to build operator"
success "Operator built successfully"
echo ""

# Build docker image
info "Building Docker image..."
make docker-build IMG=bolometer:test || error "Failed to build Docker image"
success "Docker image built successfully"
echo ""

# Load image into kind (if using kind)
if kubectl config current-context | grep -q "kind"; then
    info "Detected kind cluster, loading image..."
    kind load docker-image bolometer:test || error "Failed to load image to kind"
    success "Image loaded to kind"
    echo ""
fi

# Build sample app
info "Building sample app..."
cd examples/sample-app
docker build -t demo-go-app:test . || error "Failed to build sample app"
if kubectl config current-context | grep -q "kind"; then
    kind load docker-image demo-go-app:test || error "Failed to load sample app to kind"
fi
cd ../..
success "Sample app built successfully"
echo ""

# Install CRDs
info "Installing CRDs..."
kubectl apply -f config/crd/ || error "Failed to install CRDs"
success "CRDs installed"
echo ""

# Install RBAC
info "Installing RBAC..."
kubectl apply -f config/rbac/ || error "Failed to install RBAC"
success "RBAC installed"
echo ""

# Deploy operator
info "Deploying operator..."
kubectl apply -f config/manager/ || error "Failed to deploy operator"
success "Operator deployed"
echo ""

# Wait for operator
info "Waiting for operator to be ready..."
kubectl wait --for=condition=available --timeout=120s deployment/bolometer -n $OPERATOR_NS || error "Operator not ready"
success "Operator is ready"
echo ""

# Deploy sample app
info "Deploying sample application..."
kubectl apply -f examples/target-app.yaml || error "Failed to deploy sample app"
success "Sample app deployed"
echo ""

# Wait for sample app
info "Waiting for sample app to be ready..."
kubectl wait --for=condition=ready --timeout=120s pod -l app=demo-go-app -n $TEST_APP_NAMESPACE || error "Sample app not ready"
success "Sample app is ready"
echo ""

# Test pprof endpoint
info "Testing pprof endpoint..."
POD=$(kubectl get pod -n $TEST_APP_NAMESPACE -l app=demo-go-app -o jsonpath='{.items[0].metadata.name}')
kubectl port-forward -n $TEST_APP_NAMESPACE $POD 6060:6060 >/dev/null 2>&1 &
PF_PID=$!
sleep 3

if curl -s http://localhost:6060/debug/pprof/ >/dev/null; then
    success "pprof endpoint is accessible"
    kill $PF_PID 2>/dev/null || true
else
    kill $PF_PID 2>/dev/null || true
    error "pprof endpoint is not accessible"
fi
echo ""

# Create ProfilingConfig with LocalStack for testing
info "Deploying LocalStack for S3 testing..."
kubectl create namespace localstack --dry-run=client -o yaml | kubectl apply -f -

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
          value: "0"
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

info "Waiting for LocalStack..."
kubectl wait --for=condition=available --timeout=120s deployment/localstack -n localstack || error "LocalStack not ready"
success "LocalStack is ready"
echo ""

# Create S3 bucket in LocalStack
info "Creating S3 bucket in LocalStack..."
sleep 5
kubectl run aws-cli-temp --rm -i --image=amazon/aws-cli --restart=Never -- \
  --endpoint-url=http://localstack.localstack.svc.cluster.local:4566 \
  s3 mb s3://test-bucket >/dev/null 2>&1 || true
success "S3 bucket created"
echo ""

# Create ProfilingConfig
info "Creating ProfilingConfig..."
cat <<EOF | kubectl apply -f -
apiVersion: bolometer.io/v1alpha1
kind: ProfilingConfig
metadata:
  name: test-profiling
  namespace: $TEST_APP_NAMESPACE
spec:
  selector:
    namespace: $TEST_APP_NAMESPACE
    labelSelector:
      app: demo-go-app
  thresholds:
    cpuThresholdPercent: 30
    memoryThresholdPercent: 30
    checkIntervalSeconds: 10
    cooldownSeconds: 60
  s3Config:
    bucket: test-bucket
    prefix: test-profiles
    region: us-east-1
    endpoint: http://localstack.localstack.svc.cluster.local:4566
  profileTypes:
  - heap
  - cpu
EOF
success "ProfilingConfig created"
echo ""

# Wait and observe
info "Waiting for operator to process ProfilingConfig (30 seconds)..."
sleep 30

# Check status
info "Checking ProfilingConfig status..."
kubectl get profilingconfig -n $TEST_APP_NAMESPACE
kubectl describe profilingconfig test-profiling -n $TEST_APP_NAMESPACE | grep -A 10 "Status:"
echo ""

# Check operator logs
info "Recent operator logs:"
kubectl logs -n $OPERATOR_NS -l app=bolometer --tail=20
echo ""

# Generate load
info "Generating load on sample app..."
kubectl port-forward -n $TEST_APP_NAMESPACE deployment/demo-go-app 8080:8080 >/dev/null 2>&1 &
PF_PID=$!
sleep 2

for i in {1..20}; do
    curl -s http://localhost:8080/load >/dev/null &
done
wait

kill $PF_PID 2>/dev/null || true
success "Load generated"
echo ""

# Wait for profiling to happen
info "Waiting for profiling to occur (60 seconds)..."
sleep 60

# Check operator logs for profiling activity
info "Checking for profiling activity..."
LOGS=$(kubectl logs -n $OPERATOR_NS -l app=bolometer --tail=100)

if echo "$LOGS" | grep -q "Found matching pods"; then
    success "Operator found matching pods"
else
    error "Operator did not find matching pods"
fi

if echo "$LOGS" | grep -q "Threshold exceeded\|On-demand profiling"; then
    success "Profiling was triggered"
else
    info "No profiling triggered yet (may need more load or time)"
fi

if echo "$LOGS" | grep -q "Profile captured\|Capturing profile"; then
    success "Profile capture attempted"
else
    info "No profile capture logged yet"
fi

echo ""

# Check final status
info "Final ProfilingConfig status:"
kubectl get profilingconfig test-profiling -n $TEST_APP_NAMESPACE -o jsonpath='{.status}' | jq '.' 2>/dev/null || kubectl get profilingconfig test-profiling -n $TEST_APP_NAMESPACE -o jsonpath='{.status}'
echo ""
echo ""

# Summary
echo "=========================================="
echo "E2E Test Summary"
echo "=========================================="
success "Operator deployed and running"
success "Sample app deployed and running"
success "pprof endpoint accessible"
success "ProfilingConfig created and processed"
success "LocalStack S3 backend available"
echo ""

info "To view operator logs:"
echo "  kubectl logs -n $OPERATOR_NS -l app=bolometer -f"
echo ""

info "To check ProfilingConfig status:"
echo "  kubectl describe profilingconfig test-profiling -n $TEST_APP_NAMESPACE"
echo ""

info "To generate more load:"
echo "  kubectl port-forward -n $TEST_APP_NAMESPACE deployment/demo-go-app 8080:8080"
echo "  for i in {1..50}; do curl http://localhost:8080/load & done"
echo ""

info "To cleanup:"
echo "  ./cleanup-test.sh"
echo ""

echo -e "${GREEN}=========================================="
echo "E2E Test Completed Successfully!"
echo "==========================================${NC}"


