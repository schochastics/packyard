# syntax=docker/dockerfile:1
#
# Multi-stage build for pakman-server.
# Final image is distroless/static — pure Go binary, no shell, no apk/apt.

# ---- build stage ----
FROM golang:1.25-alpine AS build

# git is only needed to resolve git describe during the build, and ca-certificates
# so go get/mod can speak TLS. Everything else we need is in the base image.
RUN apk add --no-cache git ca-certificates

WORKDIR /src

# Cache dependencies separately from source.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w -X github.com/schochastics/pakman/internal/version.Version=${VERSION}" \
    -o /out/pakman-server \
    ./cmd/pakman-server

# ---- final stage ----
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/pakman-server /usr/local/bin/pakman-server

# TLS roots for outbound HTTPS (distroless/static ships them but we make it explicit).
# Data directory is expected to be a mount point at /data.
USER nonroot:nonroot
WORKDIR /data
VOLUME ["/data"]
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/pakman-server"]
