# ── Stage 1: build ────────────────────────────────────────────────────────────
FROM golang:1.26-alpine AS build
WORKDIR /build

# Copy dependency descriptors first so the module cache layer survives source
# changes.
COPY go.mod go.sum* ./
RUN go mod download

# Selective copies, never `COPY . .` — a broad copy would pull in reference/ and
# any local snapshot data. .dockerignore also excludes them, but defence in
# depth matters here: those paths can hold live buyer PII.
COPY cmd ./cmd
COPY internal ./internal

ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/server ./cmd/server

# ── Stage 2: run ──────────────────────────────────────────────────────────────
FROM alpine:3.21
WORKDIR /app

# Non-root user named after the service.
RUN addgroup -S reconciler && adduser -S reconciler -G reconciler

# Provider APIs are HTTPS-only; without root certs every outbound call fails
# with an opaque x509 error.
RUN apk add --no-cache ca-certificates

COPY --from=build /out/server /app/server

USER reconciler
EXPOSE 8080
ENTRYPOINT ["/app/server"]
