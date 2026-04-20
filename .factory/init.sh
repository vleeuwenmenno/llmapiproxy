#!/bin/bash
set -euo pipefail

# Idempotent environment setup for chatv2 mission

# Build the binary if it doesn't exist or is stale
if [ ! -f llmapiproxy ] || [ cmd/llmapiproxy/*.go -nt llmapiproxy ] 2>/dev/null; then
  echo "Building llmapiproxy..."
  go build ./cmd/llmapiproxy
fi

# Verify config exists
if [ ! -f config.yaml ]; then
  echo "WARNING: config.yaml not found. Copy from config.example.yaml and fill in API keys."
fi

echo "Environment ready."
