# syntax=docker/dockerfile:1.29.0@sha256:572cebbc2b15e6f84c077742952674889cb63cd5e9b80abdffcbe587b6f19183
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

FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS builder
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

FROM gcr.io/distroless/static:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
USER nonroot:nonroot
COPY --from=builder /out/bstore-bridge /usr/local/bin/bstore-bridge
COPY --from=builder --chown=65532:65532 --chmod=0700 /state/bstore-bridge /var/lib/bstore-bridge
# The licences travel with the image: the binary statically contains bstore,
# go-sdk and the other modules LICENSE-THIRD-PARTY lists.
COPY LICENSE NOTICE LICENSE-THIRD-PARTY /usr/share/doc/bstore-bridge/
EXPOSE 8096
ENTRYPOINT ["/usr/local/bin/bstore-bridge"]
