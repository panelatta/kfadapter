# The console is built with the same Node major as CI. Dependency manifests are
# copied before sources so dependency layers stay cached across source edits.
FROM node:24-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY --from=web /src/internal/web/static/dist/ ./internal/web/static/dist/

ARG VERSION=devel
RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags="-s -w -X main.version=$VERSION" \
    -o /kfadapter \
    ./cmd/kfadapter

FROM alpine:3.23

RUN apk add --no-cache ca-certificates tzdata \
    && mkdir -p /kfadapter/data \
    && chown -R 65532:65532 /kfadapter \
    && chmod 0700 /kfadapter/data
WORKDIR /kfadapter
ARG VERSION=devel
LABEL org.opencontainers.image.version="$VERSION"
COPY --from=builder --chown=65532:65532 /kfadapter ./kfadapter
USER 65532:65532
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["./kfadapter", "healthcheck"]
ENTRYPOINT ["./kfadapter"]
