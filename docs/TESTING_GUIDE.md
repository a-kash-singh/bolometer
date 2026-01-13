# Testing Guide

## Quick Start

### Run All Tests
```bash
make test
```

### Run Controller Tests Only
```bash
make test-controller
```

### Generate Coverage Report
```bash
make test-coverage
```

### Generate HTML Coverage Report
```bash
make test-coverage-html
open coverage.html
```

## Test Structure

### Controller Tests
Location: `internal/controller/*_test.go`

#### Files
- `profilingconfig_controller_test.go` - Main controller reconciliation tests
- `pod_watcher_test.go` - Pod tracking and selection tests

#### Test Coverage
- **22 controller tests** - Reconciliation logic, validation, monitoring lifecycle
- **17 pod watcher tests** - Pod discovery, tracking, cooldown management
- **66.5% code coverage** for controller package

### Example Test
```go
func TestReconcile_ValidConfig(t *testing.T) {
    config := createTestProfilingConfig("test-config", "default")
    reconciler := setupTestReconciler(config)

    req := ctrl.Request{
        NamespacedName: types.NamespacedName{
            Name:      config.Name,
            Namespace: config.Namespace,
        },
    }

    result, err := reconciler.Reconcile(context.Background(), req)

    if err != nil {
        t.Errorf("Reconcile returned unexpected error: %v", err)
    }

    if result.RequeueAfter != 30*time.Second {
        t.Errorf("Expected requeue after 30s, got %v", result.RequeueAfter)
    }
}
```

## Writing New Tests

### 1. Using Fake Clients

```go
func TestMyFeature(t *testing.T) {
    // Create test objects
    config := createTestProfilingConfig("test", "default")
    pod := createTestPod("test-pod", "default", true)

    // Setup reconciler with fake clients
    reconciler := setupTestReconciler(config, pod)

    // Your test logic here
}
```

### 2. Test Fixtures

Use helper functions to create test objects:

```go
// Create a ProfilingConfig
config := createTestProfilingConfig("name", "namespace")

// Create a Pod with profiling annotation
pod := createTestPod("pod-name", "namespace", true)

// Create a Pod without annotation
pod := createTestPod("pod-name", "namespace", false)
```

### 3. Assertions

```go
// Check for expected behavior
if result.Requeue {
    t.Error("Expected no requeue")
}

// Verify status updates
updatedConfig := &profilingv1alpha1.ProfilingConfig{}
err = reconciler.Get(ctx, req.NamespacedName, updatedConfig)
if updatedConfig.Status.ActivePods != 1 {
    t.Errorf("Expected ActivePods=1, got %d", updatedConfig.Status.ActivePods)
}
```

### 4. Table-Driven Tests

```go
func TestValidation(t *testing.T) {
    tests := []struct {
        name        string
        config      *profilingv1alpha1.ProfilingConfig
        expectError bool
    }{
        {
            name:        "Valid config",
            config:      createTestProfilingConfig("test", "default"),
            expectError: false,
        },
        {
            name: "Missing bucket",
            config: func() *profilingv1alpha1.ProfilingConfig {
                c := createTestProfilingConfig("test", "default")
                c.Spec.S3Config.Bucket = ""
                return c
            }(),
            expectError: true,
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            err := validateConfig(tt.config)
            if (err != nil) != tt.expectError {
                t.Errorf("Expected error=%v, got %v", tt.expectError, err)
            }
        })
    }
}
```

## Test Coverage Goals

### Current Coverage (66.5%)

#### Well Covered (>80%)
- ✅ Pod discovery and filtering
- ✅ Pod tracking lifecycle
- ✅ Configuration validation
- ✅ Monitoring start/stop
- ✅ Basic reconciliation

#### Partially Covered (40-80%)
- ⚠️ On-demand monitoring (46.2%)
- ⚠️ Threshold monitoring (87.5%)

#### Not Covered (<40%)
- ❌ Profile capture (0%)
- ❌ S3 upload (0%)
- ❌ Status updates after profiling (0%)
- ❌ Threshold violation checking (0%)

### Future Goals

**Short-term (70% coverage):**
- Add profiler tests with mocked port-forward
- Add S3 uploader tests with LocalStack

**Medium-term (80% coverage):**
- Integration tests with envtest
- E2E test automation

**Long-term (85%+ coverage):**
- Performance tests
- Chaos/failure tests
- Load testing

## Testing Best Practices

### DO ✅
- Use fake clients for unit tests
- Test both happy path and error cases
- Use descriptive test names
- Keep tests fast and independent
- Use table-driven tests for multiple scenarios
- Assert on specific values, not just "no error"
- Test concurrent access when relevant

### DON'T ❌
- Don't use real Kubernetes clusters in unit tests
- Don't test implementation details
- Don't write flaky tests (timing-dependent)
- Don't skip cleanup (defer statements)
- Don't ignore test failures
- Don't write tests that depend on each other
- Don't test external dependencies directly

## Running Tests in CI

### GitHub Actions Example

```yaml
name: Test
on: [push, pull_request]
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3

      - name: Setup Go
        uses: actions/setup-go@v4
        with:
          go-version: '1.23'

      - name: Run tests
        run: make test

      - name: Check coverage
        run: |
          make test-coverage
          go tool cover -func cover.out | grep total | awk '{if ($3+0 < 60) {print "Coverage below 60%"; exit 1}}'

      - name: Upload coverage
        uses: codecov/codecov-action@v3
        with:
          files: ./cover.out
```

## Debugging Tests

### Run specific test
```bash
go test ./internal/controller -run TestReconcile_ValidConfig -v
```

### Run with race detector
```bash
go test ./internal/controller -race
```

### Run with CPU profiling
```bash
go test ./internal/controller -cpuprofile cpu.prof
go tool pprof cpu.prof
```

### Run with memory profiling
```bash
go test ./internal/controller -memprofile mem.prof
go tool pprof mem.prof
```

## Common Issues

### Issue: Test hangs
**Cause:** Goroutine not properly terminated
**Solution:** Ensure context cancellation in tests
```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()
```

### Issue: Race condition detected
**Cause:** Concurrent access to shared data without locks
**Solution:** Use mutexes or channels for synchronization

### Issue: Flaky test
**Cause:** Timing dependencies or global state
**Solution:** Use channels or polling instead of time.Sleep

## Additional Resources

- [Controller Runtime Testing](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/client/fake)
- [Testing Kubernetes Operators](https://sdk.operatorframework.io/docs/building-operators/golang/testing/)
- [Go Testing Best Practices](https://go.dev/doc/tutorial/add-a-test)

## Test Summary

Run this to see test summary:
```bash
make test-controller
```

Output:
```
PASS
coverage: 66.5% of statements
ok      github.com/profiling-operator/profiling-operator/internal/controller    1.018s
```

Detailed coverage by function:
```
pod_watcher.go:40:          NewPodWatcher              100.0%
pod_watcher.go:49:          ListMatchingPods           87.5%
pod_watcher.go:82:          isPodProfilingEnabled      100.0%
pod_watcher.go:92:          TrackPod                   100.0%
pod_watcher.go:112:         StopTrackingPod            100.0%
pod_watcher.go:148:         CanProfile                 100.0%
profilingconfig_controller.go:43:   NewProfilingConfigReconciler   100.0%
profilingconfig_controller.go:72:   Reconcile                      83.3%
profilingconfig_controller.go:278:  validateConfig                 100.0%
total:                                                     66.5%
```

## Contributing

When adding new features:
1. Write tests first (TDD)
2. Run tests locally: `make test`
3. Check coverage: `make test-coverage`
4. Ensure coverage doesn't decrease
5. Add integration tests if applicable

## Questions?

- See existing tests in `internal/controller/*_test.go`
- Check `CONTROLLER_TESTS_SUMMARY.md` for detailed test descriptions
- Refer to controller-runtime documentation for fake client usage
