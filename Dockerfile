# syntax=docker/dockerfile:1

# ---------------------------------------------------------------- build ----
FROM golang:1.27-alpine AS build

ARG VERSION=dev
WORKDIR /src

# Dependencies first so a source-only change reuses the module layer.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

# CGO_ENABLED=0 keeps the binary static: the SQLite driver is pure Go, so the
# runtime image needs no libraries at all.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X github.com/mvtp/qrurl/internal/server.BuildVersion=${VERSION}" \
      -o /out/qrurl ./cmd/qrurl \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" \
      -o /out/qrdecode ./cmd/qrdecode

# ----------------------------------------------------------------- test ----
# Built only when requested: `docker build --target test .`
FROM build AS test
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go vet ./... && go test ./...

# ------------------------------------------------------------- runtime ----
FROM alpine:3.22 AS runtime

# tzdata: the service buckets "today" in a configurable timezone.
# ca-certificates: not needed by the service itself, but keeps the image
#   usable for an operator running curl inside it.
# wget comes from busybox and drives the healthcheck.
RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -g 10001 -S qrurl \
 && adduser -u 10001 -S -G qrurl -H -s /sbin/nologin qrurl \
 && mkdir -p /data/qr /data/backups \
 && chown -R qrurl:qrurl /data

COPY --from=build /out/qrurl /usr/local/bin/qrurl
COPY --from=build /out/qrdecode /usr/local/bin/qrdecode

USER qrurl:qrurl
WORKDIR /data
VOLUME ["/data"]

ENV QRURL_ADDR=0.0.0.0:8013 \
    QRURL_DATA_DIR=/data \
    QRURL_TIMEZONE=Asia/Seoul \
    QRURL_LOG_FORMAT=json

EXPOSE 8013

# The container listens on all interfaces because Docker networking, not the
# process, is what keeps it private: the port is never published to the host,
# only reachable from the reverse proxy on the shared network.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8013/health | grep -q '"status":"ok"' || exit 1

ENTRYPOINT ["/usr/local/bin/qrurl"]
CMD ["serve"]
