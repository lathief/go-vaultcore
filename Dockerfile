# syntax=docker/dockerfile:1

# ---- build stage ----
FROM golang:1.26-alpine AS builder

WORKDIR /src

# Copy manifests first so dependency download is cached independently of source.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# ---- runtime stage ----
FROM alpine:3.22

# Run as an unprivileged user; no shell access needed at runtime beyond busybox.
RUN apk upgrade --no-cache \
    && apk add --no-cache ca-certificates \
    && addgroup -S app \
    && adduser -S -G app -u 10001 app

COPY --from=builder --chown=app:app /out/server /usr/local/bin/server

USER app
WORKDIR /home/app

EXPOSE 8080
STOPSIGNAL SIGTERM

# wget from busybox keeps this working without extra packages.
HEALTHCHECK --interval=15s --timeout=3s --start-period=5s --retries=3 \
    CMD ["wget", "-qO-", "http://127.0.0.1:8080/health"]

ENTRYPOINT ["/usr/local/bin/server"]
