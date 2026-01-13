# IAM Roles for Service Accounts (IRSA) Setup

This guide explains how to set up IRSA for the Profiling Operator to access S3 securely.

## What is IRSA?

IAM Roles for Service Accounts (IRSA) allows Kubernetes service accounts to assume AWS IAM roles. This provides:
- Fine-grained access control
- No need to manage AWS credentials
- Automatic credential rotation
- Audit trail through CloudTrail

## Prerequisites

- EKS cluster with OIDC provider enabled
- AWS CLI configured
- kubectl access to the cluster
- IAM permissions to create roles and policies

## Step 1: Enable OIDC Provider (if not already enabled)

Check if OIDC provider is enabled:

```bash
aws eks describe-cluster --name <cluster-name> --query "cluster.identity.oidc.issuer" --output text
```

If not enabled, enable it:

```bash
eksctl utils associate-iam-oidc-provider \
  --cluster <cluster-name> \
  --region <region> \
  --approve
```

## Step 2: Create S3 Bucket

```bash
export S3_BUCKET=profiling-operator-profiles
export AWS_REGION=us-west-2

aws s3 mb s3://${S3_BUCKET} --region ${AWS_REGION}
```

Optional: Enable versioning and lifecycle policies:

```bash
# Enable versioning
aws s3api put-bucket-versioning \
  --bucket ${S3_BUCKET} \
  --versioning-configuration Status=Enabled

# Create lifecycle policy to delete old profiles
cat > lifecycle-policy.json <<EOF
{
  "Rules": [
    {
      "Id": "DeleteOldProfiles",
      "Status": "Enabled",
      "Filter": {
        "Prefix": "profiles/"
      },
      "Expiration": {
        "Days": 30
      }
    }
  ]
}
EOF

aws s3api put-bucket-lifecycle-configuration \
  --bucket ${S3_BUCKET} \
  --lifecycle-configuration file://lifecycle-policy.json
```

## Step 3: Create IAM Policy

Create the policy document:

```bash
export ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)

cat > profiling-operator-policy.json <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "s3:PutObject",
        "s3:PutObjectAcl",
        "s3:GetObject",
        "s3:ListBucket"
      ],
      "Resource": [
        "arn:aws:s3:::${S3_BUCKET}",
        "arn:aws:s3:::${S3_BUCKET}/*"
      ]
    }
  ]
}
EOF
```

Create the policy:

```bash
aws iam create-policy \
  --policy-name ProfilingOperatorS3Policy \
  --policy-document file://profiling-operator-policy.json \
  --description "Policy for Profiling Operator to upload profiles to S3"

export POLICY_ARN=arn:aws:iam::${ACCOUNT_ID}:policy/ProfilingOperatorS3Policy
```

## Step 4: Create IAM Role with Trust Relationship

Get cluster OIDC ID:

```bash
export CLUSTER_NAME=your-cluster-name
export OIDC_ID=$(aws eks describe-cluster --name ${CLUSTER_NAME} --region ${AWS_REGION} --query "cluster.identity.oidc.issuer" --output text | cut -d '/' -f 5)
export NAMESPACE=profiling-system
export SERVICE_ACCOUNT=profiling-operator
```

Create trust policy:

```bash
cat > trust-policy.json <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "Federated": "arn:aws:iam::${ACCOUNT_ID}:oidc-provider/oidc.eks.${AWS_REGION}.amazonaws.com/id/${OIDC_ID}"
      },
      "Action": "sts:AssumeRoleWithWebIdentity",
      "Condition": {
        "StringEquals": {
          "oidc.eks.${AWS_REGION}.amazonaws.com/id/${OIDC_ID}:sub": "system:serviceaccount:${NAMESPACE}:${SERVICE_ACCOUNT}",
          "oidc.eks.${AWS_REGION}.amazonaws.com/id/${OIDC_ID}:aud": "sts.amazonaws.com"
        }
      }
    }
  ]
}
EOF
```

Create the role:

```bash
aws iam create-role \
  --role-name ProfilingOperatorRole \
  --assume-role-policy-document file://trust-policy.json \
  --description "IAM role for Profiling Operator"

export ROLE_ARN=arn:aws:iam::${ACCOUNT_ID}:role/ProfilingOperatorRole
```

Attach the policy to the role:

```bash
aws iam attach-role-policy \
  --role-name ProfilingOperatorRole \
  --policy-arn ${POLICY_ARN}
```

## Step 5: Install Operator with IRSA

### Using Helm

```bash
helm install profiling-operator ./helm/profiling-operator \
  --set serviceAccount.annotations."eks\.amazonaws\.com/role-arn"="${ROLE_ARN}" \
  --set defaultConfig.s3.bucket="${S3_BUCKET}" \
  --set defaultConfig.s3.region="${AWS_REGION}" \
  --namespace ${NAMESPACE} \
  --create-namespace
```

### Using kubectl

1. Update `config/rbac/service_account.yaml`:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: profiling-operator
  namespace: profiling-system
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::ACCOUNT_ID:role/ProfilingOperatorRole
```

2. Deploy:

```bash
kubectl apply -f config/rbac/
kubectl apply -f config/crd/
kubectl apply -f config/manager/
```

## Step 6: Verify IRSA Setup

Check the service account:

```bash
kubectl get sa profiling-operator -n profiling-system -o yaml
```

Verify the annotation is present:

```yaml
annotations:
  eks.amazonaws.com/role-arn: arn:aws:iam::ACCOUNT_ID:role/ProfilingOperatorRole
```

Check the pod has the correct environment variables:

```bash
kubectl get pod -n profiling-system -l app=profiling-operator -o jsonpath='{.items[0].spec.containers[0].env}'
```

You should see AWS-related environment variables injected by the webhook.

## Step 7: Test S3 Access

Create a test ProfilingConfig:

```yaml
apiVersion: profiling.io/v1alpha1
kind: ProfilingConfig
metadata:
  name: test-profiling
  namespace: default
spec:
  selector:
    namespace: default
  thresholds:
    cpuThresholdPercent: 80
    memoryThresholdPercent: 90
  s3Config:
    bucket: profiling-operator-profiles
    region: us-west-2
    prefix: test-profiles
  profileTypes:
  - heap
```

Monitor operator logs:

```bash
kubectl logs -n profiling-system -l app=profiling-operator -f
```

Look for successful S3 operations or permission errors.

## Troubleshooting

### Error: "AccessDenied"

1. Verify IAM policy has correct permissions
2. Check trust relationship in IAM role
3. Ensure OIDC provider ID matches
4. Verify service account annotation

### Error: "No credentials found"

1. Check OIDC provider is enabled on cluster
2. Verify pod has AWS environment variables
3. Ensure service account is correctly referenced in deployment

### Check AWS credentials in pod

```bash
kubectl exec -n profiling-system -it deployment/profiling-operator -- sh

# Inside the pod (if shell is available)
env | grep AWS
```

Expected variables:
- `AWS_ROLE_ARN`
- `AWS_WEB_IDENTITY_TOKEN_FILE`

### Verify IAM role assumption

Check CloudTrail for AssumeRoleWithWebIdentity events:

```bash
aws cloudtrail lookup-events \
  --lookup-attributes AttributeKey=EventName,AttributeValue=AssumeRoleWithWebIdentity \
  --max-results 10
```

## Best Practices

1. **Least Privilege**: Only grant necessary S3 permissions
2. **Bucket Policies**: Add bucket policies for additional security
3. **Encryption**: Enable S3 bucket encryption
4. **Lifecycle Policies**: Auto-delete old profiles to save costs
5. **Monitoring**: Set up CloudWatch alarms for unauthorized access
6. **Audit**: Regularly review CloudTrail logs

## Security Considerations

### Enable S3 Bucket Encryption

```bash
aws s3api put-bucket-encryption \
  --bucket ${S3_BUCKET} \
  --server-side-encryption-configuration '{
    "Rules": [{
      "ApplyServerSideEncryptionByDefault": {
        "SSEAlgorithm": "AES256"
      }
    }]
  }'
```

### Block Public Access

```bash
aws s3api put-public-access-block \
  --bucket ${S3_BUCKET} \
  --public-access-block-configuration \
    "BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true"
```

### Add Bucket Policy

```bash
cat > bucket-policy.json <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "RequireSecureTransport",
      "Effect": "Deny",
      "Principal": "*",
      "Action": "s3:*",
      "Resource": [
        "arn:aws:s3:::${S3_BUCKET}",
        "arn:aws:s3:::${S3_BUCKET}/*"
      ],
      "Condition": {
        "Bool": {
          "aws:SecureTransport": "false"
        }
      }
    }
  ]
}
EOF

aws s3api put-bucket-policy \
  --bucket ${S3_BUCKET} \
  --policy file://bucket-policy.json
```

## Cost Optimization

1. Enable S3 Intelligent-Tiering for automatic cost optimization
2. Set lifecycle policies to transition old profiles to Glacier
3. Delete profiles after retention period
4. Use S3 Storage Lens for usage insights

```bash
# Enable Intelligent-Tiering
aws s3api put-bucket-intelligent-tiering-configuration \
  --bucket ${S3_BUCKET} \
  --id EntireBucket \
  --intelligent-tiering-configuration '{
    "Id": "EntireBucket",
    "Status": "Enabled",
    "Tierings": [
      {
        "Days": 90,
        "AccessTier": "ARCHIVE_ACCESS"
      },
      {
        "Days": 180,
        "AccessTier": "DEEP_ARCHIVE_ACCESS"
      }
    ]
  }'
```

## Alternative: Using S3-Compatible Storage

For non-AWS environments or S3-compatible storage:

```yaml
spec:
  s3Config:
    bucket: my-bucket
    region: us-east-1
    prefix: profiles
    endpoint: https://minio.example.com  # Custom endpoint
```

Configure service account with appropriate credentials for your S3-compatible service.

## Cleanup

```bash
# Detach policy from role
aws iam detach-role-policy \
  --role-name ProfilingOperatorRole \
  --policy-arn ${POLICY_ARN}

# Delete role
aws iam delete-role --role-name ProfilingOperatorRole

# Delete policy
aws iam delete-policy --policy-arn ${POLICY_ARN}

# Delete S3 bucket (after emptying it)
aws s3 rm s3://${S3_BUCKET} --recursive
aws s3 rb s3://${S3_BUCKET}
```

