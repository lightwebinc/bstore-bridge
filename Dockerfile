# syntax=docker/dockerfile:1.28.0@sha256:bb22d9815c728170f72750f4e5b0d672e06176142e1d602c7e66c050100b7e5b
#
# The bridge, a static binary on a distroless nonroot base:
#
#   docker build -t ghcr.io/lightwebinc/bstore-bridge .
#
# No ENV defaults are baked in: the incumbent, its lookup hosts, the node,
# the settlement leg and the overlay hosts are addresses of somebody's
# deployment. Pass them in the configuration file (docs/bridge.md),
# mounted read-only, for example at /etc/bstore-bridge/bridge.json.
#
# The data directory, /var/lib/bstore-bridge, is in the image, empty, owned
# by nonroot and mode 0700, so a new named volume mounted there starts with
# the same owner and mode:
#
#   docker run --rm -v bridge-data:/var/lib/bstore-bridge ghcr.io/lightwebinc/bstore-bridge init -data /var/lib/bstore-bridge
#
# The incumbent's key never belongs in this image or its volume: `claim`
# is run where that key is.

FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS builder
RUN apk add --no-cache git ca-certificates
WORKDIR /src
ENV GOWORK=off

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -eux; mkdir -p /out /state/bstore-bridge; \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
      go build -trimpath -buildvcs=false \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/bstore-bridge ./cmd/bstore-bridge

FROM gcr.io/distroless/static:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
USER nonroot:nonroot
COPY --from=builder /out/bstore-bridge /usr/local/bin/bstore-bridge
COPY --from=builder --chown=65532:65532 --chmod=0700 /state/bstore-bridge /var/lib/bstore-bridge
# The licences travel with the image: the binary statically contains bstore,
# go-sdk and the other modules LICENSE-THIRD-PARTY lists.
COPY LICENSE NOTICE LICENSE-THIRD-PARTY /usr/share/doc/bstore-bridge/
EXPOSE 8096
ENTRYPOINT ["/usr/local/bin/bstore-bridge"]
