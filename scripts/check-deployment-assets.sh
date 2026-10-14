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

# The image builds the console and binary from pinned manifests on
# digest-pinned bases and runs as an unprivileged numeric user on Alpine.
for requirement in \
    'FROM node:24-alpine@sha256:' \
    'COPY web/package.json web/package-lock.json ./' \
    'RUN npm ci --no-audit --no-fund' \
    'FROM golang:1.26.9-alpine@sha256:' \
    'COPY go.mod go.sum ./' \
    'RUN go mod download' \
    'COPY cmd/ ./cmd/' \
    'COPY internal/ ./internal/' \
    'ARG VERSION=devel' \
    'RUN CGO_ENABLED=0 go build \' \
    '-X main.version=$VERSION' \
    'FROM alpine:3.23@sha256:' \
    'RUN apk add --no-cache ca-certificates tzdata \' \
    'LABEL org.opencontainers.image.version="$VERSION"' \
    'USER 65532:65532' \
    'HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["./chinadapter", "healthcheck"]' \
    'ENTRYPOINT ["./chinadapter"]'; do
    require "$requirement" Dockerfile
done
# Every base image, including one added later, is pinned by digest, and no
# frontend directive pulls an unpinned BuildKit syntax image.
if grep -Ein '^[[:space:]]*FROM[[:space:]]' Dockerfile | grep -Ev '@sha256:[0-9a-f]{64}([[:space:]]|$)'; then
    fail "every Dockerfile FROM must pin its image by @sha256 digest"
fi
if grep -Ein '^#[[:space:]]*syntax[[:space:]]*=' Dockerfile | grep -Ev '@sha256:[0-9a-f]{64}[[:space:]]*$'; then
    fail "a Dockerfile syntax directive must pin its frontend by @sha256 digest"
fi

# Images are published only by the CI workflow, only after every verification
# job passed, and only with SHA-pinned third-party actions and versioned tools.
workflow=.github/workflows/ci.yml
[ ! -e .github/workflows/publish.yml ] || fail "publishing must stay gated inside $workflow"
require '    needs: [verify, lint, vulncheck, deployment]' "$workflow"
require "    if: github.event_name == 'push' && (github.ref == 'refs/heads/main' || startsWith(github.ref, 'refs/tags/v'))" "$workflow"
require '      packages: write' "$workflow"
require 'golangci/golangci-lint-action@' "$workflow"
require 'govulncheck@v' "$workflow"
require 'provenance: true' "$workflow"
require 'sbom: true' "$workflow"
require 'scope=chinadapter-image' "$workflow"
require './scripts/check-migrate-legacy-state.sh' "$workflow"
if grep -En 'uses:' "$workflow" | grep -Ev 'uses: [^[:space:]@]+@[0-9a-f]{40}([[:space:]]|$)'; then
    fail "every action in $workflow must be pinned to a commit SHA"
fi
if grep -En '@latest([^0-9A-Za-z._-]|$)' "$workflow"; then
    fail "tools in $workflow must be pinned to a released version, not @latest"
fi

# The fork publishes the chinadapter image under its owner even though the
# repository keeps its pre-rename name. A release summary includes both inputs
# so copied deployments also work from another repository.
require 'IMAGE_NAME: ghcr.io/${{ github.repository_owner }}/chinadapter' "$workflow"
if grep -Fn 'ghcr.io/${{ github.repository }}' "$workflow"; then
    fail "the image must be published as <owner>/chinadapter, not under the repository name"
fi
require '${CHINADAPTER_IMAGE_REPOSITORY:-ghcr.io/panelatta/chinadapter}' compose.yaml
require 'DEFAULT_IMAGE_REPOSITORY = "ghcr.io/panelatta/chinadapter"' scripts/check-compose-security.py
require 'echo "export CHINADAPTER_IMAGE_REPOSITORY=$IMAGE_NAME"' "$workflow"
require 'echo "export CHINADAPTER_IMAGE_DIGEST=$DIGEST"' "$workflow"
require 'sudo --preserve-env=CHINADAPTER_IMAGE_REPOSITORY,CHINADAPTER_IMAGE_DIGEST scripts/preflight.sh' "$workflow"

# Production runs one immutable, digest-pinned image.
require '${CHINADAPTER_IMAGE_DIGEST:?' compose.yaml
if grep -Eq 'image: .*:latest' compose.yaml; then
    fail "production Compose must not run a mutable tag"
fi
[ -f deploy/compose.local-build.yaml ] || fail "developer Compose file is missing"
require 'name: chinadapter-local' deploy/compose.local-build.yaml
require '${CHINADAPTER_LOCAL_IMAGE:-chinadapter:local}' deploy/compose.local-build.yaml
require '${CHINADAPTER_TZ:-' deploy/compose.local-build.yaml
require '${CHINADAPTER_TZ:-' compose.yaml
for local_instructions in deploy/compose.local-build.yaml "$workflow"; do
    require 'docker compose --env-file /dev/null --project-name chinadapter-local -f deploy/compose.local-build.yaml' "$local_instructions"
done

# Deployment assets use only the chinadapter names; the pre-rename names
# appear only in the migration tooling under scripts/.
if grep -n -i 'kfadapter' Dockerfile compose.yaml config.yaml deploy/compose.local-build.yaml .dockerignore "$workflow"; then
    fail "deployment assets must use the chinadapter names"
fi

# README.md is the operator guide. It must show the digest-pinned production
# flow, the isolated developer project, and the pre-rename state migration.
for readme_requirement in \
    'export CHINADAPTER_IMAGE_REPOSITORY=ghcr.io/panelatta/chinadapter' \
    'sudo --preserve-env=CHINADAPTER_IMAGE_REPOSITORY,CHINADAPTER_IMAGE_DIGEST scripts/preflight.sh' \
    'docker compose --env-file /dev/null --project-name chinadapter-local -f deploy/compose.local-build.yaml' \
    'scripts/migrate-legacy-state.sh'; do
    require "$readme_requirement" README.md
done

# Secrets and generated artifacts must never enter the Docker context.
for exclusion in .git/ .github/ .omp/ .env .env.\* state/ backups/ account_cred\* credentials/ secrets/ deploy/ internal/web/static/ node_modules/ web/node_modules/ scripts/__pycache__/ __pycache__/; do
    grep -Fqx "$exclusion" .dockerignore || fail "missing .dockerignore entry: $exclusion"
done

# Deployment assets are templates/configuration, never credential stores.
if grep -R -n -E -- '-----BEGIN( [A-Z]+)? PRIVATE KEY-----|(^|[[:space:]])(password|token|authKey|encryptKey)[[:space:]]*:' Dockerfile compose.yaml deploy .dockerignore; then
    fail "deployment assets contain credential material"
fi

printf '%s\n' "deployment-assets: passed"
