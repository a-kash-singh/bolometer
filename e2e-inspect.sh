#!/usr/bin/env bash
# =============================================================================
# Bolometer E2E Inspector
# =============================================================================
# Connects to a running bolometer-e2e kind cluster (from e2e-local.sh) and:
#   1. Shows live operator logs while waiting for more profiles
#   2. Lists every captured profile in MinIO with size + metadata
#   3. Downloads one profile per type and validates the pprof binary format
#   4. Shows a final ProfilingConfig status summary
#
# Usage:
#   ./e2e-inspect.sh                     # watch for 3 minutes, then inspect
#   ./e2e-inspect.sh --watch=120         # watch for N seconds
#   ./e2e-inspect.sh --no-watch          # skip live tail, just inspect now
#   ./e2e-inspect.sh --generate-load     # hammer the /load endpoint while watching
# =============================================================================
set -euo pipefail

CLUSTER_NAME="bolometer-e2e"
NAMESPACE="bolometer-system"
DEMO_NAMESPACE="demo"
MINIO_NAMESPACE="minio"
MINIO_BUCKET="profiles"
MINIO_ACCESS_KEY="minioadmin"
MINIO_SECRET_KEY="minioadmin"
WATCH_SECONDS=180
GENERATE_LOAD=false

for arg in "$@"; do
  case $arg in
    --watch=*)       WATCH_SECONDS="${arg#--watch=}" ;;
    --no-watch)      WATCH_SECONDS=0 ;;
    --generate-load) GENERATE_LOAD=true ;;
  esac
done

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'
info()    { echo -e "${BLUE}[INFO]${NC}  $*"; }
success() { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC}  $*"; }
error()   { echo -e "${RED}[ERROR]${NC} $*" >&2; }
header()  { echo -e "\n${BOLD}${CYAN}$*${NC}"; echo -e "${CYAN}$(printf '─%.0s' {1..70})${NC}"; }

# ── Preflight ──────────────────────────────────────────────────────────────────
if ! kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
  error "Cluster '$CLUSTER_NAME' not found. Run ./e2e-local.sh first."
  exit 1
fi
kubectl config use-context "kind-${CLUSTER_NAME}" >/dev/null
success "Connected to kind-${CLUSTER_NAME}"

# ── Phase 1: Live watch + optional load generation ─────────────────────────────
if [[ "$WATCH_SECONDS" -gt 0 ]]; then
  header "Phase 1 — Watching operator for ${WATCH_SECONDS}s"
  echo "  (Ctrl-C is safe here — the cluster keeps running)"
  echo ""

  if [[ "$GENERATE_LOAD" == "true" ]]; then
    DEMO_POD=$(kubectl get pod -n "$DEMO_NAMESPACE" -l app=demo-go-app \
      -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
    if [[ -n "$DEMO_POD" ]]; then
      info "Generating continuous load on $DEMO_POD..."
      (
        while true; do
          kubectl exec "$DEMO_POD" -n "$DEMO_NAMESPACE" -- \
            wget -qO- http://localhost:8080/load >/dev/null 2>&1 || true
          sleep 2
        done
      ) &
      LOAD_PID=$!
      trap "kill $LOAD_PID 2>/dev/null || true" EXIT
    fi
  fi

  # Tail logs with color highlights
  kubectl logs -n "$NAMESPACE" -l app=bolometer -f --since=0s 2>/dev/null | \
    grep --line-buffered -E "INFO|ERROR|WARN|profile|threshold|upload|on-demand" | \
    while IFS= read -r line; do
      if echo "$line" | grep -qi "error"; then
        echo -e "${RED}$line${NC}"
      elif echo "$line" | grep -qi "profile\|upload\|threshold\|on-demand"; then
        echo -e "${GREEN}$line${NC}"
      else
        echo "$line"
      fi
    done &
  LOG_PID=$!

  sleep "$WATCH_SECONDS"
  kill $LOG_PID 2>/dev/null || true
  [[ "$GENERATE_LOAD" == "true" ]] && kill $LOAD_PID 2>/dev/null || true
  echo ""
fi

# ── Phase 2: ProfilingConfig status ───────────────────────────────────────────
header "Phase 2 — ProfilingConfig Status"

kubectl get profilingconfig -A \
  -o custom-columns="NAMESPACE:.metadata.namespace,NAME:.metadata.name,PODS:.status.activePods,PROFILES:.status.totalProfiles,UPLOADS:.status.totalUploads,LAST-PROFILE:.status.lastProfileTime"

echo ""
echo "Detailed status:"
kubectl describe profilingconfig e2e-test -n "$DEMO_NAMESPACE" 2>/dev/null | \
  grep -A 30 "^Status:" | head -40

# ── Phase 3: List profiles in MinIO ───────────────────────────────────────────
header "Phase 3 — Profiles in MinIO"

# Create a persistent mc pod (kubectl run --attach --rm is unreliable — pipe
# breaks before output arrives). Instead: create → wait for Running → exec → delete.
kubectl delete pod mc-inspect -n "$MINIO_NAMESPACE" --ignore-not-found >/dev/null 2>&1
kubectl run mc-inspect \
  --image=minio/mc:latest \
  --restart=Never \
  -n "$MINIO_NAMESPACE" \
  -- sleep 300 >/dev/null 2>&1

TOTAL_FILES=0
MC_POD_READY=false
info "Waiting for mc pod to start..."
if kubectl wait --for=condition=ready pod/mc-inspect -n "$MINIO_NAMESPACE" --timeout=30s >/dev/null 2>&1; then
  MC_POD_READY=true

  MC_OUTPUT=$(kubectl exec mc-inspect -n "$MINIO_NAMESPACE" -- sh -c "
    mc alias set local http://minio.${MINIO_NAMESPACE}.svc.cluster.local:9000 \
      ${MINIO_ACCESS_KEY} ${MINIO_SECRET_KEY} >/dev/null 2>&1
    echo '=== Files in bucket ==='
    mc ls --recursive local/${MINIO_BUCKET}/ 2>/dev/null || echo '(bucket empty)'
    echo ''
    echo '=== Disk usage by date ==='
    mc du --depth=2 local/${MINIO_BUCKET}/ 2>/dev/null || true
  " 2>/dev/null)

  echo "$MC_OUTPUT"

  TOTAL_FILES=$(echo "$MC_OUTPUT" | grep -c "\.pprof" || true)
  echo ""
  echo -e "  Total .pprof files captured: ${BOLD}${TOTAL_FILES}${NC}"
else
  warn "mc pod failed to start — skipping MinIO listing"
fi

# ── Phase 4: Download and validate profiles ───────────────────────────────────
header "Phase 4 — Download & Validate Profiles"

TMP_DIR=$(mktemp -d)
trap "rm -rf $TMP_DIR; kubectl delete pod mc-inspect -n $MINIO_NAMESPACE --ignore-not-found >/dev/null 2>&1" EXIT

if [[ "$MC_POD_READY" != "true" ]]; then
  warn "Skipping download — mc pod was not available."
else
  info "Listing profiles for download..."
  DOWNLOAD_OUTPUT=$(kubectl exec mc-inspect -n "$MINIO_NAMESPACE" -- sh -c "
    mc alias set local http://minio.${MINIO_NAMESPACE}.svc.cluster.local:9000 \
      ${MINIO_ACCESS_KEY} ${MINIO_SECRET_KEY} >/dev/null 2>&1
    mc ls --recursive local/${MINIO_BUCKET}/ 2>/dev/null | awk '{print \$NF}'
  " 2>/dev/null || echo "")

  if [[ -z "$DOWNLOAD_OUTPUT" ]]; then
    warn "No profiles found in MinIO yet."
    echo "  The operator may need more time. Run:"
    echo "    ./e2e-inspect.sh --no-watch --generate-load"
  else
    echo ""
    echo "  Validating pprof binary format..."
    echo ""
    printf "  %-60s %-8s %s\n" "FILE" "SIZE" "VALID"
    printf "  %-60s %-8s %s\n" "$(printf '─%.0s' {1..60})" "--------" "-------"

    VALID_COUNT=0
    INVALID_COUNT=0

    while IFS= read -r filepath; do
      [[ -z "$filepath" ]] && continue
      [[ "$filepath" != *.pprof ]] && continue

      # Download via exec into the already-running mc pod
      LOCAL_FILE="$TMP_DIR/$(basename "$filepath")"
      kubectl exec mc-inspect -n "$MINIO_NAMESPACE" -- sh -c "
        mc alias set local http://minio.${MINIO_NAMESPACE}.svc.cluster.local:9000 \
          ${MINIO_ACCESS_KEY} ${MINIO_SECRET_KEY} >/dev/null 2>&1
        mc cat local/${MINIO_BUCKET}/${filepath}
      " 2>/dev/null > "$LOCAL_FILE" || continue

      SIZE=$(wc -c < "$LOCAL_FILE" 2>/dev/null || echo 0)
      SIZE_HUMAN="${SIZE}B"
      if [[ $SIZE -gt 1024 ]]; then SIZE_HUMAN="$(( SIZE / 1024 ))KB"; fi

      # Go pprof binary format is gzip-compressed protobuf — magic bytes 1f 8b
      MAGIC=$(xxd -l 2 "$LOCAL_FILE" 2>/dev/null | awk '{print $2$3}' | head -1 || echo "")
      if [[ "$MAGIC" == "1f8b" ]] || [[ $SIZE -gt 100 ]]; then
        STATUS="${GREEN}✓ pprof${NC}"
        VALID_COUNT=$((VALID_COUNT + 1))
      else
        STATUS="${RED}✗ invalid (${MAGIC})${NC}"
        INVALID_COUNT=$((INVALID_COUNT + 1))
      fi

      printf "  %-60s %-8s " "$(basename "$filepath")" "$SIZE_HUMAN"
      echo -e "$STATUS"

    done <<< "$DOWNLOAD_OUTPUT"

    echo ""
    echo -e "  Valid: ${GREEN}${VALID_COUNT}${NC}  Invalid: ${RED}${INVALID_COUNT}${NC}"

    # If go is available, run pprof top on the first heap / goroutine profile
    if command -v go &>/dev/null; then
      HEAP_FILE=$(find "$TMP_DIR" -name "*heap*.pprof" | head -1)
      if [[ -n "$HEAP_FILE" ]]; then
        echo ""
        info "Running 'go tool pprof -top' on heap profile: $(basename "$HEAP_FILE")"
        go tool pprof -top "$HEAP_FILE" 2>/dev/null | head -20 || \
          warn "pprof analysis failed (profile may be too small)"
      fi

      GOROUTINE_FILE=$(find "$TMP_DIR" -name "*goroutine*.pprof" | head -1)
      if [[ -n "$GOROUTINE_FILE" ]]; then
        echo ""
        info "Running 'go tool pprof -top' on goroutine profile: $(basename "$GOROUTINE_FILE")"
        go tool pprof -top "$GOROUTINE_FILE" 2>/dev/null | head -20 || \
          warn "pprof analysis failed"
      fi
    else
      echo ""
      info "Install Go to run 'go tool pprof' analysis locally."
      echo "  Or copy a profile and analyze it:"
      echo "    go tool pprof -http=:8082 <downloaded.pprof>"
    fi
  fi
fi

# ── Phase 5: Summary ───────────────────────────────────────────────────────────
header "Summary"

TOTAL=$(kubectl get profilingconfig e2e-test -n "$DEMO_NAMESPACE" \
  -o jsonpath='{.status.totalProfiles}' 2>/dev/null || echo "0")
UPLOADS=$(kubectl get profilingconfig e2e-test -n "$DEMO_NAMESPACE" \
  -o jsonpath='{.status.totalUploads}' 2>/dev/null || echo "0")
LAST=$(kubectl get profilingconfig e2e-test -n "$DEMO_NAMESPACE" \
  -o jsonpath='{.status.lastProfileTime}' 2>/dev/null || echo "never")

echo ""
echo -e "  Profiles captured : ${BOLD}${TOTAL}${NC}"
echo -e "  Uploads to MinIO  : ${BOLD}${UPLOADS}${NC}"
echo -e "  Last profile at   : ${BOLD}${LAST}${NC}"
echo ""
echo "  Next steps:"
echo "    Browse MinIO UI:"
echo "      kubectl port-forward svc/minio 9001:9001 -n $MINIO_NAMESPACE &"
echo "      open http://localhost:9001  (user: $MINIO_ACCESS_KEY / pass: $MINIO_SECRET_KEY)"
echo ""
echo "    Watch live profiling:"
echo "      kubectl logs -n $NAMESPACE -l app=bolometer -f"
echo ""
echo "    Generate more load:"
echo "      POD=\$(kubectl get pod -n $DEMO_NAMESPACE -l app=demo-go-app -o jsonpath='{.items[0].metadata.name}')"
echo "      kubectl exec \$POD -n $DEMO_NAMESPACE -- wget -qO- http://localhost:8080/load"
echo ""
echo "    Run again with more soak time:"
echo "      ./e2e-inspect.sh --watch=300 --generate-load"
echo ""
echo "    Clean up:"
echo "      kind delete cluster --name $CLUSTER_NAME"
