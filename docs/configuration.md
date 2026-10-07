# Configuration

Every flag, configuration key and image setting of `bstore-bridge`, from
the code. [bridge.md](bridge.md) is the operator's guide;
[examples.md](examples.md) the commands in order.

## Commands and flags

| Command | Flags |
| --- | --- |
| `init` | `-data DIR`: create the bridge's identity and wallet |
| `address` | `-config FILE`: print the identity, the fund address and the pool |
| `fund` | `-config FILE -beef FILE` (or `-` for standard input; bytes or hex): import a payment you sent to the fund address from the BEEF your wallet handed over, with no lookup; one not mined yet is taken when every transaction it spends is proven, sent to the settlement leg, and held in the pool until it mines, unless `-refuse-unmined`. `-txid TXID`: import a mined payment by its txid (mainnet, testnet). `-blocks N`: **coinbase: only on a regtest chain you run (development and tests)** |
| `rescan` | `-config FILE -from H [-to H]`: **coinbase: only on a regtest chain you run (development and tests)**; it finds coinbase only, never an ordinary payment |
| `run` | `-config FILE` |
| `payouts` | `-config FILE`: the settlements forwarded for the incumbent, one JSON object a line |
| `claim` | `-remittances FILE -key-file F`: run by the incumbent's operator where the incumbent's key is; `F` holds 64 hex digits or a WIF, mode 0600 |
| `doctor` | `-config FILE`, `-ca FILE` (a PEM file trusted for the front's certificate); reads only |
| `version` | |

Exit status: 0 done, 2 a usage or configuration error, 1 anything else.
The command reads no environment variables.

## Networks and endpoints

The `provider` block names the chain services, as for any bstore
provider:

| Key | Values |
| --- | --- |
| `provider.mainnet` | `true` on mainnet (the fund address form); `false` for testnet and regtest |
| `provider.node` | a Teranode asset API: proofs, spends, block hashes |
| `provider.settle` | `arcade:<url>` (with `arcade_key`), `tcp:<host:port>`, or `rpc:<url>` (with `rpc_user`, `rpc_pass`) |
| `provider.headers` | `woc:main`, `woc:test`, `chaintracks:<url>`, or a bridge URL |

## bridge.json

A JSON file; an unknown key is refused. `provider` is bstore's provider
configuration (bstore `docs/provider.md`), with backend `bridge`; its
`public_url` is the bridge's own, and its front is
`<public_url>/bstore/v1`. `export`, an inline `policy.answer_inline`, and
`policy.hold_unbountied` (each closure taken costs the bridge a commit) are
refused, and `terms.renew_per_gib_day` and `terms.renew_minimum` must be at
least the caps below, so a renewal never costs the bridge more than it is
paid.

```json
{
  "provider": {
    "data": "/var/lib/bstore-bridge",
    "listen": "127.0.0.1:8096",
    "public_url": "https://bridge.example.com/front",
    "node": "https://node.example.com",
    "settle": "arcade:https://arcade.example.com",
    "headers": "woc:main",
    "mainnet": true,
    "hosts": ["https://overlay.example.com"],
    "buckets": [{"bucket": "example_photos_hmqzvtkwrx"}],
    "backend": "bridge",
    "terms": {"renew_per_gib_day": 300, "renew_minimum": 2000}
  },
  "incumbent": {
    "url": "https://uhrp.example.com",
    "identity": "02...",
    "lookup": ["https://overlay.example.com"]
  }
}
```

| Key | Default | Meaning |
| --- | --- | --- |
| `incumbent.url` | required | the incumbent's public base URL, `https`, no trailing slash |
| `incumbent.identity` | required | the incumbent's identity key, 66 hex digits (public only) |
| `incumbent.lookup` | required | overlay hosts answering `ls_uhrp` that carry the incumbent's advertisements |
| `incumbent.ca` | none | a PEM file of certificates trusted, beside the system's, for the incumbent and the lookup hosts |
| `incumbent.max_sats_per_gib_day`, `incumbent.min_payment` | `terms.renew_per_gib_day`, `terms.renew_minimum` | the cap on one commit or renewal: the larger of `min_payment` and this rate for each GiB a day, rounded up |
| `incumbent.max_sats_per_day` | 1000000 | the most paid to the incumbent in any 24 hours |
| `incumbent.ad_wait_seconds` | 300 | how long the incumbent's advertisement is looked for after a commit or renewal before the attempt fails and is tried again |
| `incumbent.chirp_commit` | `probe` | `probe`: refuse, before paying, an incumbent build with the CHIRP commit location regression (from its API description, or from a paid commit it refused that way); `trust`: the operator states the build carries the fix (The commit build check) |
| `statement_check_seconds` | 21600 | how often the front statement is looked for at each host |

The `provider` block's other keys (`listen`, `tls_cert`, `tls_key`,
`public_url`, `hosts`, `facade`, `uhrp_topic`, `buckets`, `backend`,
`quota_bytes`, `unclaimed_bytes`, `unclaimed_seconds`, `policy.*`,
`terms.*`, `limits.*`, `tree_count`, `tree_sats`, `fee_sat_per_byte`,
`fee_floor`, `trusted_proxies`, `metrics_listen` and the `accept_`
keys) are bstore's provider configuration, with their defaults, in bstore's
[docs/provider.md](https://github.com/lightwebinc/bstore/blob/main/docs/provider.md#configure).

## The data directory

| Path | Holds |
| --- | --- |
| `<data>/wallet/` | the bridge's identity key and pool, mode 0700 |
| `<data>/bridge/` | `state.json` and one object list per root |
| `<data>/remittances.jsonl` | the settlements forwarded for the incumbent |
| the rest | the front's own state, as a bstore provider's |

Back it up: the wallet is the bridge's identity and its coin.

## Container image

Built from the [Dockerfile](../Dockerfile): the static binary on a
distroless nonroot base (uid 65532), entrypoint `bstore-bridge`, port
8096, the data directory `/var/lib/bstore-bridge` (owned by nonroot, mode
0700; mount a named volume there), and the licenses under
`/usr/share/doc/bstore-bridge/` (`LICENSE`, `NOTICE`,
`LICENSE-THIRD-PARTY`). No `ENV` defaults: mount the configuration
read-only, for example at `/etc/bstore-bridge/bridge.json`, and set
`listen` to `0.0.0.0:8096` inside a container.

`ghcr.io/lightwebinc/bstore-bridge` is published by a manual workflow
(`.github/workflows/image-publish.yml`) once a release is tagged; until
then build it with `make docker-build` or `docker build .`.

The build itself takes bstore from `third_party/goproxy` (`GOPROXY`,
`GONOPROXY`, `GONOSUMDB` are set in the Dockerfile and the Makefile), so
it needs no credentials.
