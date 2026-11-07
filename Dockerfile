# The Go version to build with. The image workflows pass go.mod's; this default
# is for local builds (docker-test), so keep it in step with go.mod: the golang
# images set GOTOOLCHAIN=local, so an older Go fails instead of downloading the
# one go.mod asks for.
ARG GO_VERSION=1.27.3

# ---------- Build ----------
# Runs on the build host's own platform and cross-compiles (CGO is off), so the
# multi-arch images don't compile under QEMU emulation. That needs BuildKit
# (docker buildx, docker compose build): the legacy builder leaves
# $BUILDPLATFORM empty.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS builder
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
# Set by CI (release tag or beta-<sha>); stays "dev" for local builds.
ARG VERSION=dev
WORKDIR /app

# faster caching
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# GOARM comes from the variant (linux/arm/v7 -> 7) and only applies to
# GOARCH=arm: a variant on another architecture (linux/arm64/v8) isn't a GOARM.
RUN if [ "${TARGETARCH}" = "arm" ]; then export GOARM="${TARGETVARIANT#v}"; fi && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /app/solstein .

# ---------- Runtime ----------
FROM alpine:3.24
# PUID/PGID are read at container start by entrypoint.sh, which drops from root
# to that uid/gid, so they can be overridden with runtime environment variables.
ENV PUID=1000 PGID=1000 LANG=C.UTF-8 LC_ALL=C.UTF-8 SOLSTEIN_CONFIG_DIR=/app/config
WORKDIR /app
RUN apk add --no-cache ca-certificates tzdata su-exec
COPY --from=builder /app/solstein /app/solstein
COPY --chmod=755 entrypoint.sh /app/entrypoint.sh
# Owned by the default PUID/PGID: Docker copies this directory's ownership
# into a fresh named volume, so `user: "1000:1000"` works out of the box.
RUN mkdir -p /app/config && chown 1000:1000 /app/config
VOLUME /app/config
EXPOSE 8080
ENTRYPOINT ["/app/entrypoint.sh"]
