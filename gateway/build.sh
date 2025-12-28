#!/bin/bash
# Build the Instagram gateway

set -e

cd "$(dirname "$0")"

echo "Building Instagram gateway..."

# Handle macOS homebrew library paths if needed
if [[ -z "$LIBRARY_PATH" && -d /opt/homebrew ]]; then
    echo "Using /opt/homebrew for LIBRARY_PATH and CPATH"
    export LIBRARY_PATH=/opt/homebrew/lib
    export CPATH=/opt/homebrew/include
fi

# Download dependencies
go mod download

# Build the gateway binary
go build -o ig-gateway ./cmd/ig-gateway

echo "Build complete! Binary: ./gateway/ig-gateway"
echo ""
echo "To run the gateway:"
echo "  ./ig-gateway --cookies /path/to/cookies.json"
echo ""
echo "Or set the IG_COOKIES_FILE environment variable:"
echo "  export IG_COOKIES_FILE=/path/to/cookies.json"
echo "  ./ig-gateway"
