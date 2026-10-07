# GOWORK=off everywhere on purpose: a workspace would resolve bstore or
# bcommon to a local checkout, so a go.mod skew would stay invisible here.
# What is tested is what go.mod pins.
#
# bstore is a private module: the public proxy and checksum database cannot
# see it, and a build with no credentials for its repository cannot fetch
# it. So the tagged module archive is kept under third_party/goproxy and
# served to the go command as a file proxy, ahead of the public one. It is
# not a replace and not a vendor directory: go.mod requires the tag, go.sum
# holds the tag's hashes, and the go command refuses an archive that does
# not match them. Every target below, and CI, resolve bstore this way, so
# what is tested is the same bytes everywhere. make bstore-module refreshes
# the copy from the repository after go.mod moves to another tag.
BSTORE := github.com/lightwebinc/bstore
export GOPROXY := file://$(CURDIR)/third_party/goproxy,https://proxy.golang.org,direct
export GONOPROXY := none
export GONOSUMDB := $(BSTORE)

.PHONY: verify test vet fmt-check deps-check licences licences-update bstore-module bstore-bridge e2e docker-build docker-smoke

NODE ?= node
IMAGE ?= ghcr.io/lightwebinc/bstore-bridge
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

verify: fmt-check vet deps-check licences test

test:
	GOWORK=off go test -race -count=1 ./...

vet:
	GOWORK=off go vet ./...

fmt-check:
	@test -z "$$(gofmt -l .)" || { echo "gofmt:"; gofmt -l .; exit 1; }

# The direct dependencies, asserted rather than intended:
#  - go.mod and go.sum are tidy, and no module is replaced;
#  - the direct set is exactly go-sdk, bcommon and bstore;
#  - go-sdk resolves to exactly v1.7.1, the version bcommon and bstore pin.
DEPS_DIRECT := github.com/bsv-blockchain/go-sdk github.com/lightwebinc/bcommon $(BSTORE)
DEPS_SDK    := github.com/bsv-blockchain/go-sdk v1.7.1

deps-check:
	@set -eu; \
	if ! GOWORK=off go mod tidy -diff >/dev/null; then \
		echo "deps-check: go.mod or go.sum is not tidy; run go mod tidy"; exit 1; \
	fi; \
	replaced=$$(GOWORK=off go list -m -f '{{if .Replace}}{{.Path}}{{end}}' all); \
	if [ -n "$$replaced" ]; then echo "deps-check: no module may be replaced, found: $$replaced"; exit 1; fi; \
	direct=$$(GOWORK=off go list -m -f '{{if not (or .Main .Indirect)}}{{.Path}}{{end}}' all | sed '/^$$/d' | sort); \
	want=$$(printf '%s\n' $(DEPS_DIRECT) | sort); \
	if [ "$$direct" != "$$want" ]; then \
		echo "deps-check: the direct dependencies must be exactly:"; echo "$$want" | sed 's/^/  /'; \
		echo "found:"; echo "$$direct" | sed 's/^/  /'; exit 1; \
	fi; \
	sdk=$$(GOWORK=off go list -m $(firstword $(DEPS_SDK))); \
	if [ "$$sdk" != "$(DEPS_SDK)" ]; then echo "deps-check: go-sdk must resolve to exactly $(lastword $(DEPS_SDK)), found: $$sdk"; exit 1; fi

# Refresh third_party/goproxy from the repository at the tag go.mod pins
# (needs credentials for the private repository).
bstore-module:
	@set -eu; v=$$(GOWORK=off go list -m -f '{{.Version}}' $(BSTORE)); \
	tmp=$$(mktemp -d); trap 'chmod -R u+w "$$tmp"; rm -rf "$$tmp"' EXIT; \
	GOWORK=off GOFLAGS=-mod=mod GOMODCACHE=$$tmp GOPROXY=direct GONOSUMDB=$(BSTORE) go mod download $(BSTORE)@$$v; \
	src=$$tmp/cache/download/$(BSTORE)/@v; dst=third_party/goproxy/$(BSTORE)/@v; \
	mkdir -p $$dst; cp $$src/$$v.mod $$src/$$v.zip $$dst/; \
	GOWORK=off GOFLAGS=-mod=mod GOMODCACHE=$$tmp GOPROXY=direct GONOSUMDB=$(BSTORE) go list -m -f '{"Version":"{{.Version}}","Time":"{{.Time.UTC.Format "2006-01-02T15:04:05Z"}}"}' $(BSTORE)@$$v > $$dst/$$v.info; \
	ls $$dst | sed -n 's/\.zip$$//p' > $$dst/list; \
	echo "bstore-module: $$v in $$dst"

bstore-bridge:
	GOWORK=off go build -ldflags "-X main.version=$(VERSION)" -o bin/bstore-bridge ./cmd/bstore-bridge

# LICENSE-THIRD-PARTY is generated from what the binary links: every
# module's licence text travels with a binary or an image built here.
# licences fails when the file is stale.
licences:
	GOWORK=off python3 scripts/gen-third-party-licenses.py . --check

licences-update:
	GOWORK=off python3 scripts/gen-third-party-licenses.py .

# The bridge end to end (docs/bridge.md): a reference overlay host loading
# bstore's host module bundle, over MySQL (Docker), on a local test chain,
# with a native bstore-provider, the bstore command line, the bridge, and a
# stand-in incumbent speaking the uhrp-server-basic HTTP shape. Not part
# of verify. The bundle is bstore's host/bundle/bstore-module.js, built
# there by make module at the tag go.mod pins; NODE names a Node 24 binary.
#
#	make e2e REFERENCE_HOST=/path/to/reference-host BSTORE_MODULE=/path/to/bstore-module.js NODE=/path/to/node24
e2e:
	@test -n "$(REFERENCE_HOST)" || { echo "set REFERENCE_HOST=/path/to/reference-host"; exit 1; }
	@test -n "$(BSTORE_MODULE)" || { echo "set BSTORE_MODULE=/path/to/bstore-module.js (bstore's host module bundle)"; exit 1; }
	BSTORE_BRIDGE_E2E_REFHOST=$(abspath $(REFERENCE_HOST)) BSTORE_BRIDGE_E2E_MODULE=$(abspath $(BSTORE_MODULE)) \
		BSTORE_BRIDGE_E2E_NODE=$(abspath $(shell command -v $(NODE))) \
		GOWORK=off go test -count=1 -timeout 45m -run '^TestBridgeE2E$$' -v ./cmd/bstore-bridge/

# The image, built and tagged locally as $(VERSION) and latest. Nothing
# here pushes.
docker-build:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .
	@docker image ls --format '{{.Repository}}:{{.Tag}}  {{.Size}}' | grep -E '^$(IMAGE):$(VERSION) '

# The image with no network at all: the stamped version, help, a first
# run in a fresh named volume mounted where the documentation says, and a
# configuration with an unknown key refused.
docker-smoke: docker-build
	@set -eu; img=$(IMAGE):$(VERSION); id=bstore-bridge-smoke-$$$$; \
	docker volume create $$id >/dev/null; trap 'docker volume rm -f $$id >/dev/null' EXIT; \
	echo "== version"; out=$$(docker run --rm --network none $$img version); echo "$$out"; \
	[ "$$out" = "bstore-bridge $(VERSION)" ] || { echo "docker-smoke: want bstore-bridge $(VERSION)"; exit 1; }; \
	echo "== help"; docker run --rm --network none $$img -h | head -1; \
	echo "== init in a named volume"; docker run --rm --network none -v $$id:/var/lib/bstore-bridge $$img init -data /var/lib/bstore-bridge | grep '^identity '; \
	echo "== a configuration with an unknown key is refused"; \
	rc=0; out=$$(printf '{"nope":1}' | docker run --rm -i --network none $$img run -config /dev/stdin 2>&1) || rc=$$?; echo "$$out"; \
	[ "$$rc" = 2 ] || { echo "docker-smoke: want exit 2, got $$rc"; exit 1; }; \
	echo "docker-smoke: ok ($$img)"
