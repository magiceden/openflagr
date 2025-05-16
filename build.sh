#!/bin/bash
set -e

# Configuration
IMAGE_NAME="021891583673.dkr.ecr.us-east-2.amazonaws.com/openflagr"
IMAGE_TAG="${1:-latest}"
PLATFORMS="linux/amd64,linux/arm64"
AWS_PROFILE="Services"
AWS_REGION="us-east-2"

# Authenticate to ECR
echo "Authenticating to ECR using AWS profile: $AWS_PROFILE"
aws ecr get-login-password --region "$AWS_REGION" --profile "$AWS_PROFILE" | \
  docker login --username AWS --password-stdin "021891583673.dkr.ecr.us-east-2.amazonaws.com"

# Build multi-architecture image
echo "Building multi-architecture image for: $PLATFORMS"
docker buildx build --platform=$PLATFORMS \
  --tag "$IMAGE_NAME:$IMAGE_TAG" \
  --push \
  .

echo "Image built and pushed: $IMAGE_NAME:$IMAGE_TAG"