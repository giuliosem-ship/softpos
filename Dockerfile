# Multi-stage Dockerfile for SoftPOS Hybrid Fiscal & Payment Platform
# Stage 1: Build the Go executable binary
FROM golang:alpine AS builder

WORKDIR /app

# Copy dependency definitions and sources
COPY go.mod ./
COPY pkg/ ./pkg/
COPY internal/ ./internal/
COPY cmd/ ./cmd/

# Build static executable binary
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /app/softpos-server ./cmd/server

# Stage 2: Minimal runtime image
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S appgroup && adduser -S appuser -G appgroup

WORKDIR /app

# Copy binary, schema migrations, and frontend static assets
COPY --from=builder /app/softpos-server /app/softpos-server
COPY migrations/ /app/migrations/
COPY web/ /app/web/

RUN chown -R appuser:appgroup /app

USER appuser

EXPOSE 8080

HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO- http://localhost:8080/healthz || exit 1

ENTRYPOINT ["/app/softpos-server"]
