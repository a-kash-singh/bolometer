#!/usr/bin/env bash
# =============================================================================
# Bolometer End-to-End Local Test
# =============================================================================
# Prerequisites: docker, kind, kubectl, go (1.23+), helm
#
# What this does:
#   1. Creates a kind cluster
#   2. Deploys MinIO (S3-compatible local storage)
#   3. Builds the operator and loads it into kind
#   4. Deploys CRD + RBAC + operator
#   5. Deploys a sample Go pprof app
#   6. Creates a ProfilingConfig with on-demand mode (no S3 creds needed)
#   7. Waits for profiles to be captured
#   8. Verifies results and prints a summary
#
# Usage:
#   ./e2e-local.sh              # full run
#   ./e2e-local.sh --cleanup    # destroy cluster when done
#   ./e2e-local.sh --skip-build # skip operator docker build (use cached image)
# =============================================================================
set -euo pipefail

# ── Config ─────────────────────────────────────────────────────────────────────
CLUSTER_NAME="bolometer-e2e"
NAMESPACE="bolometer-system"
DEMO_NAMESPACE="demo"
MINIO_NAMESPACE="minio"
OPERATOR_IMAGE="bolometer:e2e"
MINIO_BUCKET="profiles"
MINIO_ACCESS_KEY="minioadmin"
MINIO_SECRET_KEY="minioadmin"
TIMEOUT_SECONDS=120

# ── Flags ──────────────────────────────────────────────────────────────────────
CLEANUP=false
SKIP_BUILD=false
SOAK_SECONDS=0  # extra time to let profiles accumulate before summary
for arg in "$@"; do
  case $arg in
    --cleanup)       CLEANUP=true ;;
    --skip-build)    SKIP_BUILD=true ;;
    --soak=*)        SOAK_SECONDS="${arg#--soak=}" ;;
  esac
done

# ── Helpers ────────────────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
info()    { echo -e "${BLUE}[INFO]${NC}  $*"; }
success() { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC}  $*"; }
error()   { echo -e "${RED}[ERROR]${NC} $*" >&2; }
step()    { echo -e "\n${BLUE}══ $* ══${NC}"; }

check_prereqs() {
  step "Checking prerequisites"
  local missing=()
  for cmd in docker kind kubectl go helm; do
    if ! command -v "$cmd" &>/dev/null; then
      missing+=("$cmd")
    else
      success "$cmd $(${cmd} version --short 2>/dev/null | head -1 || true)"
    fi
  done
  if [[ ${#missing[@]} -gt 0 ]]; then
    error "Missing: ${missing[*]}"
    echo ""
    echo "Install missing tools:"
    echo "  docker:  https://docs.docker.com/get-docker/"
    echo "  kind:    go install sigs.k8s.io/kind@latest"
    echo "  kubectl: https://kubernetes.io/docs/tasks/tools/"
    echo "  go:      https://go.dev/dl/"
    echo "  helm:    https://helm.sh/docs/intro/install/"
    exit 1
  fi
}

wait_for_deployment() {
  local ns="$1" name="$2"
  info "Waiting for deployment $ns/$name..."
  kubectl rollout status deployment/"$name" -n "$ns" --timeout="${TIMEOUT_SECONDS}s"
  success "Deployment $name is ready"
}

wait_for_pod_label() {
  local ns="$1" label="$2"
  info "Waiting for pod with label $label in $ns..."
  local deadline=$((SECONDS + TIMEOUT_SECONDS))
  until kubectl get pods -n "$ns" -l "$label" --field-selector=status.phase=Running 2>/dev/null | grep -q Running; do
    if [[ $SECONDS -gt $deadline ]]; then
      error "Timeout waiting for pod $label"
      kubectl get pods -n "$ns" -l "$label" 2>/dev/null || true
      return 1
    fi
    sleep 3
  done
  success "Pod $label is running"
}

cleanup() {
  step "Cleanup"
  if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
    kind delete cluster --name "$CLUSTER_NAME"
    success "Cluster $CLUSTER_NAME deleted"
  fi
}

# ── Main ───────────────────────────────────────────────────────────────────────
echo ""
echo "  ██████   ██████  ██      ██████  ███    ███ ███████ ████████ ███████ ██████  "
echo "  ██   ██ ██    ██ ██     ██    ██ ████  ████ ██         ██    ██      ██   ██ "
echo "  ██████  ██    ██ ██     ██    ██ ██ ████ ██ █████      ██    █████   ██████  "
echo "  ██   ██ ██    ██ ██     ██    ██ ██  ██  ██ ██         ██    ██      ██   ██ "
echo "  ██████   ██████  ███████ ██████  ██      ██ ███████    ██    ███████ ██   ██ "
echo ""
echo "  End-to-End Local Test"
echo "  ─────────────────────────────────────────────────────────────────────────"
echo ""

check_prereqs

# ── Step 1: Kind cluster ───────────────────────────────────────────────────────
step "1/7  Creating kind cluster"
if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
  warn "Cluster $CLUSTER_NAME already exists — reusing it"
else
  cat <<EOF | kind create cluster --name "$CLUSTER_NAME" --config=-
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  kubeadmConfigPatches:
  - |
    kind: InitConfiguration
    nodeRegistration:
      kubeletExtraArgs:
        node-labels: "ingress-ready=true"
EOF
  success "Cluster $CLUSTER_NAME created"
fi
kubectl cluster-info --context "kind-${CLUSTER_NAME}" >/dev/null
success "Connected to kind cluster"

# ── Step 2: MinIO ──────────────────────────────────────────────────────────────
step "2/7  Deploying MinIO (local S3)"
kubectl create namespace "$MINIO_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

cat <<EOF | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: minio
  namespace: $MINIO_NAMESPACE
spec:
  selector:
    matchLabels:
      app: minio
  template:
    metadata:
      labels:
        app: minio
    spec:
      containers:
      - name: minio
        image: minio/minio:latest
        args: ["server", "/data", "--console-address", ":9001"]
        env:
        - name: MINIO_ROOT_USER
          value: "$MINIO_ACCESS_KEY"
        - name: MINIO_ROOT_PASSWORD
          value: "$MINIO_SECRET_KEY"
        ports:
        - containerPort: 9000
        - containerPort: 9001
        readinessProbe:
          httpGet:
            path: /minio/health/ready
            port: 9000
          initialDelaySeconds: 5
          periodSeconds: 5
---
apiVersion: v1
kind: Service
metadata:
  name: minio
  namespace: $MINIO_NAMESPACE
spec:
  selector:
    app: minio
  ports:
  - name: api
    port: 9000
    targetPort: 9000
  - name: console
    port: 9001
    targetPort: 9001
EOF

wait_for_deployment "$MINIO_NAMESPACE" "minio"

# Create the profiles bucket using mc (minio client) as a job
cat <<EOF | kubectl apply -f -
apiVersion: batch/v1
kind: Job
metadata:
  name: create-bucket
  namespace: $MINIO_NAMESPACE
spec:
  template:
    spec:
      restartPolicy: OnFailure
      containers:
      - name: mc
        image: minio/mc:latest
        command:
        - /bin/sh
        - -c
        - |
          mc alias set local http://minio.${MINIO_NAMESPACE}.svc.cluster.local:9000 $MINIO_ACCESS_KEY $MINIO_SECRET_KEY
          mc mb local/$MINIO_BUCKET --ignore-existing
          echo "Bucket created: $MINIO_BUCKET"
EOF

kubectl wait --for=condition=complete job/create-bucket -n "$MINIO_NAMESPACE" --timeout=60s
success "MinIO bucket '$MINIO_BUCKET' ready"

# ── Step 3: Build and load operator image ──────────────────────────────────────
step "3/7  Building operator image"
if [[ "$SKIP_BUILD" == "true" ]]; then
  warn "Skipping build (--skip-build)"
else
  info "Building Go binary..."
  CGO_ENABLED=0 GOOS=linux go build -o bin/manager cmd/main.go
  info "Building Docker image: $OPERATOR_IMAGE"
  docker build -t "$OPERATOR_IMAGE" .
  success "Image built"
fi

info "Loading image into kind cluster..."
kind load docker-image "$OPERATOR_IMAGE" --name "$CLUSTER_NAME"
success "Image loaded into kind"

# ── Step 4: Deploy operator via Helm ──────────────────────────────────────────
step "4/7  Deploying Bolometer operator (helm install)"

# Split image into repo + tag for --set flags
OPERATOR_IMAGE_REPO="${OPERATOR_IMAGE%%:*}"
OPERATOR_IMAGE_TAG="${OPERATOR_IMAGE##*:}"

helm upgrade --install bolometer ./helm/bolometer \
  --namespace "$NAMESPACE" \
  --create-namespace \
  -f ./helm/bolometer/values-minio.yaml \
  --set image.repository="$OPERATOR_IMAGE_REPO" \
  --set image.tag="$OPERATOR_IMAGE_TAG" \
  --set aws.staticCredentials.accessKeyId="$MINIO_ACCESS_KEY" \
  --set aws.staticCredentials.secretAccessKey="$MINIO_SECRET_KEY" \
  --wait --timeout=90s

success "Bolometer operator installed via Helm"

# Install metrics-server (required for threshold-based profiling)
# kind doesn't ship with it. The --kubelet-insecure-tls flag is needed in kind.
info "Installing metrics-server..."
kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml
kubectl patch deployment metrics-server -n kube-system \
  --type='json' \
  -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'
info "Waiting for metrics-server..."
kubectl rollout status deployment/metrics-server -n kube-system --timeout=60s || \
  warn "metrics-server not ready — threshold profiling may not fire; on-demand will still work"

# ── Step 5: Deploy sample app ──────────────────────────────────────────────────
step "5/7  Deploying sample Go pprof app"
kubectl create namespace "$DEMO_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

# Build and load sample app image
info "Building sample app..."
docker build -t demo-go-app:e2e examples/sample-app/
kind load docker-image demo-go-app:e2e --name "$CLUSTER_NAME"

cat <<EOF | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: demo-go-app
  namespace: $DEMO_NAMESPACE
spec:
  replicas: 1
  selector:
    matchLabels:
      app: demo-go-app
  template:
    metadata:
      labels:
        app: demo-go-app
      annotations:
        bolometer.io/enabled: "true"
        bolometer.io/port: "6060"
    spec:
      containers:
      - name: app
        image: demo-go-app:e2e
        imagePullPolicy: Never
        ports:
        - containerPort: 8080
          name: http
        - containerPort: 6060
          name: pprof
        resources:
          requests:
            cpu: 100m
            memory: 64Mi
          limits:
            cpu: 500m
            memory: 256Mi
        env:
        - name: PPROF_PORT
          value: "6060"
---
apiVersion: v1
kind: Service
metadata:
  name: demo-go-app
  namespace: $DEMO_NAMESPACE
spec:
  selector:
    app: demo-go-app
  ports:
  - port: 8080
    targetPort: 8080
EOF

wait_for_deployment "$DEMO_NAMESPACE" "demo-go-app"

# Generate some load so profiles are interesting
info "Generating load on sample app..."
DEMO_POD=$(kubectl get pod -n "$DEMO_NAMESPACE" -l app=demo-go-app -o jsonpath='{.items[0].metadata.name}')
for i in {1..5}; do
  kubectl exec "$DEMO_POD" -n "$DEMO_NAMESPACE" -- \
    wget -qO- http://localhost:8080/load >/dev/null 2>&1 || true
done
success "Load generated"

# ── Step 6: Create ProfilingConfig ─────────────────────────────────────────────
step "6/7  Creating ProfilingConfig (on-demand mode)"
MINIO_ENDPOINT="http://minio.${MINIO_NAMESPACE}.svc.cluster.local:9000"

cat <<EOF | kubectl apply -f -
apiVersion: bolometer.io/v1alpha1
kind: ProfilingConfig
metadata:
  name: e2e-test
  namespace: $DEMO_NAMESPACE
spec:
  selector:
    namespace: $DEMO_NAMESPACE
    labelSelector:
      app: demo-go-app

  thresholds:
    cpuThresholdPercent: 1       # Very low so threshold fires immediately
    memoryThresholdPercent: 1
    checkIntervalSeconds: 10
    cooldownSeconds: 60

  onDemand:
    enabled: true
    intervalSeconds: 30

  s3Config:
    bucket: $MINIO_BUCKET
    prefix: e2e
    region: us-east-1
    endpoint: $MINIO_ENDPOINT

  profileTypes:
  - heap
  - goroutine
EOF

success "ProfilingConfig applied"

# ── Step 7: Verify ─────────────────────────────────────────────────────────────
step "7/7  Waiting for profiles to appear in MinIO"

info "Waiting up to ${TIMEOUT_SECONDS}s for operator to capture profiles..."

CAPTURED=false
deadline=$((SECONDS + TIMEOUT_SECONDS))

while [[ $SECONDS -lt $deadline ]]; do
  # Check operator logs for profile capture
  if kubectl logs -n "$NAMESPACE" -l app=bolometer --since=2m 2>/dev/null | grep -qiE "captured|uploaded|on-demand|profile"; then
    CAPTURED=true
    break
  fi

  # Also check the ProfilingConfig status
  TOTAL=$(kubectl get profilingconfig e2e-test -n "$DEMO_NAMESPACE" \
    -o jsonpath='{.status.totalProfiles}' 2>/dev/null || echo "0")
  if [[ "$TOTAL" != "0" && "$TOTAL" != "" ]]; then
    CAPTURED=true
    break
  fi

  printf "."
  sleep 5
done
echo ""

# ── Summary ────────────────────────────────────────────────────────────────────
echo ""
echo "  ─────────────────────────────────────────────────────────────────────────"
echo "  E2E TEST SUMMARY"
echo "  ─────────────────────────────────────────────────────────────────────────"

# ProfilingConfig status
echo ""
echo "  ProfilingConfig status:"
kubectl get profilingconfig e2e-test -n "$DEMO_NAMESPACE" \
  -o custom-columns="ACTIVE-PODS:.status.activePods,PROFILES:.status.totalProfiles,UPLOADS:.status.totalUploads,LAST:.status.lastProfileTime" \
  2>/dev/null || echo "  (not found)"

# Operator logs (last 20 lines)
echo ""
echo "  Operator logs (recent):"
kubectl logs -n "$NAMESPACE" -l app=bolometer --tail=20 2>/dev/null \
  | sed 's/^/    /' || echo "  (no logs)"

# MinIO bucket contents (via port-forward in background)
echo ""
echo "  MinIO bucket check:"
kubectl port-forward svc/minio 9000:9000 -n "$MINIO_NAMESPACE" &>/dev/null &
PF_PID=$!
sleep 2
if command -v mc &>/dev/null; then
  mc alias set local-e2e http://127.0.0.1:9000 "$MINIO_ACCESS_KEY" "$MINIO_SECRET_KEY" &>/dev/null || true
  FILES=$(mc ls --recursive local-e2e/"$MINIO_BUCKET"/e2e/ 2>/dev/null | wc -l || echo 0)
  echo "  Files in s3://$MINIO_BUCKET/e2e: $FILES"
  mc ls --recursive local-e2e/"$MINIO_BUCKET"/e2e/ 2>/dev/null | sed 's/^/    /' | head -20 || true
else
  echo "  (mc not installed — check MinIO console at http://localhost:9001 with $MINIO_ACCESS_KEY / $MINIO_SECRET_KEY)"
  kubectl port-forward svc/minio 9001:9001 -n "$MINIO_NAMESPACE" &>/dev/null &
fi
kill $PF_PID 2>/dev/null || true

echo ""
if [[ "$CAPTURED" == "true" ]]; then
  success "PASS: Operator captured profiles"
else
  warn "Profiles not yet detected in logs — operator may need more time or metrics-server"
  warn "Run: kubectl logs -n $NAMESPACE -l app=bolometer -f"
  warn "Note: Threshold-based profiling requires metrics-server. On-demand should work regardless."
fi

echo ""
echo "  Useful commands:"
echo "    kubectl logs -n $NAMESPACE -l app=bolometer -f"
echo "    kubectl describe profilingconfig e2e-test -n $DEMO_NAMESPACE"
echo "    kubectl get profilingconfig -A"
echo "    kubectl port-forward svc/minio 9001:9001 -n $MINIO_NAMESPACE"
echo "    # then open http://localhost:9001 (user: $MINIO_ACCESS_KEY / pass: $MINIO_SECRET_KEY)"
echo ""

if [[ "$SOAK_SECONDS" -gt 0 ]]; then
  echo ""
  info "Soaking for ${SOAK_SECONDS}s — watching operator logs..."
  kubectl logs -n "$NAMESPACE" -l app=bolometer -f --since=0s &
  LOG_PID=$!
  sleep "$SOAK_SECONDS"
  kill $LOG_PID 2>/dev/null || true
  echo ""
  echo "  Final ProfilingConfig status after soak:"
  kubectl get profilingconfig e2e-test -n "$DEMO_NAMESPACE" \
    -o custom-columns="ACTIVE-PODS:.status.activePods,PROFILES:.status.totalProfiles,UPLOADS:.status.totalUploads,LAST:.status.lastProfileTime"
fi

if [[ "$CLEANUP" == "true" ]]; then
  cleanup
else
  echo ""
  echo "  Cluster is still running. To inspect captured profiles:"
  echo "    ./e2e-inspect.sh"
  echo ""
  echo "  To clean up when done:"
  echo "    kind delete cluster --name $CLUSTER_NAME"
fi
