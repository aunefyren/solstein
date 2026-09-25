# ---------- Build ----------
# Runs on the build host's own platform and cross-compiles (CGO is off), so the
# multi-arch images don't compile under QEMU emulation.
FROM --platform=$BUILDPLATFORM golang:1.26.3-alpine AS builder
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

# TARGETVARIANT is "v7" for linux/arm/v7 (GOARM=7) and empty elsewhere.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} GOARM=${TARGETVARIANT#v} \
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
