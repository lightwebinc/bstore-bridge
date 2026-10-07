# bstore-bridge

[![CI](https://github.com/lightwebinc/bstore-bridge/actions/workflows/ci.yml/badge.svg)](https://github.com/lightwebinc/bstore-bridge/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/lightwebinc/bstore-bridge)](https://github.com/lightwebinc/bstore-bridge/releases)
[![Go Reference](https://pkg.go.dev/badge/github.com/lightwebinc/bstore-bridge.svg)](https://pkg.go.dev/github.com/lightwebinc/bstore-bridge)
[![Go version](https://img.shields.io/github/go-mod/go-version/lightwebinc/bstore-bridge)](go.mod)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

> [!WARNING]
> **Experimental software.** bstore-bridge is part of [bstack](https://github.com/lightwebinc/bstack),
> the applications and patterns built on [BSV Layered Multicast](https://github.com/lightwebinc/bsv-multicast).
> It is published to be built on and improved. Interfaces, formats and behavior may change
> rapidly between releases; pin an exact version.

Joins an existing UHRP host (a BRC-167 complete host such as ts-stack's
`uhrp-server-basic` or `storage-server`) to the
[bstore](https://github.com/lightwebinc/bstore) storage market without
changing how that host runs.

The bridge is bstore's provider run as a market front over a bridge
backend. It receives a bucket's pieces, checks each closure, uploads it to
the incumbent through the incumbent's own upload sessions, publishes the
incumbent's own BRC-26 advertisement onto the bucket's records topic,
answers challenges by reference from the incumbent's public URL, and
passes renewals through to the incumbent's `/renew`. Bounties pay the
incumbent's key; the bridge earns challenge fees and renewal margins. It
speaks the incumbent's HTTP interfaces and imports none of its code.

## Install

```console
$ go install github.com/lightwebinc/bstore-bridge/cmd/bstore-bridge@latest
```

Release tarballs for Linux and macOS (amd64, arm64) with a `SHA256SUMS`
file are on the [releases page](https://github.com/lightwebinc/bstore-bridge/releases).
The container image, `ghcr.io/lightwebinc/bstore-bridge`, is built from
the [Dockerfile](Dockerfile) ([configuration](docs/configuration.md#container-image)).

## On mainnet

```console
$ bstore-bridge init -data /var/lib/bstore-bridge
$ bstore-bridge address -config bridge.json      # pay the fund address from your own wallet
$ bstore-bridge fund -config bridge.json -txid <txid>   # once mined, import the payment
$ bstore-bridge doctor -config bridge.json       # reads only: does everything answer
$ bstore-bridge run -config bridge.json
```

`bridge.json` names the incumbent, its lookup hosts, the bucket, a node's
asset API, an arcade settlement leg and `woc:main` as the header source,
with `"mainnet": true`; testnet takes `woc:test` and `"mainnet": false`.
[docs/examples.md](docs/examples.md) has the whole file and every step.

## Documentation

| If you want to | Read |
| --- | --- |
| Run a bridge, step by step, on mainnet or testnet | [docs/examples.md](docs/examples.md) |
| Know what the incumbent must serve, operating, limits, Docker, tests | [docs/bridge.md](docs/bridge.md) |
| Look up a flag, a configuration key or the image | [docs/configuration.md](docs/configuration.md) |
| See how the bridge works and what it trusts | [docs/architecture.md](docs/architecture.md) |
| Know the market's bounds, costs and known limits | bstore's [docs/limits.md](https://github.com/lightwebinc/bstore/blob/main/docs/limits.md) |
| Learn the market by role (the bridge operator's section among them) | bstore's [docs/user-guide.md](https://github.com/lightwebinc/bstore/blob/main/docs/user-guide.md) |
| The contract the bridge implements | bstore's [docs/spec.md](https://github.com/lightwebinc/bstore/blob/main/docs/spec.md), section 17 |

## Building

```console
$ make verify
$ make bstore-bridge     # bin/bstore-bridge
$ make docker-build      # the image
```

Go 1.27.1. The bridge pins bstore v0.8.0, bcommon v0.14.0 and go-sdk
v1.7.1; bstore's tagged module is committed under `third_party/goproxy`
(`make bstore-module` refreshes it), so a build needs no credentials.

## License

Apache License 2.0. See [LICENSE](LICENSE), and [NOTICE](NOTICE) for the
third-party software and standards.
