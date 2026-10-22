# syntax=docker/dockerfile:1

# ---- build ------------------------------------------------------------------
# go.mod's `go` line is only the minimum; build with a release that still gets security fixes.
FROM golang:1.26-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=local

# Dependencies first, so source edits do not invalidate the download layer.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
RUN go build -trimpath -ldflags="-s -w" -o /out/berth ./cmd/berth

# ---- runtime ----------------------------------------------------------------
FROM alpine:3.22
# Unprivileged user; /data holds the SQLite database (password hashes), so it
# is private to that user.
RUN addgroup -S -g 10001 berth \
 && adduser -S -u 10001 -G berth -h /data -s /sbin/nologin berth \
 && mkdir -p /data \
 && chown berth:berth /data \
 && chmod 700 /data

COPY --from=build /out/berth /usr/local/bin/berth

USER berth
ENV BERTH_LISTEN_ADDR=:8080 BERTH_DB_PATH=/data/berth.db
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["berth"]
