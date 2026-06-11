#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# aws/irsa-setup.sh — one-shot IRSA setup for the Bolometer operator on EKS
#
# What this script does:
#   1. Creates an IAM policy (bolometer-s3-policy) with S3 PutObject/GetObject
#   2. Creates an IAM role (bolometer-role) with a trust policy that lets
#      the bolometer ServiceAccount assume it via OIDC
#   3. Prints the helm install command to finish the deployment
#
# Prerequisites:
#   - aws CLI configured (aws configure or IAM role with sufficient permissions)
#   - eksctl  (https://eksctl.io)  OR  just the aws CLI (both paths shown)
#   - kubectl connected to your EKS cluster
#
# Usage:
#   CLUSTER_NAME=my-cluster \
#   AWS_REGION=us-west-2 \
#   S3_BUCKET=my-pprof-bucket \
#   ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text) \
#   ./aws/irsa-setup.sh
# ─────────────────────────────────────────────────────────────────────────────
set -euo pipefail

# ── Config (override via env vars) ───────────────────────────────────────────
CLUSTER_NAME="${CLUSTER_NAME:-my-cluster}"
AWS_REGION="${AWS_REGION:-us-west-2}"
S3_BUCKET="${S3_BUCKET:-my-pprof-bucket}"
ACCOUNT_ID="${ACCOUNT_ID:-$(aws sts get-caller-identity --query Account --output text)}"
NAMESPACE="${NAMESPACE:-bolometer-system}"
SERVICE_ACCOUNT="${SERVICE_ACCOUNT:-bolometer}"
ROLE_NAME="${ROLE_NAME:-bolometer-role}"
POLICY_NAME="${POLICY_NAME:-bolometer-s3-policy}"
IMAGE_TAG="${IMAGE_TAG:-0.1.0}"
IMAGE_REPO="${IMAGE_REPO:-${ACCOUNT_ID}.dkr.ecr.${AWS_REGION}.amazonaws.com/bolometer}"

echo ""
echo "=== Bolometer IRSA Setup ==="
echo "  Cluster      : $CLUSTER_NAME"
echo "  Region       : $AWS_REGION"
echo "  Account      : $ACCOUNT_ID"
echo "  S3 Bucket    : $S3_BUCKET"
echo "  IAM Role     : $ROLE_NAME"
echo "  IAM Policy   : $POLICY_NAME"
echo "  Namespace    : $NAMESPACE"
echo "  ServiceAccount: $SERVICE_ACCOUNT"
echo ""

# ── Step 1: Create the IAM policy ─────────────────────────────────────────────
echo "Step 1/3 — Creating IAM policy $POLICY_NAME ..."

POLICY_DOC=$(cat <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "BolometerUploadProfiles",
      "Effect": "Allow",
      "Action": ["s3:PutObject", "s3:GetObject"],
      "Resource": "arn:aws:s3:::${S3_BUCKET}/*"
    },
    {
      "Sid": "BolometerListBucket",
      "Effect": "Allow",
      "Action": ["s3:ListBucket", "s3:GetBucketLocation"],
      "Resource": "arn:aws:s3:::${S3_BUCKET}"
    }
  ]
}
EOF
)

POLICY_ARN=$(aws iam create-policy \
  --policy-name "$POLICY_NAME" \
  --policy-document "$POLICY_DOC" \
  --query 'Policy.Arn' \
  --output text 2>/dev/null \
  || aws iam list-policies \
       --query "Policies[?PolicyName=='${POLICY_NAME}'].Arn" \
       --output text)

echo "  Policy ARN: $POLICY_ARN"

# ── Step 2: Create the IAM role with OIDC trust policy ────────────────────────
echo "Step 2/3 — Creating IAM role $ROLE_NAME with OIDC trust ..."

# Get the OIDC provider URL for the cluster
OIDC_PROVIDER=$(aws eks describe-cluster \
  --name "$CLUSTER_NAME" \
  --region "$AWS_REGION" \
  --query "cluster.identity.oidc.issuer" \
  --output text | sed 's|https://||')

echo "  OIDC provider: $OIDC_PROVIDER"

TRUST_POLICY=$(cat <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "Federated": "arn:aws:iam::${ACCOUNT_ID}:oidc-provider/${OIDC_PROVIDER}"
      },
      "Action": "sts:AssumeRoleWithWebIdentity",
      "Condition": {
        "StringEquals": {
          "${OIDC_PROVIDER}:sub": "system:serviceaccount:${NAMESPACE}:${SERVICE_ACCOUNT}",
          "${OIDC_PROVIDER}:aud": "sts.amazonaws.com"
        }
      }
    }
  ]
}
EOF
)

ROLE_ARN=$(aws iam create-role \
  --role-name "$ROLE_NAME" \
  --assume-role-policy-document "$TRUST_POLICY" \
  --query 'Role.Arn' \
  --output text 2>/dev/null \
  || aws iam get-role \
       --role-name "$ROLE_NAME" \
       --query 'Role.Arn' \
       --output text)

echo "  Role ARN: $ROLE_ARN"

# Attach the S3 policy
aws iam attach-role-policy \
  --role-name "$ROLE_NAME" \
  --policy-arn "$POLICY_ARN"

echo "  Policy attached."

# ── Step 3: Print the helm install command ────────────────────────────────────
echo ""
echo "Step 3/3 — Done! Run the following to deploy Bolometer:"
echo ""
echo "  helm upgrade --install bolometer ./helm/bolometer \\"
echo "    -n $NAMESPACE --create-namespace \\"
echo "    -f ./helm/bolometer/values-aws.yaml \\"
echo "    --set aws.irsa.roleArn=${ROLE_ARN} \\"
echo "    --set aws.region=${AWS_REGION} \\"
echo "    --set image.repository=${IMAGE_REPO} \\"
echo "    --set image.tag=${IMAGE_TAG}"
echo ""
echo "Then create a ProfilingConfig for each app you want to profile:"
echo "  kubectl apply -f config/samples/profiling_v1alpha1_profilingconfig.yaml"
echo ""
echo "Or use the Helm chart's built-in ProfilingConfig:"
echo "  --set profilingConfig.create=true \\"
echo "  --set profilingConfig.s3.bucket=${S3_BUCKET} \\"
echo "  --set profilingConfig.selector.namespace=YOUR_APP_NAMESPACE \\"
echo "  --set 'profilingConfig.selector.labelSelector.app=YOUR_APP_LABEL'"
echo ""
