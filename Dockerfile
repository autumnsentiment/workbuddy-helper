# syntax=docker/dockerfile:1
#
# WorkBuddy Helper container build (build from this directory).
# Stage 1 compiles the static Go binary; stage 2 builds a minimal runtime image.
#
# The container runs as root by default so that bind-mounted host directories
# (e.g. ./data on Docker Desktop for Windows) are writable without extra chown.
# If you use a named volume or already chown the mount, you may add:
#   USER 10001
# before the ENTRYPOINT to run as an unprivileged user.

FROM golang:1.26-alpine AS build
WORKDIR /src
ARG APP_VERSION=1.0.3
COPY go.mod go.sum ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 go build -buildvcs=false -trimpath \
    -ldflags="-s -w -X main.appVersion=${APP_VERSION}" \
    -o /out/workbuddy-helper ./cmd/workbuddy-helper

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
    && mkdir -p /data \
    && chmod 700 /data
WORKDIR /app
COPY --from=build /out/workbuddy-helper /app/workbuddy-helper
ENV TZ=Asia/Shanghai \
    WBH_HOST=127.0.0.1 \
    WBH_DATA=/data
VOLUME ["/data"]
EXPOSE 18080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:18080/healthz >/dev/null 2>&1 || exit 1
ENTRYPOINT ["/app/workbuddy-helper"]
CMD ["--port", "18080", "--no-open"]
