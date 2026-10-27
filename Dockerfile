# syntax=docker/dockerfile:1.28.0@sha256:bb22d9815c728170f72750f4e5b0d672e06176142e1d602c7e66c050100b7e5b
#
# The bridge, a static binary on a distroless nonroot base:
#
#   docker build -t ghcr.io/lightwebinc/bstore-bridge .
#
# bstore is a private module. Its tagged archive is in third_party/goproxy
# and is served to the go command as a file proxy, ahead of the public one,
# as the Makefile does; go.sum holds the tag's hashes, so the copy is
# checked. The build needs no credentials.
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
ENV GOWORK=off \
    GOPROXY=file:///src/third_party/goproxy,https://proxy.golang.org,direct \
    GONOPROXY=none \
    GONOSUMDB=github.com/lightwebinc/bstore

COPY go.mod go.sum ./
COPY third_party/goproxy ./third_party/goproxy
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

FROM gcr.io/distroless/static:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
USER nonroot:nonroot
COPY --from=builder /out/bstore-bridge /usr/local/bin/bstore-bridge
COPY --from=builder --chown=65532:65532 --chmod=0700 /state/bstore-bridge /var/lib/bstore-bridge
# The licences travel with the image: the binary statically contains bstore,
# go-sdk and the other modules LICENSE-THIRD-PARTY lists.
COPY LICENSE NOTICE LICENSE-THIRD-PARTY /usr/share/doc/bstore-bridge/
EXPOSE 8096
ENTRYPOINT ["/usr/local/bin/bstore-bridge"]
