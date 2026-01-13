# Testing Summary

## Quick Test Commands

### Unit Tests

```bash
# Run all unit tests
go test ./...

# Run with coverage
go test ./... -coverprofile=coverage.out
go tool cover -html=coverage.out

# Run specific package tests
go test ./internal/metrics -v
go test ./internal/uploader -v

# Run with race detector
go test ./... -race
```

### Local E2E Testing

```bash
# Prerequisites: kind/minikube cluster with metrics-server

# Run automated E2E test
./test-e2e.sh

# Cleanup after testing
./cleanup-test.sh
```

### Manual Testing Steps

1. **Setup test cluster**
   ```bash
   kind create cluster --name profiling-test
   kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml
   kubectl patch deployment metrics-server -n kube-system --type='json' \
     -p='[{"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--kubelet-insecure-tls"}]'
   ```

2. **Build and deploy operator**
   ```bash
   make docker-build IMG=profiling-operator:test
   kind load docker-image profiling-operator:test --name profiling-test
   kubectl apply -f config/crd/
   kubectl apply -f config/rbac/
   kubectl apply -f config/manager/
   ```

3. **Deploy test application**
   ```bash
   cd examples/sample-app
   docker build -t demo-go-app:test .
   kind load docker-image demo-go-app:test --name profiling-test
   kubectl apply -f ../target-app.yaml
   ```

4. **Create ProfilingConfig**
   ```bash
   kubectl apply -f config/samples/profiling_v1alpha1_profilingconfig.yaml
   ```

5. **Generate load and verify**
   ```bash
   kubectl port-forward -n demo deployment/demo-go-app 8080:8080 &
   for i in {1..50}; do curl http://localhost:8080/load & done
   kubectl logs -n profiling-system -l app=profiling-operator -f
   ```

## Test Results

### Unit Tests Status

✓ **internal/metrics** - All tests passing
  - TestCheckThresholds: 5 test cases
  - TestCalculateMetrics: 3 test cases

✓ **internal/uploader** - All tests passing
  - TestGetServiceName: 7 test cases
  - TestGenerateKey: 1 test case
  - TestGenerateKeyDifferentDates: 3 test cases

### Integration Test Scenarios

The `test-e2e.sh` script tests:

✓ Operator deployment and readiness
✓ CRD installation
✓ RBAC configuration
✓ Sample app deployment with profiling annotation
✓ pprof endpoint accessibility
✓ LocalStack S3 mock setup
✓ ProfilingConfig creation and processing
✓ Load generation and profiling trigger

## Testing Checklist

### Functional Tests
- [x] Operator starts successfully
- [x] CRDs install correctly
- [x] Pod discovery with annotations
- [x] Service name extraction from labels
- [x] S3 key structure validation
- [x] Threshold calculation accuracy
- [x] Unit tests for core components

### Manual Verification Needed
- [ ] Metrics collection from metrics-server (requires live cluster)
- [ ] Profile capture from pprof endpoints (requires live cluster)
- [ ] Actual S3 upload with IRSA (requires AWS)
- [ ] On-demand profiling intervals (requires time)
- [ ] Cooldown period enforcement (requires time)
- [ ] Multiple pod profiling (requires live cluster)

## Common Test Issues

### Issue: metrics-server not available
**Solution:**
```bash
kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml
kubectl patch deployment metrics-server -n kube-system --type='json' \
  -p='[{"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--kubelet-insecure-tls"}]'
```

### Issue: pprof endpoint not accessible
**Solution:** Ensure your Go app imports pprof:
```go
import _ "net/http/pprof"
```

### Issue: S3 upload fails in tests
**Solution:** Use LocalStack for local testing (included in test-e2e.sh)

## Next Steps

1. Run unit tests: `go test ./...`
2. Run E2E test: `./test-e2e.sh`
3. Test in real cluster with actual S3
4. Test with real production workloads
5. Monitor operator resource usage
6. Test failure scenarios
7. Test with multiple namespaces
8. Test with high pod counts

## Performance Testing

To test performance:

```bash
# Deploy multiple test apps
for i in {1..10}; do
  kubectl create namespace test-$i
  kubectl apply -f examples/target-app.yaml -n test-$i
done

# Create ProfilingConfig for each
for i in {1..10}; do
  cat <<EOF | kubectl apply -f -
apiVersion: profiling.io/v1alpha1
kind: ProfilingConfig
metadata:
  name: test-$i
  namespace: test-$i
spec:
  selector:
    namespace: test-$i
  thresholds:
    cpuThresholdPercent: 50
    memoryThresholdPercent: 50
  s3Config:
    bucket: test-bucket
    region: us-west-2
  profileTypes: [heap, cpu]
EOF
done

# Monitor operator resource usage
kubectl top pod -n profiling-system
```

## Continuous Integration

Example GitHub Actions:

```yaml
name: Test
on: [push, pull_request]
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
    - uses: actions/checkout@v3
    - uses: actions/setup-go@v4
      with:
        go-version: '1.23'
    - name: Unit tests
      run: go test ./... -v
    - name: Build
      run: make build
    - name: Docker build
      run: make docker-build IMG=profiling-operator:test
```

## Test Coverage

Current coverage:
- Metrics package: Threshold checking, percentage calculation
- Uploader package: Service name extraction, S3 key generation
- Controller: (Integration tests recommended)
- Profiler: (Integration tests recommended)

To improve coverage, add tests for:
- Controller reconciliation logic
- Error handling paths
- Edge cases (empty labels, missing fields)
- Concurrent profiling operations


