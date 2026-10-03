#!/usr/bin/env sh
# Fail closed on the static deployment supply-chain contract before an image build.
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
PROJECT_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
cd "$PROJECT_ROOT"

fail() {
    printf '%s\n' "deployment-assets: $*" >&2
    exit 1
}

require() {
    grep -Fq -- "$1" "$2" || fail "$2 is missing: $1"
}

# The image builds the console and binary from pinned manifests and runs as an
# unprivileged numeric user on Alpine.
for requirement in \
    'FROM node:24-alpine AS web' \
    'COPY web/package.json web/package-lock.json ./' \
    'RUN npm ci' \
    'FROM golang:1.26-alpine AS builder' \
    'RUN go mod download' \
    'ARG VERSION=devel' \
    'RUN CGO_ENABLED=0 go build \' \
    '-X main.version=$VERSION' \
    'FROM alpine:3.23' \
    'RUN apk add --no-cache ca-certificates tzdata \' \
    'LABEL org.opencontainers.image.version="$VERSION"' \
    'USER 65532:65532' \
    'HEALTHCHECK' \
    'ENTRYPOINT ["./kfadapter"]'; do
    require "$requirement" Dockerfile
done

# Images are published only by the CI workflow, only after verification, and
# only with SHA-pinned third-party actions.
workflow=.github/workflows/ci.yml
[ ! -e .github/workflows/publish.yml ] || fail "publishing must stay gated inside $workflow"
require '    needs: [verify, deployment]' "$workflow"
require "    if: github.event_name == 'push'" "$workflow"
require '      packages: write' "$workflow"
require 'govulncheck' "$workflow"
require 'staticcheck' "$workflow"
if grep -En 'uses: [^ ]+@v[0-9]' "$workflow"; then
    fail "every action in $workflow must be pinned to a commit SHA"
fi

# Production runs one immutable, digest-pinned image.
require '${KFADAPTER_IMAGE_DIGEST:?' compose.yaml
if grep -Eq 'image: .*:latest' compose.yaml; then
    fail "production Compose must not run a mutable tag"
fi
[ -f deploy/compose.local-build.yaml ] || fail "developer Compose file is missing"

# Secrets and generated artifacts must never enter the Docker context.
for exclusion in .git/ .github/ .env .env.\* state/ backups/ account_cred\* credentials/ secrets/ deploy/ internal/web/static/ node_modules/ web/node_modules/; do
    grep -Fqx "$exclusion" .dockerignore || fail "missing .dockerignore entry: $exclusion"
done

# Deployment assets are templates/configuration, never credential stores.
if grep -R -n -E -- '-----BEGIN( [A-Z]+)? PRIVATE KEY-----|(^|[[:space:]])(password|token|authKey|encryptKey)[[:space:]]*:' Dockerfile compose.yaml deploy .dockerignore; then
    fail "deployment assets contain credential material"
fi

printf '%s\n' "deployment-assets: passed"
