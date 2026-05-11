# Stage 1: Builder
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /build

# Copy module files
COPY go.mod go.sum ./

# Copy source code
COPY migrations ./migrations
COPY internal ./internal
COPY services/ingest-api ./services/ingest-api

# Build binary with optimization flags
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-w -s" \
    -o ingest-api ./services/ingest-api

# Stage 2: Runtime (scratch)
FROM scratch

# Copy CA certificates for HTTPS connections
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Copy binary from builder
COPY --from=builder /build/ingest-api /bin/ingest-api

# Create non-root user (UID 1000)
# Note: scratch doesn't support useradd, so we rely on explicit UID/GID in docker-compose/k8s
USER 1000:1000

EXPOSE 8080

ENTRYPOINT ["/bin/ingest-api", "serve"]
