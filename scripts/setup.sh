#!/bin/bash
# Project setup script for new developers.
# Usage: bash scripts/setup.sh

set -e

echo "=== inferplane Setup ==="

# Check prerequisites
command -v git >/dev/null 2>&1 || { echo "ERROR: git is required"; exit 1; }
command -v go  >/dev/null 2>&1 || { echo "ERROR: Go 1.25+ is required"; exit 1; }

# Fetch Go dependencies
echo "Downloading Go modules..."
go mod download

# Build the static binary
echo "Building bin/inferplane..."
CGO_ENABLED=0 go build -trimpath -o bin/inferplane ./cmd/inferplane

# Setup environment
if [ -f ".env.example" ] && [ ! -f ".env" ]; then
    echo "Creating .env from .env.example..."
    cp .env.example .env
    echo "IMPORTANT: edit .env with your actual values"
fi

# Ensure Claude hooks are executable
if [ -d ".claude/hooks" ]; then
    chmod +x .claude/hooks/*.sh 2>/dev/null || true
    echo "Claude hooks configured"
fi

echo "=== Setup Complete ==="
echo "Next steps:"
echo "  1. Edit .env with your configuration"
echo "  2. Read CLAUDE.md for project conventions"
echo "  3. Read docs/onboarding.md for the development workflow"
echo "  4. Run: go run ./cmd/inferplane serve --config examples/config.json"
