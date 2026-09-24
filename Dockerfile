# syntax=docker/dockerfile:1
#
# Multi-stage build for packyard-server.
# Final image is distroless/static — pure Go binary, no shell, no apk/apt.

# ---- build stage ----
# Runs on the build host's platform and cross-compiles for the target,
# so multi-arch builds (buildx --platform linux/amd64,linux/arm64) need
# no emulation.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

# git is only needed to resolve git describe during the build, and ca-certificates
# so go get/mod can speak TLS. Everything else we need is in the base image.
RUN apk add --no-cache git ca-certificates

WORKDIR /src

# Cache dependencies separately from source.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
    -trimpath \
    -ldflags "-s -w -X github.com/schochastics/packyard/internal/version.Version=${VERSION}" \
    -o /out/packyard-server \
    ./cmd/packyard-server

# ---- final stage ----
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/packyard-server /usr/local/bin/packyard-server

# TLS roots for outbound HTTPS (distroless/static ships them but we make it explicit).
# Data directory is expected to be a mount point at /data.
USER nonroot:nonroot
# WORKDIR creates directories owned by USER. /backup is the mount point
# for `admin backup -out /backup/...`; owning it lets a named volume
# mounted there start out writable (distroless has no shell to chown).
WORKDIR /backup
WORKDIR /data
# Run from / so the default `-data ./data` resolves to the /data volume
# for the server and for `docker exec … admin …` alike.
WORKDIR /
VOLUME ["/data"]
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/packyard-server"]
