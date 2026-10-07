# Examples

Running a bridge in front of an incumbent UHRP host on mainnet, step by
step; testnet differs only where noted. `example.com` addresses stand for
your services. [configuration.md](configuration.md) lists every key and
flag, and [bridge.md](bridge.md) what the incumbent must serve.

## What you need

| What | Mainnet | Testnet |
| --- | --- | --- |
| An incumbent | a BRC-167 complete host with upload sessions, at a public `https` URL, and its identity key | the same, on testnet |
| Its lookup | overlay hosts answering `ls_uhrp` that carry its advertisements | the same |
| A node's asset API | a Teranode asset service you run or are given access to | the same, on testnet |
| A settlement leg | `arcade:<url>` | the same, on testnet |
| A header source | `woc:main` | `woc:test` |
| The bucket | its identifier, from the bucket's owner, and the hosts that carry it | the same |
| Coin | 100,000 satoshis or more in your own wallet | testnet coin from your own testnet wallet |

## 1. Install

```console
$ go install github.com/lightwebinc/bstore-bridge/cmd/bstore-bridge@latest
```

or take a release tarball from the
[releases page](https://github.com/lightwebinc/bstore-bridge/releases), or
build the image (below).

## 2. Create the identity

```console
$ bstore-bridge init -data /var/lib/bstore-bridge
identity 02...
```

## 3. Configure

`bridge.json`, on mainnet:

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
    "lookup": ["https://overlay.example.com"],
    "max_sats_per_day": 200000
  }
}
```

On testnet: `"headers": "woc:test"` and `"mainnet": false`.

## 4. Fund it

```console
$ bstore-bridge address -config bridge.json
identity 02...
fund address 1...
pool 0 output(s), 0 satoshis
```

Send 100,000 satoshis from your own wallet to the fund address. If your
wallet hands you the payment as BEEF, import that at once:

```console
$ bstore-bridge fund -config bridge.json -beef payment.beef
4f3c...e9a1 not mined yet (held until it mines): 1 output(s) to the fund address, 1 imported (100000 satoshis), 0 already held, 0 spent
pool 1 output(s), 100000 satoshis
```

Or, once the payment has a block, by its txid:

```console
$ bstore-bridge fund -config bridge.json -txid 4f3c...e9a1
4f3c...e9a1 mined at height <h>: 1 output(s) to the fund address, 1 imported (100000 satoshis), 0 already held, 0 spent
pool 1 output(s), 100000 satoshis
```

From the BEEF, no lookup is made: a mined payment's proof is checked
against the header source, and one not mined yet is taken when every
transaction it spends is proven (`-refuse-unmined` takes mined payments
only), sent to the settlement leg, and held until it mines. By txid, the
proof is checked against the header source and each output is asked of
the node before it is pooled. A payment that pays the fund address nothing
is refused. Importing the same payment twice adds nothing. The pool pays the incumbent's commit and renewal prices (capped
by `max_sats_per_gib_day`, `min_payment` and `max_sats_per_day`) and the
funding of the bridge's front statements.

## 5. Check, then run

```console
$ bstore-bridge doctor -config bridge.json
$ bstore-bridge run -config bridge.json
```

`doctor` reads only: the front as a provider reports it, the incumbent's
URL and build (`chirp commit`), the lookup hosts, and the settlements
forwarded. Behind a TLS proxy, route `<public_url>/bstore/v1/`,
`/.well-known/auth` at the origin, and `/submit`.

## 6. Day to day

```console
$ bstore-bridge payouts -config bridge.json          # settlements forwarded for the incumbent
$ bstore-bridge payouts -config bridge.json | wc -l
```

Give the incumbent's operator a copy of `<data>/remittances.jsonl`. Where
the incumbent's key is (never on the bridge):

```console
$ chmod 600 incumbent.key
$ bstore-bridge claim -remittances remittances.jsonl -key-file incumbent.key
```

Each line printed is the arguments of a BRC-100 wallet's
`internalizeAction` for one output that pays the incumbent.

## 7. In a container

```console
$ docker build -t bstore-bridge .
$ docker run --rm -v bridge-data:/var/lib/bstore-bridge bstore-bridge init -data /var/lib/bstore-bridge
$ docker run --rm -v bridge-data:/var/lib/bstore-bridge \
    -v "$PWD/bridge.json:/etc/bstore-bridge/bridge.json:ro" \
    bstore-bridge address -config /etc/bstore-bridge/bridge.json
$ docker run --rm -v bridge-data:/var/lib/bstore-bridge \
    -v "$PWD/bridge.json:/etc/bstore-bridge/bridge.json:ro" \
    bstore-bridge fund -config /etc/bstore-bridge/bridge.json -txid <txid>
$ docker run -d --name bstore-bridge -p 127.0.0.1:8096:8096 \
    -v bridge-data:/var/lib/bstore-bridge \
    -v "$PWD/bridge.json:/etc/bstore-bridge/bridge.json:ro" \
    bstore-bridge run -config /etc/bstore-bridge/bridge.json
```

Inside a container set `listen` to `0.0.0.0:8096`. `claim` never runs in
the bridge's container.
