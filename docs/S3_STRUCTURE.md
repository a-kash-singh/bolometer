# S3 Storage Structure

This document explains how profiles are organized in S3 storage.

## Directory Structure

Profiles are stored in S3 with the following hierarchy:

```
s3://{bucket}/{prefix}/{date}/{service-name}/{timestamp}-{profile-type}.pprof
```

### Components

**Bucket**: The S3 bucket name configured in ProfilingConfig

**Prefix**: Optional prefix path (default: empty)

**Date**: Date when profile was captured in `YYYY-MM-DD` format
- Example: `2024-01-15`
- Allows easy organization by date
- Simplifies lifecycle policies

**Service Name**: Extracted intelligently from pod metadata
- See service name extraction section below

**Filename**: `{timestamp}-{profile-type}.pprof`
- Timestamp: `YYYYMMDDHHmmss` format (20240115-120000)
- Profile type: heap, cpu, goroutine, mutex

## Example Structure

```
s3://my-profiling-bucket/
├── profiles/
│   ├── 2024-01-15/
│   │   ├── my-web-app/
│   │   │   ├── 20240115-120000-heap.pprof
│   │   │   ├── 20240115-120000-cpu.pprof
│   │   │   ├── 20240115-120000-goroutine.pprof
│   │   │   ├── 20240115-120000-mutex.pprof
│   │   │   ├── 20240115-143000-heap.pprof
│   │   │   └── 20240115-143000-cpu.pprof
│   │   ├── payment-service/
│   │   │   ├── 20240115-121500-heap.pprof
│   │   │   └── 20240115-121500-cpu.pprof
│   │   └── auth-service/
│   │       ├── 20240115-130000-heap.pprof
│   │       └── 20240115-130000-cpu.pprof
│   └── 2024-01-16/
│       ├── my-web-app/
│       │   └── 20240116-090000-heap.pprof
│       └── payment-service/
│           └── 20240116-092000-cpu.pprof
```

## Service Name Extraction

The operator extracts the service name from pod metadata using the following priority:

### 1. app.kubernetes.io/name Label (Recommended)

This is the Kubernetes recommended label for application name.

```yaml
apiVersion: v1
kind: Pod
metadata:
  labels:
    app.kubernetes.io/name: my-web-app
```

Result: `s3://bucket/profiles/2024-01-15/my-web-app/...`

### 2. app Label (Common Convention)

Widely used convention in Kubernetes.

```yaml
apiVersion: v1
kind: Pod
metadata:
  labels:
    app: payment-service
```

Result: `s3://bucket/profiles/2024-01-15/payment-service/...`

### 3. k8s-app Label

Alternative label convention.

```yaml
apiVersion: v1
kind: Pod
metadata:
  labels:
    k8s-app: auth-service
```

Result: `s3://bucket/profiles/2024-01-15/auth-service/...`

### 4. Owner Reference (Workload Name)

If no labels are found, extracts from owner references.

For Deployments:
```yaml
ownerReferences:
  - kind: ReplicaSet
    name: my-app-7d8f9c5b6d
```

Result: `s3://bucket/profiles/2024-01-15/my-app/...`
(Hash suffix is stripped automatically)

For StatefulSets:
```yaml
ownerReferences:
  - kind: StatefulSet
    name: database-statefulset
```

Result: `s3://bucket/profiles/2024-01-15/database-statefulset/...`

### 5. Pod Name Prefix (Fallback)

As a last resort, uses pod name with hash suffixes removed.

Pod name: `my-service-abc123-xyz456`

Result: `s3://bucket/profiles/2024-01-15/my-service/...`

## Benefits of This Structure

### 1. Date-Based Organization

- Easy to browse profiles by date
- Simplifies finding recent issues
- Natural chronological ordering

### 2. Service Grouping

- All profiles for a service are together
- Easy to compare profiles across time
- Service-specific analysis

### 3. Lifecycle Management

S3 lifecycle policies can target entire dates:

```json
{
  "Rules": [{
    "Id": "DeleteOldProfiles",
    "Status": "Enabled",
    "Filter": {
      "Prefix": "profiles/"
    },
    "Expiration": {
      "Days": 30
    }
  }]
}
```

### 4. Cost Optimization

Transition old profiles to cheaper storage:

```json
{
  "Rules": [{
    "Id": "TransitionToGlacier",
    "Status": "Enabled",
    "Transitions": [
      {
        "Days": 7,
        "StorageClass": "GLACIER"
      }
    ]
  }]
}
```

### 5. Easy Querying

AWS CLI commands are intuitive:

```bash
# List profiles for specific date
aws s3 ls s3://bucket/profiles/2024-01-15/

# List profiles for specific service
aws s3 ls s3://bucket/profiles/2024-01-15/my-web-app/

# Download all profiles from a date
aws s3 sync s3://bucket/profiles/2024-01-15/ ./profiles/

# Download specific service profiles
aws s3 sync s3://bucket/profiles/2024-01-15/my-web-app/ ./my-web-app-profiles/

# Search across dates for a service
aws s3 ls s3://bucket/profiles/ --recursive | grep my-web-app
```

## Profile Metadata

Each profile includes S3 object metadata:

```
pod-name: my-web-app-7d8f9c5b6d-xyz456
pod-namespace: production
profile-type: heap
reason: threshold-exceeded
timestamp: 2024-01-15T12:00:00Z
pod-label-app: my-web-app
pod-label-version: v1.2.3
```

View metadata:

```bash
aws s3api head-object \
  --bucket my-bucket \
  --key profiles/2024-01-15/my-web-app/20240115-120000-heap.pprof
```

## Best Practices

### 1. Use Recommended Labels

Always add `app.kubernetes.io/name` label to your pods:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-web-app
spec:
  template:
    metadata:
      labels:
        app.kubernetes.io/name: my-web-app
        app.kubernetes.io/version: "1.2.3"
```

### 2. Consistent Naming

Use consistent service names across environments:
- `my-web-app` (not `my-web-app-prod`)
- Use namespace to distinguish environments

### 3. S3 Bucket Organization

Recommended bucket structure:

```
s3://company-profiling/
├── dev/
│   └── 2024-01-15/
│       └── my-service/
├── staging/
│   └── 2024-01-15/
│       └── my-service/
└── production/
    └── 2024-01-15/
        └── my-service/
```

Set prefix in ProfilingConfig:

```yaml
spec:
  s3Config:
    bucket: company-profiling
    prefix: production  # or dev, staging
    region: us-west-2
```

### 4. Lifecycle Policies

Set up automatic cleanup:

```json
{
  "Rules": [
    {
      "Id": "ArchiveOldProfiles",
      "Status": "Enabled",
      "Transitions": [
        {
          "Days": 7,
          "StorageClass": "STANDARD_IA"
        },
        {
          "Days": 30,
          "StorageClass": "GLACIER"
        }
      ],
      "Expiration": {
        "Days": 90
      }
    }
  ]
}
```

### 5. Monitoring

Track S3 usage with CloudWatch:
- Storage metrics per prefix
- Request metrics
- Data transfer

## Analysis Workflows

### Compare Profiles Over Time

```bash
# Download all CPU profiles for a service
aws s3 sync s3://bucket/profiles/2024-01-15/my-app/ ./day1/ --exclude "*" --include "*-cpu.pprof"
aws s3 sync s3://bucket/profiles/2024-01-16/my-app/ ./day2/ --exclude "*" --include "*-cpu.pprof"

# Compare with pprof
go tool pprof -base=day1/20240115-120000-cpu.pprof day2/20240116-120000-cpu.pprof
```

### Analyze All Profiles for a Date

```bash
# Download all profiles from a specific date
aws s3 sync s3://bucket/profiles/2024-01-15/ ./analysis/

# Find memory leaks
for file in analysis/*/heap.pprof; do
  echo "Analyzing $file"
  go tool pprof -top "$file"
done
```

### Service-Specific Analysis

```bash
# Get all heap profiles for a service across dates
aws s3 cp s3://bucket/profiles/ ./heap-profiles/ \
  --recursive \
  --exclude "*" \
  --include "*/my-web-app/*-heap.pprof"

# Batch analysis
for f in heap-profiles/**/*-heap.pprof; do
  go tool pprof -top "$f" > "${f}.analysis.txt"
done
```

## Migration from Other Structures

If you have existing profiles with a different structure, use AWS CLI to reorganize:

```bash
# Example: migrate from old structure to new structure
# Old: s3://bucket/namespace/pod-name/timestamp/type.pprof
# New: s3://bucket/date/service-name/timestamp-type.pprof

# This requires custom scripting based on your specific old structure
```

## Troubleshooting

### Service Name Not Recognized

Check pod labels:

```bash
kubectl get pod <pod-name> -o jsonpath='{.metadata.labels}'
```

Add recommended label:

```bash
kubectl label pod <pod-name> app.kubernetes.io/name=my-service
```

### Wrong Service Name

The operator uses the first matching label/reference. To override, ensure your pod has the `app.kubernetes.io/name` label as it has the highest priority.

### Profiles Not Appearing

Check operator logs:

```bash
kubectl logs -n profiling-system -l app=profiling-operator | grep S3
```

Verify S3 path:

```bash
aws s3 ls s3://bucket/profiles/$(date +%Y-%m-%d)/
```

## Summary

The date-based, service-organized S3 structure provides:
- Intuitive browsing and searching
- Easy lifecycle management
- Service-centric analysis
- Cost-effective storage
- Simple automation

This structure is optimized for both operational troubleshooting and historical analysis workflows.

