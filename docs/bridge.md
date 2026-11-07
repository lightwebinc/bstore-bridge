# The bstore bridge

The operator's guide. [examples.md](examples.md) has the commands in
order, [configuration.md](configuration.md) every setting, and
[architecture.md](architecture.md) how the bridge works.

`bstore-bridge` joins an incumbent UHRP host to the bstore storage market
(bstore `docs/spec.md`, section 17). The incumbent keeps running as it
does: its own key, its own advertisements, its own prices, its own
customers. The bridge adds one more ingest path (the plane) and one more
revenue line (the market) in front of it.

It follows one rule: it speaks the incumbent's HTTP interfaces, as their
published descriptions give them, and imports none of the incumbent's code.

## What the incumbent must serve

| Interface | Used for |
| --- | --- |
| BRC-167 baseline upload sessions, `POST <url>/chirp/v1/uploads`, `PUT` and `HEAD .../objects/<id>`, `POST .../commit`, over BRC-104, the commit priced by a BRC-105 402 | handing a closure to the incumbent |
| BRC-167 complete-host object route, `GET <url>/chirp/v1/<root>/objects/<object>`, public, ideally with HTTP `Range` | answering challenges, checking what the incumbent serves |
| BRC-26 advertisements of committed roots, published to `tm_uhrp` and answered by an `ls_uhrp` lookup host | the advertisement the market sees |
| The owner's `POST <url>/renew` (`uhrpUrl`, `additionalMinutes`), over BRC-104, priced by a 402 | renewal passthrough |

An incumbent without upload sessions (one that only takes the whole-file
`/quote`, `/upload`, `PUT` flow) is readable by bstore readers but cannot
be bridged: that flow advertises at a URL that is not a CHIRP route, which
the market does not count (spec section 9.3).

### In front of uhrp-server-basic

What the bridge meets in front of the reference incumbent, ts-stack
`infra/uhrp-server-basic` (checked against ts-stack `52fc646`):

- **The handshake.** The incumbent runs the TypeScript SDK's BRC-104
  middleware, which refuses a handshake whose `requestedCertificates`
  carries Go's field names as malformed. go-sdk v1.7.1, which the bridge
  pins, sends the wire shape (`certifiers`, `types`) itself.
- **An empty JSON body.** The middleware reads a body of `{}` as no body,
  so a signed `{}` fails authentication; the bridge's unpaid first call is
  `/renew` with `{"uhrpUrl":""}`, refused (400) and signed.
- **Range is ignored.** The object route answers every request 200 with
  the whole object. Price challenges for it (see Range, below).
- **CHIRP commits.** From ts-stack `b3155fa2` (2026-09-22) the server's
  advertisement metadata check accepts only a `/cdn/<object>` location, so
  every CHIRP commit fails after its payment is taken: the server logs
  "UHRP hosted file location is invalid" and answers 400
  `ERR_CHIRP_COMMIT` ("CHIRP closure validation or advertisement
  failed."), its generic commit refusal. Such an
  incumbent cannot be bridged until its operator runs a build that accepts
  the CHIRP committed-object URL. The bridge refuses it before paying
  (below, "The commit build check").

### The commit build check

Before a commit or a renewal is paid for (a renewal re-signs the
advertisement's CHIRP location, so the regression refuses it too), the
bridge reads the incumbent's API description,
`GET <url>/chirp/v1/openapi.json`, which every uhrp-server-basic with
upload sessions serves without authentication or payment. ts-stack
`b3155fa2` added the location check and, in the same change, the `bsvAuth`
security scheme to that description; no release since has removed the
check. So a description with both the commit route and `bsvAuth` is a
build in the affected range. With `incumbent.chirp_commit` `probe` (the
default):

- a build in the affected range is refused: nothing is uploaded or paid,
  and the front tries again later;
- a description that cannot be read (no answer, a 5xx, not JSON) is not
  taken as a clean build: no commit is paid for until it can be read (a
  renewal goes ahead, so a lease does not lapse for it);
- a 404 (no description) or a description without `bsvAuth` is a build
  before the range, and commits go ahead. A reverse proxy in front of the
  incumbent that does not pass `/chirp/v1/openapi.json` hides the build:
  pass it.

Whatever the description says, a payment the incumbent took and then
refused with its catch-all answer (a commit's 400 `ERR_CHIRP_COMMIT`, a
renewal's 500 `ERR_INTERNAL_RENEW`, or its 404 from a build the
description names as affected; the regression's answers, and the server
names no more than that to the client) is recorded with the description's
fingerprint (its SHA-256), and no commit or renewal is paid for again
while the incumbent serves the same description and the bridge has not
restarted. A description that cannot be read, when the payment was refused
or later, does not clear the record. A renewal's 404 from any other build
is an advertisement the incumbent's lookup does not show yet: only that
root waits (a day) before it is paid for again. The catch-all also covers other failures after payment
(a wallet or broadcast failure), so the record is held only until the
operator restarts the bridge, which says it may be tried again.

The upstream fix will not necessarily change the description, so the
check cannot tell a fixed build in the range from an affected one. An
operator whose incumbent runs a build with the fix (the upstream one when
it ships, or the one-condition patch accepting
`/chirp/v1/<root>/objects/<root>`) sets `incumbent.chirp_commit` to
`trust` and restarts the bridge: the description is then not held against
the incumbent. `bstore-bridge doctor` prints what the check finds
(`chirp commit`).

## How it works, and its security

[architecture.md](architecture.md): the bridge as a market front over a
bridge backend, what it hands the incumbent, front statements, challenges
by reference, Range, renewals, settlement forwarding, and where the keys
live.

## Configuration

[configuration.md](configuration.md): every key of `bridge.json`, every
command flag, and the image.

## Operating

```console
$ bstore-bridge init -data /var/lib/bstore-bridge
identity 02...
$ bstore-bridge address -config bridge.json
$ bstore-bridge fund -config bridge.json -beef payment.beef   # or -txid <txid> once mined
$ bstore-bridge run -config bridge.json
$ bstore-bridge payouts -config bridge.json
$ bstore-bridge doctor -config bridge.json     # reads only: the front, the incumbent, the lookup hosts
$ bstore-bridge claim -remittances remittances.jsonl -key-file incumbent.key
```

Pay the fund address from your own wallet and import the payment: from the
BEEF your wallet handed over (`fund -beef FILE`, or `-` for standard input;
it need not have mined, and its outputs wait in the pool until it does),
or by its txid once it is mined (`fund -txid`). The pool pays the incumbent's prices and the
front statements' funding outputs. (`rescan -from` and `fund -blocks` are
coinbase: only on a regtest chain you run, for development and tests.) Behind a
TLS proxy, route `<public_url>/bstore/v1/`, `/.well-known/auth` at the
origin, and `/submit`; the bridge serves no `/chirp/v1/` routes, since
readers fetch from the incumbent. State is under `<data>/bridge/`
(`state.json` and one object list per root) beside the front's own.

A front statement is minted once per bucket; a host found missing it is
sent the same statement again (twice at most) before a new one is minted,
and the statements are looked for again every `statement_check_seconds`
(every ten minutes after a failure).

## Limits

- One incumbent per bridge; one bridge may front it in many buckets.
- The upload streams the closure from the front's copy to the incumbent
  once per root; the front keeps the bytes only until the commit.
- A challenge reads every sampled segment from the incumbent before it is
  answered: its latency is the incumbent's.
- A transient incumbent outage answers challenges `503`; a 404 marks the
  root damaged for good.
- Settlements are forwarded, not taken: the incumbent's operator claims
  them with `claim`. A writer's `bstore settle` and `bstore due` list the
  incumbent's share as forwarded by this front and do not count it as
  owed; once the incumbent's wallet spends the output, the writer's node
  shows it and the payee is done. A claim alone changes nothing on chain.
- In a writer's settlement race the incumbent's own derived front is asked
  first; an unchanged incumbent has no such route, so that draw is spent,
  at once when it answers `404` and after three tries when it does not
  answer (frozen.md rows C23 and C29), and the bridge answers the second in
  the same look. The bridge has one draw, resent unchanged on a passing
  failure (row C23).
- A paid challenge the bridge refuses after the buyer's payment arrived
  (the incumbent unreachable, a damaged root) carries the bstore
  provider's signed payment-not-taken note (bstore spec section 10.4, row
  C30), so the buyer's coin is released at once.
- An object shared by two closures is read under each root that names it,
  those with an unexpired advertisement first, before it is called missing.
- With Range ignored or wrong, challenges are priced per sample as on a
  native provider while each sample may cost a 4 MiB read: an operator in
  front of such an incumbent raises `terms.challenge_per_sample`.

## Docker

```console
$ make docker-build    # ghcr.io/lightwebinc/bstore-bridge:<version> and :latest
$ make docker-smoke    # version, help, init in a named volume, a bad config refused
$ docker run --rm -v bridge-data:/var/lib/bstore-bridge ghcr.io/lightwebinc/bstore-bridge init -data /var/lib/bstore-bridge
$ docker run -d --name bstore-bridge -p 127.0.0.1:8096:8096 \
    -v bridge-data:/var/lib/bstore-bridge -v ./bridge.json:/etc/bstore-bridge/bridge.json:ro \
    ghcr.io/lightwebinc/bstore-bridge run -config /etc/bstore-bridge/bridge.json
```

The image carries the bridge alone, its data directory
(`/var/lib/bstore-bridge`, owned by the image's nonroot user) and the
licenses. With `listen` at `0.0.0.0:8096` in the configuration, put a TLS
proxy in front as above (or set `tls_cert` and `tls_key`). `claim` belongs
where the incumbent's key is, never in the bridge's container. The image is
published by the workflow [configuration.md](configuration.md#container-image)
names; otherwise build it from a checkout.

## Testing

`make verify` runs the unit tests: the backend against a stand-in
incumbent on a local test chain (commit, ranged and whole reads, Range
ignored and wrong, renewal by `/renew`, a restart, a dropped blob, the
payment cap, a foreign advertisement on the lookup host refused), and the
incumbent client's handling of what an incumbent may answer.

`make e2e REFERENCE_HOST=... BSTORE_MODULE=... NODE=...` runs the bridge
end to end: a reference overlay host loading bstore's host module, over
MySQL in Docker, on a local test chain, with a native `bstore-provider`,
the `bstore` command line as owner and writer, the bridge, and a stand-in
incumbent (`internal/fakeuhrp`), a small UHRP host written from
`uhrp-server-basic`'s published HTTP descriptions: BRC-104 on its upload
sessions and `/renew`, BRC-105 on its commit and renewal, a public object
route with `Range`, BRC-26 advertisements under its own key published to
an `ls_uhrp` lookup host, and 404 for any route it does not have.
