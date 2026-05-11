#!/usr/bin/env bash
# -----------------------------------------------------------------------
# deploy.sh — Build, push, and deploy Millennium OES to AWS
# Usage: ./scripts/deploy.sh [paper|prod]
# -----------------------------------------------------------------------

set -euo pipefail

ENV=${1:-paper}
AWS_REGION=${AWS_REGION:-us-east-1}
APP_NAME="millennium-oes"

echo "==> Deploying Millennium OES to environment: $ENV"

# -----------------------------------------------------------------------
# 1. Get ECR repo URL from Terraform output
# -----------------------------------------------------------------------
cd terraform
ECR_URL=$(terraform output -raw ecr_repository_url)
cd ..

echo "==> ECR: $ECR_URL"

# -----------------------------------------------------------------------
# 2. Authenticate Docker to ECR
# -----------------------------------------------------------------------
aws ecr get-login-password --region "$AWS_REGION" \
  | docker login --username AWS --password-stdin "$ECR_URL"

# -----------------------------------------------------------------------
# 3. Build and push Docker image
# -----------------------------------------------------------------------
IMAGE_TAG=$(git rev-parse --short HEAD)
FULL_IMAGE="$ECR_URL:$IMAGE_TAG"
LATEST_IMAGE="$ECR_URL:latest"

echo "==> Building image: $FULL_IMAGE"
docker build \
  --platform linux/amd64 \
  -t "$FULL_IMAGE" \
  -t "$LATEST_IMAGE" \
  .

echo "==> Pushing image..."
docker push "$FULL_IMAGE"
docker push "$LATEST_IMAGE"

# -----------------------------------------------------------------------
# 4. Update ECS service to use new image
# -----------------------------------------------------------------------
CLUSTER=$(cd terraform && terraform output -raw ecs_cluster_name)

echo "==> Updating ECS service..."
aws ecs update-service \
  --cluster "$CLUSTER" \
  --service "$APP_NAME" \
  --force-new-deployment \
  --region "$AWS_REGION" \
  --output text \
  --query 'service.serviceName'

echo "==> Waiting for deployment to stabilize..."
aws ecs wait services-stable \
  --cluster "$CLUSTER" \
  --services "$APP_NAME" \
  --region "$AWS_REGION"

echo "==> Deployment complete!"
echo "==> URL: http://$(cd terraform && terraform output -raw alb_dns_name)"
