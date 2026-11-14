# ADR: Per-tailnet aggregate DERP payload rate limits

- Date: 2026-11-12
- Status: Accepted / Implemented and locally verified; not deployed
- Audience: DERP maintainers and admission-service implementers.

## Decision and authority

This ADR specifies the wire contract, startup mode, policy selection, and
bucket lifecycle for per-tailnet aggregate policing. The local implementation,
tests, and independent post-implementation review are complete. This status does
not authorize deployment or claim a published release. Before configuring this
mode, read [the tailnet feature guide](derper-tailnet-qos.md). The existing
per-user feature remains documented in [derper-user-qos.md](derper-user-qos.md).

The design received two rounds of technical review. The subsequent independent
post-implementation review covered both implementation areas with no required
fixes. This decision supersedes the earlier
user-identity protocol draft that required `users:read`; that draft was never
implemented. The selected design requires no additional OAuth permission.
An admission service needs only `devices:core:read` for its device-list source.
It does not promise user ownership information.

## Aggregation and accounting

Each derper process shall maintain one long-lived bucket pair per logical
tailnet, keyed by an operator-configured `RateLimitSubject`. All nodes and
connections for that subject shall share its independent upload and download
token buckets. Ten tailnets shall have ten independent pairs, including when
all ten select the same `default` template. `default` is not a shared bucket.

Upload means client to derper. Download means derper to client. A transfer
within one tailnet shall consume that subject's upload once and download once.
The policer counts DERP packet payload bytes, not wire overhead. It shall drop
an over-budget payload immediately, rather than queue or shape it. It does not
guarantee fairness among nodes, a total physical egress cap, or coordination
across processes. Direct P2P, Peer Relay, and STUN are outside this policer.

The existing trusted-mesh exemption shall remain unchanged. Mesh forwarding
shall not introduce a second upload charge at the receiving relay; delivery to
a local client still incurs its download charge. Operators shall not give
ordinary third parties a mesh key as a means of admitting their clients.

## Startup mode and compatibility boundary

`--tailnet-rate-config=<path>` selects the implemented opt-in mode in the local
source tree. It is not a claim about a published image or a running deployment.
It shall be mutually exclusive with `--user-rate-config` and `--rate-config`.
The new mode shall require all of the following:

- A configured `--verify-client-url`.
- `--verify-clients=false`.
- `--verify-client-url-fail-open=false`.

An incompatible combination shall fail startup. The existing fail-open default
is `true`; implementation shall not silently change that legacy default.
Without the new flag, legacy behavior shall remain unchanged.

Local WhoIs shall not supply identity in this mode. Default v1 supplies only
network identity; the optional v2 increment below adds registering-account
identity without classifying limits by tags.
Existing owner or trusted-user exceptions
shall not migrate automatically, including any old owner unlimited policy.
Mode selection occurs at startup; a policy reload shall not switch modes.

## Policy schema and selection

The JSON configuration shall contain a `tailnets` object keyed by subject.
`default` is optional. Each policy shall contain both
`upload_bytes_per_second` and `download_bytes_per_second`. Rates shall be
canonical nonnegative integer literals in `[0, 2147483647]`. The
parser shall reject overflow, fractional or exponent notation, and incorrect
types rather than truncate them. The complete strict JSON file shall be at most
1 MiB; unknown fields and duplicate decoded field names shall be rejected.

Policy selection shall use: exact subject override, then explicit `default`,
then deny. An override shall replace the entire policy, without field merging.
A missing required field makes the configuration invalid. A missing applicable
policy means deny, not a zero rate. Zero explicitly means unlimited for that
direction. Deny, unlimited, and limited are distinct states. The schema provides
no separate deny override when a default exists.

`burst_bytes` is optional. Omission shall select `derp.MaxPacketSize`. An
explicit burst shall be at least `derp.MaxPacketSize` and at most 2147483647. The new schema does not inherit the old
schema's special zero-burst behavior.

The following is illustrative, not a default configuration or a prescribed
allowance. All rates are bytes per second. Decimal Mbps converts as
`Mbps * 1,000,000 / 8`; 30 Mbps is only an example, not a built-in allowance.

```json
{
  "default": {
    "upload_bytes_per_second": 2500000,
    "download_bytes_per_second": 5000000
  },
  "tailnets": {
    "network-a": {
      "upload_bytes_per_second": 1250000,
      "download_bytes_per_second": 7500000
    },
    "network-b": {
      "upload_bytes_per_second": 0,
      "download_bytes_per_second": 0
    },
    "network-c": {
      "upload_bytes_per_second": 0,
      "download_bytes_per_second": 1000000
    }
  }
}
```

There shall be no hard-coded 30 Mbps allowance or other implicit rate default.
Subject keys shall follow the subject grammar in the wire contract below.

## Reload and bucket lifecycle

A valid reload shall atomically apply the complete policy to existing buckets.
An invalid replacement shall retain the entire previous configuration. Removing
an override shall select `default` if present, otherwise deny. The resulting
policy shall govern subsequent payloads on already established connections.
Identity shall remain bound at connection admission; `SIGHUP` shall not
re-identify an established connection.

Each directional bucket shall obey these rules:

| Event | Required balance behavior |
|---|---|
| A new subject first appears as limited | Allow its initial burst once. |
| The same subject reconnects | Reuse its balance; do not create a full bucket. |
| A reload selects an unchanged policy | Preserve the balance. |
| A finite rate or burst changes | Settle elapsed time under the old parameters before applying the new ones. |
| A burst shrinks | Cap the balance at the new burst. |
| A burst grows | Preserve the settled balance; do not fill the added capacity. |
| Limited becomes unlimited or deny | Freeze the finite balance; do not accumulate tokens while inactive. |
| That direction returns to limited | Resume the frozen balance, subject to the new burst cap; do not refill it. |
| A subject previously existed only as unlimited or deny and has no finite bucket | Initialize its first limited balance at zero. |

These rules prevent reconnects and policy transitions from manufacturing fresh
bursts. Unlimited bypasses rate policing for that direction; deny rejects
payloads rather than impersonating an unlimited bucket.

## Versioned admission wire contract

The new mode shall send the original `NodePublic` and `Source` request fields
with the additional integer field `IdentityVersion: 1`. `Source` remains address
context, not an identity source. The service shall not use a client-declared
subject or infer a subject from a source address.

An allowing response has this shape. The node key below is a placeholder, not
a parseable key:

```json
{
  "Allow": true,
  "IdentityVersion": 1,
  "Identity": {
    "NodePublic": "nodekey:<matching-public-key>",
    "RateLimitSubject": "network-a"
  }
}
```

A denial shall use `{"Allow":false}`. `Allow:false` shall take precedence over
identity data and shall never admit a connection. The service shall not return
users, email addresses, tags, rates, or an official TailnetID.

`RateLimitSubject` is an operator-configured identifier, not Tailscale's
internal tailnet ID. It shall contain 1–64 ASCII characters from
`[A-Za-z0-9_-]`. Comparison shall be case-sensitive. Parsers shall not trim it
or implicitly lowercase it. Operators shall assign one stable subject to one
logical network and shall not recycle that subject for another network.

The trust boundary is the HTTPS admission callback authenticated by its callback
token. Derper shall retain the DERP handshake proof of possession of the node's
private key. It shall parse the echoed `NodePublic` using the canonical node-key
format and compare the parsed key to the admitted connection's key. An echo
mismatch shall reject admission; an address or an unverified client claim shall
not replace this binding.

The v1 exchange shall use a single complete JSON document of at most 4096 bytes
per body. The v1 parsers shall enforce field types and reject duplicate critical
fields, trailing JSON, unknown versions, malformed or mismatched node keys, and
invalid subjects. An allowing response without a valid v1 identity shall fail
closed. In particular, an old Worker's Allow-only response shall not downgrade
the new mode to unlimited admission.

The v1 version token shall be the JSON integer literal `1`; `1.0`, `1e0`,
strings, and numbers that round to 1 shall not count as version 1.
Only the actual absence of `IdentityVersion` shall select legacy request
handling in a new Worker. Explicit `null`, zero, and incorrect types shall
reject rather than count as absence. An absent version shall retain the
Allow-only response and the original fast-OR behavior. This v1 strictness shall
not change the old derper decoder's permissive legacy behavior.

### Compatibility matrix

| Derper / request | Admission service | Required result |
|---|---|---|
| Legacy, no version | Legacy Worker | Existing Allow-only behavior. |
| Legacy, no version | New Worker | Allow-only response; legacy fast OR and fault isolation. |
| New mode, version 1 | New Worker | Allow only with a valid matching v1 identity and an applicable policy. |
| New mode, version 1 | Legacy Allow-only Worker | Reject; no identity downgrade or unlimited fallback. |
| Explicit null, zero, wrong type, or unknown version | New Worker | Reject; not a legacy request. |
| Legacy derper receives a response | Either Worker | Preserve the old decoder behavior; do not retrofit the v1 parser. |

## Admission-service boundary and adoption

The admission service shall derive subjects from trusted configured tailnet
sources and authenticated device-list snapshots. It shall not add `users:read`
or infer user ownership. Legacy Allow-only snapshots shall not be upgraded into
v1 identity evidence. A new identity request requires a complete, valid source
configuration and an unambiguous subject across all participating sources.

Source mapping, cache validity, last-known-good snapshot adoption, and their
availability and revocation tradeoffs belong to the admission service's design.
This ADR does not define a second cache protocol or a global identity index.
The local implementation is complete; rollout remains separate work. Cross-process buckets,
CloudFront changes and site cutovers remain out of scope. The account increment
below extends identity only through explicit v2 capability; it does not change
default v1 identity or authorize a rollout.

## Account aggregation increment (2026-11-14; local only)

This section extends the parent-only v1 contract above. The account increment
has passed local verification and independent code review. It is not committed,
published, or deployed. The user-approved registering-account correction uses
`Devices.user` for both tagged and untagged devices. Its local evidence includes
298 Worker unit tests, 84 real-workerd tests, 30 joint scenarios with zero skips,
and five top-level Go compatibility tests with 44 subtests. All ten joint
listeners were reclaimed with zero cleanup errors. The independent redteam review
of the five-file implementation/test increment returned APPROVED with no required
fixes; it did not review this documentation update or authorize deployment.
Earlier full Go/race and performance measurements remain historical results, not
reruns for this correction. Its dated acceptance record is
[Issue #12](https://github.com/WooDragon/derper-plus/issues/12); completed #10
remains the record for the earlier parent-only delivery.

### Parent and account policy

A parent policy may contain `accounts`. When present, `accounts.default` and
`accounts.tagged` are required; `accounts.overrides` is optional. Every leaf
requires both directional rates and may specify `burst_bytes`, using the same
integer and burst bounds as the parent. Nulls, empty leaves, missing directions,
unknown fields, duplicate decoded names, and recursively nested `accounts` are
invalid. An override replaces the complete leaf; it does not merge fields.
Parent selection remains exact network replacement, then top-level `default`,
then deny. An exact network without `accounts` does not inherit
`default.accounts`.

User override keys shall match `h1_[a-f0-9]{64}` (67 characters). The network
subject's 64-character limit does not apply to account keys. The new Worker
projects tagged and untagged devices with the same valid `Devices.user` into the
same `(network,user)` child bucket pair, using the unchanged HMAC derivation.
`Devices.user` identifies the registering account, not the tagged device's
current owner. Tags do not affect rate classification. Registry keys remain
structured `(network,kind,key)` and isolated across networks.

The server shall retain `(tagged,shared)` identity and `accounts.tagged` policy
for compatibility with existing evidence. That compatibility identity selects
a separate shared pair within the network. The new Worker shall not project
it; its registering-account identities select user overrides or
`accounts.default`, not `accounts.tagged`.

Both layers apply in each direction:

| Parent | Account | Effective constraint |
|---|---|---|
| Limited | Zero | Parent only |
| Zero | Limited | Account only |
| Zero | Zero | Unlimited |
| Limited | Limited | Both must allow |
| Deny | Any | Deny; an account cannot restore access |

Absent `accounts` retains parent-only policing. Zero removes only that layer's
directional limit; it is not an exemption from the parent. This remains payload
policing without fair scheduling or a minimum bandwidth guarantee.

### Fixed process capability and v2 wire

`--tailnet-account-identity` defaults to false. False sends v1 and rejects any
configuration containing `accounts`, including unmatched entries, on both
initial load and reload. True is valid only with `--tailnet-rate-config` and
requires full v2 identity for every non-mesh connection, even when its selected
parent has no account policy. Capability validation also applies to server
configuration APIs. Initialization fixes the capability; it cannot change once
the registry is initialized or Accept has started. A process change, not SIGHUP,
is required to change this capability. Existing rate-mode exclusivity, local
WhoIs exclusion, and trusted-mesh exemption remain unchanged.

A v2 request uses literal `IdentityVersion: 2` with the existing `NodePublic` and
`Source`. An allowing response has the following shape; the node key is a
placeholder, not a parseable key:

```json
{"Allow":true,"IdentityVersion":2,"Identity":{"NodePublic":"nodekey:<matching-public-key>","RateLimitSubject":"network-a","Account":{"Kind":"user","Key":"h1_596e6e2f00ef312162a11e49aee4fefd8f46b9ad117dc26307bbe80c6401f987"}}}
```

The account key in this example is a synthetic fixture. The retained compatibility
identity is `{"Kind":"tagged","Key":"shared"}`. The server shall still accept it,
but the new Worker shall emit `Kind:user` for both tagged and untagged devices.
Denial remains `{"Allow":false}`.
The v2 parser checks complete whitelists, types, presence, duplicate decoded
fields, the 4096-byte body limit, and the canonical node-key echo at response,
Identity, and Account boundaries. `2.0`, `2e0`, strings, v1 responses, and
Allow-only responses cannot downgrade v2. The legacy/v1 decoders retain their
existing behavior; v2 strictness is not retroactive.

### Evidence, pseudonyms, and key maintenance

The new Worker uses valid `Devices.user` from `devices:core:read` as the
registering account for both tagged and untagged devices. It does not call the
Users API or infer current owner/admin status. Tags do not affect account
projection, including when tag fields are missing or malformed. Both device
classes require a nonempty user string with no leading/trailing whitespace or
control characters and at most 1024 UTF-8 bytes. Missing or invalid user evidence
shall yield unknown and reject v2, without falling back to shared. No trimming,
lowercasing, Unicode normalization, or alias merging occurs. Valid legacy/v1
membership remains unchanged.

The service's `ACCOUNT_IDENTITY_KEY` is 64 lowercase hex characters decoded into
32 HMAC key bytes. A user key is `h1_` plus lowercase HMAC-SHA-256 hex over UTF-8
`JSON.stringify(["derp-account-v1",networkSubject,user])`. JSON array encoding,
including escaping, is required; delimiter concatenation is not equivalent.
The synthetic vectors in
[testdata/account-vectors.json](../derp/derpserver/testdata/account-vectors.json)
cover cross-network isolation, case, escaping, and Unicode distinctions. Raw
user/email strings shall not enter snapshots, wire responses, or request logs.
HMAC provides keyed pseudonymization, not encryption or stable account IDs.

The service retains `source:v2:` and base snapshot schema version 2. Optional
`accountEvidence:{version:1,context:<64 lowercase hex>}` and per-node account
objects extend it. Context is the HMAC of UTF-8
`JSON.stringify(["derp-account-context-v1",networkSubject])` under the same key.
Same-network sources with different OAuth credentials yield the same context
and account key. v2 requires current metadata for every participating source,
valid base snapshots, and consistent identities across every matching unexpired
record, including duplicate node keys. Unknown/conflicting matches reject.
Unknown evidence on an unrelated node does not itself reject a known target.

A successful refresh replaces old account evidence even when the new evidence
is unknown; API failure retains the complete LKG. Existing shared snapshots and
valid contexts remain legal compatibility evidence; the projection correction
does not invalidate them automatically. Before a consumer cutover, all configured
sources shall complete a real successful refresh under the new Worker. Operators
shall then read back v2 `Kind:user` and verify the same key for tagged and
untagged devices with the same registering account. A valid context, an old
shared LKG, or a health response alone does not establish readiness.

Old snapshots or invalid account additions do not invalidate otherwise valid
v1 membership. Missing or invalid secret leaves legacy/v1 operational but rejects
v2. An old Worker may serve v1 from extended snapshots; if its refresh removes
account metadata, v2 rejects instead of falling back. No identity is synthesized
from old snapshots.

No production account key was generated or uploaded for this increment.
Production adoption requires separately approved secure provisioning and a
recoverable protected backup outside the sole control machine. Operators shall
keep secret bytes out of command arguments, source control, and logs. Restoring
snapshots without the corresponding key does not restore v2 readiness. Rotation
changes keys, overrides, and bucket identities while established connections
retain their old identity. Rotation therefore requires disabling account
capability and coordinating migration; it is not transparent hot rotation.
This ADR specifies the obligation, not an authorized rollout procedure.

### Atomic accounting and retained history

Trusted admission registers parent and account buckets before packet handling.
v2 registers unlimited account history even while `accounts` is absent; trusted
identities denied by the parent retain deny history. The packet path performs
bucket lookups but never creates buckets. Admission and registration retain full
identity validation; the packet path does not repeat string validation per
payload.

Within one registry mutex and one timestamp, each directional check settles both
layers, checks all active limits, then debits both finite balances only if all
allow. A denial may settle time but debits neither layer. Upload charges the
sender and download charges the receiver; this is not a transaction across the
entire forwarding path. Mesh ingress avoids duplicate upload; local delivery
still consumes parent and account download allowances.

Parsing and fixed-capability validation occur outside the registry lock. A valid
reload updates all parent/account buckets and publishes the policy under one
lock and timestamp; invalid input changes nothing. The existing lifecycle table
applies to both layers. Closing `accounts` freezes child finite history through
unlimited; reopening it resumes without refill. Parent removal selects deny
unless a default applies. Reconnects never create a fresh burst. A new limited
identity receives one initial burst; an identity previously registered only as
unlimited/deny starts its first limited balance at zero.

Historical buckets are not garbage-collected. Reload work remains
O(parent buckets + account buckets), and the server lock remains held during
application. The measured reload cost is accepted as nonblocking, not fixed;
measurements belong to the dated issue, not a production pause guarantee. Other
limits remain mutable/reused user strings, indefinite LKG revocation delay,
connection-bound identity, and per-process rather than distributed quotas.

## Acceptance coverage and delivery status

The completed local tests cover the following design obligations:

- The complete compatibility matrix, including malformed version values and
  an Allow-only response to a v1 caller.
- Ten subjects with independent bucket pairs, including ten uses of `default`;
  exact overrides, explicit one-way and two-way zero rates, and missing-policy deny.
- Reloads on established connections; unchanged balances, finite parameter
  settlement, burst caps, frozen balances, and no reconnect refill.
- Stable subjects during source and OAuth rotation; conflicting duplicate
  configuration; order-independent source aggregation.
- API-failure last-known-good evidence, missing snapshots as unknown, successful
  empty lists, device expiry, and rejection of legacy snapshot upgrades.
- Node-key binding; strict parsing negatives; denial precedence; unchanged
  trusted-mesh accounting and exemption.

Local verification includes the Worker unit and real-workerd suites, fork
regression and race suites, and a real Worker/fork joint run. That joint run
exercised a real binary's SIGHUP against established connections. Independent
post-implementation review is complete. Dated counts and build limitations are
recorded in existing [Issue #10](https://github.com/WooDragon/derper-plus/issues/10).
These results do not establish production adoption or authorize rollout.

The local source content was synchronized to verified upstream stable v1.104.6
(`c54807352ae196919497b3f8fddc3cedf88368b9`, released 2026-11-11), with Go 1.27.3
and a matching builder. This is content synchronization, not a history merge,
a commit to `stable`, or a published release. The original HEAD, index, and refs
were preserved. The three added upstream workflows restrict every job to the
official repository. No automatic tracking task was established.
