# syntax=docker/dockerfile:1
# Multi-stage build for the Kerno (SoftPOS) server.
#
# Stage 1: build a static binary with the exact Go toolchain pinned in go.mod
# (go 1.27.1). GOTOOLCHAIN=local forbids silently downloading another toolchain.
FROM golang:1.27.1-alpine AS builder

ENV CGO_ENABLED=0 GOOS=linux GOTOOLCHAIN=local

WORKDIR /src

# Dependencies first (cached layer), then sources.
COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY cmd/ ./cmd/
COPY pkg/ ./pkg/
COPY migrations/ ./migrations/

# Migrations are embedded in the binary (migrations.FS); time/tzdata is
# embedded too, so the runtime image needs no zoneinfo package.
RUN go build -trimpath -ldflags="-s -w" -o /out/softpos-server ./cmd/server

# Stage 2: minimal runtime, non-root.
FROM alpine:3.22

RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 10001 app && adduser -S -D -H -u 10001 -G app app

WORKDIR /app

COPY --from=builder /out/softpos-server /app/softpos-server
# Only the Web POS static assets are served (STATIC_DIR=/app/web/pos).
COPY --chown=root:root web/pos/ /app/web/pos/

USER 10001:10001

EXPOSE 8080

HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/app/softpos-server"]
