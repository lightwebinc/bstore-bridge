# bstore-bridge

[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

Joins an existing UHRP host (a BRC-167 complete host such as ts-stack's
`uhrp-server-basic` or `storage-server`) to the bstore storage market
without changing how that host runs.

The bridge is bstore's provider run as a market front over a bridge
backend. It receives a bucket's pieces, checks each closure, uploads it to
the incumbent through the incumbent's own upload sessions, publishes the
incumbent's own BRC-26 advertisement onto the bucket's records topic,
answers challenges by reference from the incumbent's public URL, and
passes renewals through to the incumbent's `/renew`. Bounties pay the
incumbent's key; the bridge earns challenge fees and renewal margins. It
speaks the incumbent's HTTP interfaces and imports none of its code.

```console
$ make verify
$ make bstore-bridge
$ bin/bstore-bridge init -data /var/lib/bstore-bridge
$ bin/bstore-bridge run -config bridge.json
$ make docker-build    # the image, ghcr.io/lightwebinc/bstore-bridge
```

| If you want to | Read |
| --- | --- |
| Run the bridge: what the incumbent must serve, configuration, operating, Docker, limits | [docs/bridge.md](docs/bridge.md) |
| Know the market's bounds, costs and known limits | bstore's `docs/limits.md` |
| Learn the market by role (the bridge operator's section among them) | bstore's `docs/user-guide.md` |
| The contract the bridge implements | bstore's `docs/spec.md`, section 17 |

Go 1.27. The bridge pins bstore v0.1.6, bcommon v0.9.1 and go-sdk v1.5.2;
bstore, a private module, is committed under `third_party/goproxy` (`make
bstore-module` refreshes it).

## License

Apache License 2.0. See [LICENSE](LICENSE), and [NOTICE](NOTICE) for the
third-party software and standards.
