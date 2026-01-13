# Installation Guide

This guide provides detailed instructions for installing the Profiling Operator using the automated installation script.

## Table of Contents

- [Prerequisites](#prerequisites)
- [Quick Install](#quick-install)
- [Installation Options](#installation-options)
- [AWS/EKS Setup](#awseks-setup)
- [Local/Development Setup](#localdevelopment-setup)
- [Verification](#verification)
- [Troubleshooting](#troubleshooting)
- [Manual Installation](#manual-installation)
- [Uninstallation](#uninstallation)

## Prerequisites

Before running the installation script, ensure you have:

### Required
- **kubectl** (v1.24+) - Kubernetes CLI tool
- **Kubernetes cluster** (v1.30+) - Access to a running cluster
- **metrics-server** - Installed in the cluster for resource monitoring

### For AWS/EKS
- **OIDC Provider** - Enabled on your EKS cluster for IRSA
- **IAM Role** - With S3 write permissions (see [AWS/EKS Setup](#awseks-setup))
- **S3 Bucket** - For storing profiling data
- **AWS CLI** (optional) - For validation and troubleshooting

### For Local Development
- **kind** or **minikube** - Local Kubernetes cluster
- **LocalStack** (optional) - For S3 emulation

## Quick Install

### Interactive Installation (Recommended)

The installation script will guide you through the process:

```bash
./install.sh
```

The script will:
1. ✅ Validate all prerequisites (kubectl, cluster access, metrics-server)
2. 📝 Prompt for configuration (S3 bucket, region, IAM role)
3. 🚀 Install the operator components (CRDs, RBAC, deployment)
4. ✅ Validate the installation
5. 📋 Display next steps

### Non-Interactive Installation

For automated deployments, you can provide configuration via command-line:

```bash
./install.sh \
  --namespace profiling-system \
  --image ghcr.io/yourorg/profiling-operator:v0.1.0
```

## Installation Options

### Command-Line Flags

| Flag | Description | Default |
|------|-------------|---------|
| `--namespace <name>` | Kubernetes namespace to install into | `profiling-system` |
| `--image <image>` | Container image to use | `profiling-operator:latest` |
| `--skip-aws-validation` | Skip AWS/IRSA validation checks | `false` |
| `--localstack` | Configure for LocalStack testing | `false` |
| `--help` | Show help message | - |

### Examples

**Install to custom namespace:**
```bash
./install.sh --namespace my-monitoring
```

**Use a specific image version:**
```bash
./install.sh --image ghcr.io/yourorg/profiling-operator:v0.1.0
```

**Skip AWS validation (for non-AWS clusters):**
```bash
./install.sh --skip-aws-validation
```

**LocalStack mode (local development):**
```bash
./install.sh --localstack
```

## AWS/EKS Setup

### Prerequisites

1. **Enable OIDC Provider**

   Check if OIDC is enabled:
   ```bash
   aws eks describe-cluster --name <cluster-name> --query "cluster.identity.oidc.issuer" --output text
   ```

   If not enabled, enable it:
   ```bash
   eksctl utils associate-iam-oidc-provider --cluster <cluster-name> --approve
   ```

2. **Create S3 Bucket**

   ```bash
   aws s3 mb s3://my-profiling-bucket --region us-west-2
   ```

3. **Create IAM Policy**

   Create a file `profiling-operator-policy.json`:
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
       },
       {
         "Effect": "Allow",
         "Action": [
           "s3:ListBucket"
         ],
         "Resource": "arn:aws:s3:::my-profiling-bucket"
       }
     ]
   }
   ```

   Create the policy:
   ```bash
   aws iam create-policy \
     --policy-name ProfilingOperatorPolicy \
     --policy-document file://profiling-operator-policy.json
   ```

4. **Create IAM Role for IRSA**

   Get your OIDC provider ID:
   ```bash
   OIDC_ID=$(aws eks describe-cluster --name <cluster-name> --query "cluster.identity.oidc.issuer" --output text | cut -d '/' -f 5)
   AWS_ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
   AWS_REGION=us-west-2
   ```

   Create trust policy `trust-policy.json`:
   ```json
   {
     "Version": "2012-10-17",
     "Statement": [
       {
         "Effect": "Allow",
         "Principal": {
           "Federated": "arn:aws:iam::${AWS_ACCOUNT_ID}:oidc-provider/oidc.eks.${AWS_REGION}.amazonaws.com/id/${OIDC_ID}"
         },
         "Action": "sts:AssumeRoleWithWebIdentity",
         "Condition": {
           "StringEquals": {
             "oidc.eks.${AWS_REGION}.amazonaws.com/id/${OIDC_ID}:sub": "system:serviceaccount:profiling-system:profiling-operator",
             "oidc.eks.${AWS_REGION}.amazonaws.com/id/${OIDC_ID}:aud": "sts.amazonaws.com"
           }
         }
       }
     ]
   }
   ```

   Replace placeholders and create the role:
   ```bash
   envsubst < trust-policy.json > trust-policy-final.json

   aws iam create-role \
     --role-name profiling-operator-role \
     --assume-role-policy-document file://trust-policy-final.json

   aws iam attach-role-policy \
     --role-name profiling-operator-role \
     --policy-arn arn:aws:iam::${AWS_ACCOUNT_ID}:policy/ProfilingOperatorPolicy
   ```

### Installation

Now run the installation script and provide the IAM role ARN when prompted:

```bash
./install.sh
```

When prompted, enter:
- **S3 bucket name**: `my-profiling-bucket`
- **AWS region**: `us-west-2`
- **IAM Role ARN**: `arn:aws:iam::123456789012:role/profiling-operator-role`

## Local/Development Setup

### Using kind + LocalStack

1. **Create kind cluster:**

   ```bash
   kind create cluster --name profiling-test
   ```

2. **Install metrics-server:**

   ```bash
   kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml

   # Patch for kind (disable TLS verification)
   kubectl patch deployment metrics-server -n kube-system --type='json' \
     -p='[{"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--kubelet-insecure-tls"}]'
   ```

3. **Deploy LocalStack:**

   ```bash
   kubectl apply -f examples/localstack.yaml
   ```

   Wait for LocalStack to be ready:
   ```bash
   kubectl wait --for=condition=ready pod -l app=localstack --timeout=120s
   ```

4. **Create S3 bucket in LocalStack:**

   ```bash
   kubectl run aws-cli --rm -it --image=amazon/aws-cli --restart=Never -- \
     --endpoint-url=http://localstack.default.svc.cluster.local:4566 \
     s3 mb s3://profiling-bucket
   ```

5. **Install the operator:**

   ```bash
   # Build local image
   make docker-build

   # Load into kind
   kind load docker-image profiling-operator:latest --name profiling-test

   # Install with LocalStack mode
   ./install.sh --localstack
   ```

### Using minikube

Similar to kind, but use `minikube image load` instead of `kind load docker-image`:

```bash
minikube start
make docker-build
minikube image load profiling-operator:latest
./install.sh --localstack
```

## Verification

The installation script automatically validates the installation, but you can manually verify:

### 1. Check Deployment Status

```bash
kubectl get deployment profiling-operator -n profiling-system
```

Expected output:
```
NAME                  READY   UP-TO-DATE   AVAILABLE   AGE
profiling-operator    1/1     1            1           2m
```

### 2. Check Pod Status

```bash
kubectl get pods -n profiling-system
```

Expected output:
```
NAME                                  READY   STATUS    RESTARTS   AGE
profiling-operator-xxxxx-yyyyy        1/1     Running   0          2m
```

### 3. Check Logs

```bash
kubectl logs -n profiling-system -l app=profiling-operator
```

Look for:
- `Starting EventSource` - Controller is starting
- `Starting Controller` - Reconciliation loop started
- No error messages

### 4. Check CRD

```bash
kubectl get crd profilingconfigs.profiling.io
```

### 5. Check Health Endpoints

```bash
POD_NAME=$(kubectl get pods -n profiling-system -l app=profiling-operator -o jsonpath='{.items[0].metadata.name}')

# Liveness probe
kubectl exec -n profiling-system $POD_NAME -- wget -q -O- http://localhost:8081/healthz

# Readiness probe
kubectl exec -n profiling-system $POD_NAME -- wget -q -O- http://localhost:8081/readyz
```

Both should return: `ok`

### 6. Test ProfilingConfig Creation

```bash
kubectl apply -f config/samples/profiling_v1alpha1_profilingconfig.yaml
kubectl get profilingconfigs -A
```

## Troubleshooting

### Installation Script Fails

**Problem:** `kubectl: command not found`

**Solution:** Install kubectl:
```bash
# macOS
brew install kubectl

# Linux
curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl"
sudo install -o root -g root -m 0755 kubectl /usr/local/bin/kubectl
```

---

**Problem:** `Cannot access Kubernetes cluster`

**Solution:** Configure your kubeconfig:
```bash
# For EKS
aws eks update-kubeconfig --name <cluster-name> --region <region>

# For kind
kind get kubeconfig --name <cluster-name> > ~/.kube/config

# Verify
kubectl cluster-info
```

---

**Problem:** `metrics-server is not installed`

**Solution:** Install metrics-server:
```bash
kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml

# For local clusters, patch to disable TLS verification
kubectl patch deployment metrics-server -n kube-system --type='json' \
  -p='[{"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--kubelet-insecure-tls"}]'
```

### Operator Not Starting

**Problem:** Pod is in `ImagePullBackOff` state

**Solution:**
- If using local image, ensure it's loaded into the cluster:
  ```bash
  # kind
  kind load docker-image profiling-operator:latest

  # minikube
  minikube image load profiling-operator:latest
  ```
- Use a registry image instead:
  ```bash
  ./install.sh --image ghcr.io/yourorg/profiling-operator:v0.1.0
  ```

---

**Problem:** Pod is in `CrashLoopBackOff` state

**Solution:** Check logs for errors:
```bash
kubectl logs -n profiling-system -l app=profiling-operator --previous
```

Common issues:
- Missing RBAC permissions: Re-run `./install.sh`
- Invalid IRSA configuration: Verify IAM role and trust policy
- Metrics server not accessible: Ensure metrics-server is running

---

**Problem:** `unable to recognize "config/crd/...": no matches for kind`

**Solution:** The CRD wasn't applied. Manually apply:
```bash
kubectl apply -f config/crd/profiling.io_profilingconfigs.yaml
kubectl wait --for=condition=Established --timeout=60s crd/profilingconfigs.profiling.io
```

### AWS/IRSA Issues

**Problem:** `AccessDenied` errors when uploading to S3

**Solution:** Verify IRSA configuration:

1. Check service account annotation:
   ```bash
   kubectl get sa profiling-operator -n profiling-system -o yaml
   ```

   Should have:
   ```yaml
   annotations:
     eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/profiling-operator-role
   ```

2. Verify IAM role trust policy includes the service account

3. Check IAM policy allows S3 PutObject

4. Test AWS credentials in the pod:
   ```bash
   kubectl exec -n profiling-system <pod-name> -- env | grep AWS
   ```

## Manual Installation

If you prefer not to use the installation script:

### 1. Create Namespace

```bash
kubectl create namespace profiling-system
```

### 2. Install CRDs

```bash
kubectl apply -f config/crd/profiling.io_profilingconfigs.yaml
```

### 3. Create Service Account

Edit `config/rbac/service_account.yaml` to add IRSA annotation, then:

```bash
kubectl apply -f config/rbac/service_account.yaml
```

### 4. Install RBAC

```bash
kubectl apply -f config/rbac/role.yaml
kubectl apply -f config/rbac/role_binding.yaml
```

### 5. Deploy Operator

Edit `config/manager/deployment.yaml` to set the correct image, then:

```bash
kubectl apply -f config/manager/deployment.yaml
```

### 6. Verify

```bash
kubectl get pods -n profiling-system
kubectl logs -n profiling-system -l app=profiling-operator
```

## Uninstallation

### Using kubectl

```bash
# Delete the operator
kubectl delete -f config/manager/deployment.yaml

# Delete RBAC
kubectl delete -f config/rbac/

# Delete CRDs (this will also delete all ProfilingConfig resources!)
kubectl delete -f config/crd/

# Delete namespace
kubectl delete namespace profiling-system
```

### Using Helm

If installed via Helm:

```bash
helm uninstall profiling-operator -n profiling-system
kubectl delete crd profilingconfigs.profiling.io
kubectl delete namespace profiling-system
```

### Clean Up S3 Data

The uninstallation does not delete S3 data. To clean up:

```bash
aws s3 rm s3://my-profiling-bucket/profiles/ --recursive
```

## Next Steps

After successful installation:

1. **Deploy a sample application**: See `examples/demo-go-app/`
2. **Create a ProfilingConfig**: See `config/samples/`
3. **Monitor profiles**: Set up Grafana dashboards
4. **Integrate with analysis tools**: Use pprof to analyze captured profiles

For more information, see:
- [Quick Start Guide](QUICKSTART.md)
- [Testing Guide](TESTING.md)
- [Main README](../README.md)
