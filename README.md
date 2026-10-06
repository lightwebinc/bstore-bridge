# bstore-bridge

Joins an existing UHRP host (a BRC-167 complete host such as ts-stack's
`uhrp-server-basic` or `storage-server`) to the bstore storage market
without changing how that host runs.

The bridge is bstore's provider run as a market front over a bridge
backend. It receives a bucket's pieces from the plane, checks each
closure, uploads it to the incumbent through the incumbent's own upload
sessions, publishes the incumbent's own BRC-26 advertisement onto the
bucket's records topic, answers challenges by reference from the
incumbent's public URL, and passes renewals through to the incumbent's
`/renew`. Bounties pay the incumbent's key; the bridge earns challenge
fees and renewal margins. It speaks the incumbent's HTTP interfaces and
imports none of its code.

```console
$ make verify
$ make bstore-bridge
$ bin/bstore-bridge init -data /var/lib/bstore-bridge
$ bin/bstore-bridge run -config bridge.json
```

- [docs/bridge.md](docs/bridge.md): what the bridge does, its security
  model, configuration, operation and limits.
- The contract it implements: bstore's `docs/spec.md`, section 17.

## License

Apache License 2.0. See [LICENSE](LICENSE).
