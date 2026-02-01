#!/bin/bash

# Exit on error
set -e

# Check if version tag is provided
if [ -z "$1" ]; then
    echo "Usage: $0 <version-tag>"
    echo "Example: $0 v1.0.0"
    exit 1
fi

VERSION=$1

# Ensure buildx is available
if ! docker buildx version > /dev/null 2>&1; then
    echo "Error: docker buildx is not available. Please install Docker Buildx."
    exit 1
fi

# Create and use a buildx builder if it doesn't exist
BUILDER_NAME="mqtt-logger-builder"
if ! docker buildx inspect $BUILDER_NAME > /dev/null 2>&1; then
    echo "Creating buildx builder: $BUILDER_NAME"
    docker buildx create --name $BUILDER_NAME --use
else
    echo "Using existing buildx builder: $BUILDER_NAME"
    docker buildx use $BUILDER_NAME
fi

# Bootstrap the builder (needed for multi-platform support)
docker buildx inspect --bootstrap

# Build and push for both amd64 and arm64
echo "Building for linux/amd64,linux/arm64..."
docker buildx build \
    --platform linux/amd64,linux/arm64 \
    -f Dockerfile \
    -t slimbean/mqtt-logger:$VERSION \
    --push \
    .

echo "Successfully built and pushed multi-arch images:"
echo "  - slimbean/mqtt-logger:$VERSION"
