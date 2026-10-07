# The bstore bridge

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
renewal's 500 `ERR_INTERNAL_RENEW` or 404 `ERR_OLD_ADVERTISEMENT_NOT_FOUND`; the regression's answers, and the
server names no more than that to the client) is recorded with the
description's fingerprint (its SHA-256), and no commit or renewal is paid
for again while the incumbent serves the same description and the bridge
has not restarted. The catch-all also covers other failures after payment
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

## How it works

The bridge is bstore's provider (`bstore/provider`) run as a **front**
(provider backend `bridge`) over the bridge backend in this repository.

**Receiving.** Pieces and entries arrive on the front's `/submit` route,
from the plane's listener or from a writer that submits directly, and are
held to every rule a native provider holds them to: carriers, funding,
writers, quotas. Once an entry names a closure that is complete, the front
checks it (spec section 5, rows C1 to C4), recomputes the proof root, and
decides by its hold policy and the bounty's own checks, exactly as a
native provider does.

**Handing the closure over.** For a closure it takes, the front hands
every object to the backend (`Put`), which holds it to its hash and
records it as owed, keeping no bytes. The backend's `Advertise` then:

1. takes the incumbent's live advertisement of the root as it is, when one
   already lasts long enough (no upload, no payment);
2. otherwise, for a root the bridge committed before, renews it through
   the incumbent's `/renew`;
3. otherwise opens an upload session with the retention asked for, stages
   every object (read from the front's own copy, or, if the front no longer
   has it, streamed from the incumbent's own URL and held to its hash),
   and commits the root, paying the incumbent's 402 within the configured
   cap and the daily budget. A commit whose `hostedFileLocation` is not the
   incumbent's own CHIRP URL of the root is refused.

The advertisement is then found on `ls_uhrp`: by the incumbent's identity
key, the root, exactly the incumbent's CHIRP URL of the root and the root
node's length, unexpired, mined with a proof that verifies in the bridge's
own headers, and unspent by its node's positive evidence. A lookup that
fails is not taken as "no advertisement": nothing is paid on it, and the
attempt is made again later. Before a commit or a renewal is sent it is
recorded as pending; one that may have been paid (the incumbent answered
an error after the payment left, or the bridge stopped) is never paid
again while its advertisement is awaited, for a day. Before the first
advertisement of a root is returned, the bridge reads every object of the
closure from the incumbent's URL and holds it to its hash.

The front checks the advertisement once more (the advertiser's key, the
root, the backend's URL, the expiry, the root node's length, the proof),
records it, and publishes it unchanged to the records topic of every
bucket it is held for, at every configured host. Then the front drops its
own copy of the bytes. It keeps, per root, the leaf hash of every 4 KiB
segment, each blob's subtree root and the closure's object list: 32 bytes
per 4 KiB segment, 1/128 of the data, about 7.8 GB per TB.

**The front statement.** The bridge publishes a front statement (spec
section 8.2) on each bucket's records topic: its own key answers
challenges and renewals for the incumbent at the bridge's front URL. A
writer's race asks the incumbent's derived front first; an unchanged
incumbent has none, so the race moves to the fronts that state they answer
for it. A host keeps a stranger's statement a day unless it is among the
first for the advertiser (spec section 12.3), so the bridge looks for its
statement at each host every `statement_check_seconds` and publishes it
again where it is missing.

**Challenges.** The front answers `/challenge` for requests naming the
incumbent as `provider` and the bridge as `answerer`, with every sample by
reference (`segment` null, spec section 7.5): the buyer fetches each
sampled segment from the incumbent's advertised URL itself. Before it
signs, the front reads every sampled segment from the incumbent's URL
through the backend and holds it to the leaf hash it keeps. A segment the
incumbent no longer serves (404 or 410, or bytes that are not the blob's)
under every root the bridge handed that names it marks the root damaged:
the answer is `422 unknown-root`, nothing is signed, and the root is not
answered for or renewed. A passing failure (a timeout, a reset, a 5xx) is a
`503` and marks nothing. Every ten minutes the front looks again at a
damaged root whose advertisement has not expired: when the incumbent serves
every object of the closure again, and a spread of up to eight blobs read
whole hold to their hashes, the root is answered for again. The front never
restores bytes; it restores its willingness to vouch, for what it has just
read. A front answers only while the advertisement it adopted is
unexpired.

Every read goes to the incumbent's object route. Nothing is answered from a
copy: the bridge holds none, and a whole blob read once is reused for no
more than five seconds, for the other samples of the same challenge.

**Range.** After each commit, and daily, the bridge checks what the
incumbent does with an HTTP `Range`: it asks for the last 4 KiB of the
closure's largest object and compares it with the same bytes of the whole
object, held to its hash.

- **Honoured:** a sample costs one 4 KiB read, for the bridge and for every
  buyer.
- **Ignored** (the incumbent answers 200 with the whole object): a sample
  costs its whole blob, up to 4 MiB, for the bridge and for every buyer;
  a challenge of `c` samples costs up to `c` x 4 MiB of reads. The bridge
  logs it and reads whole blobs from then on. An operator in front of such
  an incumbent should price challenges to cover it
  (`terms.challenge_per_sample`).
- **Wrong** (a 206 whose bytes are not the object's): the bridge reads
  whole blobs, held to their hash, from then on. The front also rechecks
  any sample whose ranged bytes do not match from the whole blob before it
  decides anything, as a buyer does (spec section 7.4), and tells the
  backend. A buyer meeting the same wrong ranges rechecks from the whole
  blob and blames nobody.

A 206 is taken only when its `Content-Range` names exactly the range asked
for and it carries exactly that many bytes.

**What a pass proves.** A by-reference sample proves that the incumbent's
public URL served the sampled bytes when the buyer asked, whoever
answered. The bridge's signature binds only the paths, which it builds
from the hashes it keeps; it cannot make a buyer believe bytes are there
that the URL does not serve, because the buyer reads them itself, and a
sample the URL does not serve is unretrievable, which is about the
incumbent's URL (spec section 7.4).

**Renewals.** `<front>/renew` is priced by the bridge's own terms and paid
to the bridge. Once paid, the front asks the backend for the later expiry;
the backend renews through the incumbent's `/renew` (as the identity that
uploaded the root, since the incumbent's renewal is owner-only), paying
the incumbent's price within the cap, waits for the incumbent's new
advertisement (which spends the old one, so it is its successor), and the
front answers the buyer with it and publishes it. The difference between
the two prices is the bridge's margin; set `terms.renew_per_gib_day` and
`terms.renew_minimum` above what the incumbent charges. A renewal that
cannot be made at once is owed and made later, as on a native provider.
For a root the bridge did not commit (one the incumbent advertised for
its own customer), a renewal is a fresh commit by the bridge.

**Settlement.** A bounty pays the advertisement's identity key (spec
section 9.5): the incumbent's. The bridge never holds that key and takes
no bounty. A writer sends its remittance to the advertiser's own derived
route (row C24), which an unchanged incumbent does not serve, so the front
finds each settlement from public data instead: for every entry it took,
it watches the hold output and, once a spend of it is mined, appends the
spend to `remittances.jsonl` in the data directory: the transaction as
Atomic BEEF with its proof, the BRC-29 derivation prefix, the sender (the
writer) and the suffix of every rank. The bridge cannot tell which rank is
the incumbent's; only the incumbent's key can.

`bstore-bridge claim -remittances FILE -key-file F` is the incumbent's
operator's tool, run where the incumbent's key already is, on a copy of the
bridge's `remittances.jsonl` (the file goes to the key, never the key to
the bridge). It reads the key from `F` (refused unless only its owner may
read it), and prints, once for each forwarded output that pays it, the
arguments of a BRC-100 wallet's `internalizeAction` (`wallet payment`: the
transaction, the output index, the prefix, the suffix and the sender). The
key is never written anywhere.

What the bridge earns: challenge fees (paid to its own key) and the margin
on renewals. What it spends: the incumbent's commit and renewal prices,
its own fees for front statements, and the delivered bytes of every piece
on the buckets it subscribes to.

## Security

- **Where the keys live.** The bridge's own key (its identity, the key it
  authenticates to the incumbent with, signs challenge responses and front
  statements under, and pays from) is in `<data>/wallet`, mode 0700. The
  incumbent's private key never reaches the bridge: its configuration holds
  only the incumbent's public identity key. A stolen bridge key can spend
  the bridge's pool, sign answers as the bridge (whose failures are the
  bridge's own, spec section 7.4) and renew roots the bridge committed;
  it cannot touch the incumbent's bounties or advertisements.
- **Answers the incumbent did not sign.** Every authenticated answer must
  carry the incumbent's configured identity key. go-sdk's BRC-104 client
  falls back to a plain HTTP request, which follows redirects and ignores
  the bridge's bounds, when a server does not complete the handshake; the
  bridge refuses such an answer and makes no further call to the incumbent
  until it is restarted, and it makes no paid call before an unpaid one has
  seen the incumbent's key. A BRC-105 payment is made only to a server that
  authenticated. The one plain request the fallback may send is the
  residue: it carries nothing secret and its answer is discarded.
- **What the bridge fetches.** Only URLs built from its configuration and
  a hash: the incumbent's base URL with a root and an object identifier,
  the configured lookup hosts, and the configured overlay hosts. Nothing a
  request, an advertisement, a lookup answer or a front statement names is
  fetched. Redirects are not followed; every answer is bounded (4 MiB and
  a margin for an object, 64 KiB for a JSON answer, 4 MiB for a lookup
  answer); requests time out. The incumbent may be on a private address,
  since it is the operator's own configuration.
- **What the bridge mirrors.** Only an advertisement by the configured
  incumbent key, for a root whose closure the front checked and took for
  an entry, at exactly the incumbent's CHIRP URL of the root, unexpired,
  mined in the bridge's headers and unspent. A forged lookup answer can at
  worst hide an advertisement; it cannot make the bridge publish another
  host's, or one at another URL.
- **What the bridge pays.** At most the cap per commit or renewal
  (`max_sats_per_gib_day`, `min_payment`, which may not exceed what the
  front charges for the same time), and at most `max_sats_per_day` in any
  24 hours; a 402 over the cap is refused. A payment once sent is the
  incumbent's to broadcast; it is never paid twice for the same commit or
  renewal while its advertisement is awaited.

## Configuration

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
    "headers": "https://headers.example.com",
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

## Operating

```console
$ bstore-bridge init -data /var/lib/bstore-bridge
identity 02...
$ bstore-bridge address -config bridge.json
$ bstore-bridge rescan -config bridge.json -from <height>
$ bstore-bridge run -config bridge.json
$ bstore-bridge payouts -config bridge.json
$ bstore-bridge doctor -config bridge.json     # reads only: the front, the incumbent, the lookup hosts
$ bstore-bridge claim -remittances remittances.jsonl -key-file incumbent.key
```

Fund the fund address and rescan, as for a native provider: the pool pays
the incumbent's prices and the front statements' funding outputs. Behind a
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
licences. With `listen` at `0.0.0.0:8096` in the configuration, put a TLS
proxy in front as above (or set `tls_cert` and `tls_key`). `claim` belongs
where the incumbent's key is, never in the bridge's container. The image is
not published yet: build it from a checkout.

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
