# Quick Start Guide

This guide will help you get the Profiling Operator up and running in your Kubernetes cluster.

## Prerequisites

- Kubernetes cluster (v1.30+)
- kubectl configured
- metrics-server installed
- AWS account with S3 bucket
- IAM role for IRSA (if using EKS)

## Step 1: Install metrics-server (if not already installed)

```bash
kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml
```

Verify it's running:

```bash
kubectl get deployment metrics-server -n kube-system
kubectl top nodes
```

## Step 2: Set up AWS IAM for IRSA

### Create S3 Bucket

```bash
aws s3 mb s3://my-profiling-bucket --region us-west-2
```

### Create IAM Policy

Save this as `profiling-policy.json`:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "s3:PutObject",
        "s3:PutObjectAcl"
      ],
      "Resource": "arn:aws:s3:::my-profiling-bucket/profiles/*"
    }
  ]
}
```

Create the policy:

```bash
aws iam create-policy \
  --policy-name ProfilingOperatorS3Access \
  --policy-document file://profiling-policy.json
```

### Create IAM Role for IRSA

Get your cluster's OIDC provider:

```bash
export CLUSTER_NAME=your-cluster-name
export AWS_REGION=us-west-2
export ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
export OIDC_ID=$(aws eks describe-cluster --name $CLUSTER_NAME --region $AWS_REGION --query "cluster.identity.oidc.issuer" --output text | cut -d '/' -f 5)
```

Create trust policy `trust-policy.json`:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "Federated": "arn:aws:iam::ACCOUNT_ID:oidc-provider/oidc.eks.REGION.amazonaws.com/id/OIDC_ID"
      },
      "Action": "sts:AssumeRoleWithWebIdentity",
      "Condition": {
        "StringEquals": {
          "oidc.eks.REGION.amazonaws.com/id/OIDC_ID:sub": "system:serviceaccount:profiling-system:profiling-operator"
        }
      }
    }
  ]
}
```

Replace ACCOUNT_ID, REGION, and OIDC_ID with your values.

Create the role:

```bash
aws iam create-role \
  --role-name profiling-operator-role \
  --assume-role-policy-document file://trust-policy.json

aws iam attach-role-policy \
  --role-name profiling-operator-role \
  --policy-arn arn:aws:iam::${ACCOUNT_ID}:policy/ProfilingOperatorS3Access
```

## Step 3: Install the Operator

### Option A: Using Helm (Recommended)

```bash
helm install profiling-operator ./helm/profiling-operator \
  --set serviceAccount.annotations."eks\.amazonaws\.com/role-arn"="arn:aws:iam::${ACCOUNT_ID}:role/profiling-operator-role" \
  --namespace profiling-system \
  --create-namespace
```

### Option B: Using kubectl

1. Update the service account annotation in `config/rbac/service_account.yaml`:

```yaml
annotations:
  eks.amazonaws.com/role-arn: arn:aws:iam::ACCOUNT_ID:role/profiling-operator-role
```

2. Deploy:

```bash
kubectl apply -f config/rbac/
kubectl apply -f config/crd/
kubectl apply -f config/manager/
```

### Verify Installation

```bash
kubectl get pods -n profiling-system
kubectl logs -n profiling-system -l app=profiling-operator
```

## Step 4: Deploy a Sample Application

### Build and Deploy Sample App

```bash
# Build the sample app
cd examples/sample-app
docker build -t demo-go-app:latest .
docker push your-registry/demo-go-app:latest

# Update the image in target-app.yaml
# Then deploy
kubectl apply -f ../target-app.yaml
```

Or use your existing Go application with pprof enabled.

## Step 5: Create ProfilingConfig

Create `my-profiling-config.yaml`:

```yaml
apiVersion: profiling.io/v1alpha1
kind: ProfilingConfig
metadata:
  name: demo-profiling
  namespace: demo
spec:
  selector:
    namespace: demo
    labelSelector:
      app: demo-go-app
  
  thresholds:
    cpuThresholdPercent: 80
    memoryThresholdPercent: 90
    checkIntervalSeconds: 30
    cooldownSeconds: 300
  
  s3Config:
    bucket: my-profiling-bucket
    prefix: profiles
    region: us-west-2
  
  profileTypes:
  - heap
  - cpu
  - goroutine
  - mutex
```

Apply it:

```bash
kubectl apply -f my-profiling-config.yaml
```

## Step 6: Verify Profiling

### Check ProfilingConfig Status

```bash
kubectl get profilingconfig -n demo
kubectl describe profilingconfig demo-profiling -n demo
```

### Generate Load to Trigger Profiling

```bash
# Port-forward to the app
kubectl port-forward -n demo deployment/demo-go-app 8080:8080

# Generate load
for i in {1..100}; do
  curl http://localhost:8080/load &
done
```

### Check Operator Logs

```bash
kubectl logs -n profiling-system -l app=profiling-operator -f
```

You should see logs indicating:
- Pods being tracked
- Metrics being collected
- Threshold violations detected
- Profiles being captured
- Uploads to S3

### Verify Profiles in S3

```bash
aws s3 ls s3://my-profiling-bucket/profiles/ --recursive
```

You should see profiles organized by date and service:
```
profiles/2024-01-15/demo-go-app/20240115-120000-heap.pprof
profiles/2024-01-15/demo-go-app/20240115-120000-cpu.pprof
profiles/2024-01-15/demo-go-app/20240115-143000-heap.pprof
```

## Step 7: Enable On-Demand Profiling (Optional)

Update your ProfilingConfig to add on-demand profiling:

```yaml
spec:
  # ... existing config ...
  onDemand:
    enabled: true
    intervalSeconds: 35
```

Apply the update:

```bash
kubectl apply -f my-profiling-config.yaml
```

Now profiles will be captured every 35 seconds regardless of resource usage.

## Step 8: Analyze Profiles

Download profiles from S3:

```bash
# Download profiles for a specific date and service
aws s3 cp s3://my-profiling-bucket/profiles/2024-01-15/demo-go-app/ . --recursive

# Or download all profiles for a specific date
aws s3 cp s3://my-profiling-bucket/profiles/2024-01-15/ . --recursive
```

Analyze with Go's pprof tool:

```bash
# Heap profile
go tool pprof 20240115-120000-heap.pprof

# CPU profile
go tool pprof 20240115-120000-cpu.pprof

# Goroutine profile
go tool pprof 20240115-120000-goroutine.pprof

# Interactive web UI
go tool pprof -http=:8081 20240115-120000-cpu.pprof
```

## Troubleshooting

### Operator not starting

Check logs:
```bash
kubectl logs -n profiling-system -l app=profiling-operator
```

### Profiles not being captured

1. Verify pod annotation:
```bash
kubectl get pod -n demo -l app=demo-go-app -o jsonpath='{.items[0].metadata.annotations}'
```

2. Test pprof endpoint:
```bash
kubectl port-forward -n demo deployment/demo-go-app 6060:6060
curl http://localhost:6060/debug/pprof/
```

3. Check metrics are available:
```bash
kubectl top pod -n demo
```

### S3 upload failures

1. Verify IRSA annotation:
```bash
kubectl get sa profiling-operator -n profiling-system -o yaml
```

2. Check IAM role and policy
3. Verify S3 bucket exists and is accessible

### High resource usage

Adjust profiling frequency:
- Increase `checkIntervalSeconds`
- Increase `cooldownSeconds`
- Reduce `onDemand.intervalSeconds` (or disable on-demand)

## Next Steps

- Configure multiple ProfilingConfigs for different namespaces
- Set up alerts based on operator metrics
- Create dashboards for profile statistics
- Integrate with your CI/CD pipeline
- Set up automated profile analysis

## Cleanup

```bash
# Delete ProfilingConfig
kubectl delete profilingconfig demo-profiling -n demo

# Delete sample app
kubectl delete -f examples/target-app.yaml

# Uninstall operator
helm uninstall profiling-operator -n profiling-system
# or
kubectl delete -f config/manager/
kubectl delete -f config/crd/
kubectl delete -f config/rbac/

# Delete namespace
kubectl delete namespace profiling-system
```

