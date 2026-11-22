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
- `monitor_test.go`, `validity_test.go`, `s3_client_lifecycle_test.go` -
  Monitor schedules, cooldowns, capture limit, status counters, invalid
  configs and the S3 client, driven by a fake clock
- `isolation_test.go` and the other black-box test files - End-to-end
  captures through a test API server (port-forward, pprof) and a fake S3,
  using only APIs that also exist in the original controller

See "Running Tests" in [DEVELOPER.md](../DEVELOPER.md) for how these fit
together, the `BOLOMETER_REAL_TIMING_TESTS` switch, and how to run the
regression tests against the original code.

#### Test Coverage
- **92% code coverage** for the controller package

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

### Current Coverage (controller package: 92%)

Covered by the controller tests, including end to end:
- ✅ Pod discovery, per-config tracking and untracking
- ✅ Configuration validation, the `Ready` condition and events
- ✅ Threshold and on-demand schedules, cooldowns, spec edits
- ✅ Background captures and the per-config capture limit
- ✅ Profile capture through a port-forward (including partial failures and
  lost connections)
- ✅ S3 upload through the per-config client
- ✅ Status counters, including conflicts and shutdown

Package-level coverage of `profiler` and `uploader` (about 43% and 80%)
counts only their own tests. Their port-forward and upload paths are also
exercised by the controller's black-box tests.

### Future Goals

- Raise `metrics` package coverage (currently about 17%)
- Run the end-to-end flow against a real cluster (see `e2e-local.sh`)

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

Output (abridged):
```
PASS
coverage: 92.1% of statements
ok      github.com/a-kash-singh/bolometer/internal/controller    35s
```

`make test-controller` also prints coverage by function (`go tool cover
-func controller-cover.out`).

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
