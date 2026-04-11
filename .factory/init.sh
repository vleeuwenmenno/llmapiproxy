#!/bin/bash
# Idempotent environment setup for LLM API Proxy OAuth mission

set -e

# Ensure config.yaml exists (copy from example if missing)
if [ ! -f config.yaml ]; then
  echo "Creating config.yaml from config.example.yaml..."
  cp config.example.yaml config.yaml
  echo "WARNING: config.yaml created with example values. Update API keys before testing."
fi

# Download dependencies
go mod download

echo "Environment setup complete."
