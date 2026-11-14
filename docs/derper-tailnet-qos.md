# DERP per-tailnet aggregate payload limits

This guide is for DERP operators who use a trusted external admission service.
The mode is implemented and locally verified. It has not been deployed or
published as part of this delivery. Existing images and deployments shall not
be assumed to contain these flags.

Before changing the protocol or policy lifecycle, read
[the accepted ADR](adr-derper-tailnet-aggregate-rate-limit.md). For local WhoIs
and user-based exceptions instead, read [the per-user guide](derper-user-qos.md).
The two modes are separate and mutually exclusive.

## Identity and accounting

`RateLimitSubject` identifies one logical tailnet. The operator assigns a stable,
case-sensitive identifier of 1–64 ASCII characters from `[A-Za-z0-9_-]`.
It is not an official Tailscale ID or a user ID. The operator shall not reuse a
subject for another network. A subject change requires draining the old
subject's connections first; existing connections retain their admitted identity.

All nodes and connections with that subject share one upload bucket and one
download bucket in this derper process. Each subject has its own pair, even
when several subjects use `default`. Upload means client to derper; download
means derper to client. A same-tailnet transfer consumes each direction once.
The policer counts DERP packet payload bytes and immediately drops excess
payloads. It does not queue traffic or guarantee fairness among nodes.

The limits are per process, not a network-wide total. Wire overhead, direct P2P,
Peer Relay, and STUN are outside these limits. Trusted mesh peers remain exempt.
Mesh forwarding avoids a second upload charge at the receiving relay; delivery
to its local client still consumes that client's subject download allowance.
Ordinary third parties shall not receive mesh keys for admission.

By default, the callback sends `NodePublic`, `Source`, and literal `IdentityVersion: 1`.
The fixed opt-in account capability below selects literal version 2 instead.
An allowing response must contain a matching node-key echo and a valid subject.
The DERP handshake still proves possession of the node's private key. Local
WhoIs, source addresses, user ownership, and old owner exceptions do not supply
the subject. The admission service needs only `devices:core:read`, not `users:read`.
An Allow-only response, malformed identity, unknown version, callback error, or
missing policy rejects admission. Strict v1 bodies are complete JSON documents
of at most 4096 bytes. The ADR defines the full compatibility matrix.

## Local configuration interface

The strict JSON file is at most 1 MiB. `tailnets` is required; `default` is
optional. An exact subject override replaces the complete policy. Otherwise the
server uses explicit `default`, or denies when neither exists. A default is a
policy template, not a shared bucket. There is no deny override under a default.

Each policy requires `upload_bytes_per_second` and `download_bytes_per_second`.
Both use canonical integer literals in `[0, 2147483647]`. Zero explicitly means
unlimited for that direction; omission does not mean zero. Unknown fields,
duplicate decoded names, nulls, fractional or exponent notation, and overflow
are invalid. Optional `burst_bytes` defaults to `derp.MaxPacketSize` when omitted.
An explicit burst must be at least `derp.MaxPacketSize` and at most 2147483647;
zero is invalid in this schema.

This example contains fictitious subjects, not production allowances:

```json
{
  "default": {
    "upload_bytes_per_second": 1250000,
    "download_bytes_per_second": 2500000
  },
  "tailnets": {
    "network-a": {
      "upload_bytes_per_second": 0,
      "download_bytes_per_second": 1000000
    }
  }
}
```

Rates are bytes per second. Decimal Mbps converts as `Mbps * 1000000 / 8`.
No 30 Mbps or other implicit rate default exists. With no default and an empty
`tailnets` object, the configuration denies every subject.

The startup combination requires all four settings:

- `--tailnet-rate-config=<local-policy-path>`
- `--verify-client-url=<trusted-callback-URL>`
- `--verify-clients=false`
- `--verify-client-url-fail-open=false`

These are interface placeholders, not a deployment command. Operators shall
keep callback tokens out of documentation, logs, and version control. The new
flag rejects either `--user-rate-config` or `--rate-config`. Invalid combinations
fail startup before serving. Legacy fail-open still defaults to true; this mode
requires an explicit false rather than changing that default.

## Reload and rollback boundary

After replacing the complete policy file, send SIGHUP only to the intended
local derper process. A valid reload atomically applies to subsequent payloads
on existing connections. An invalid replacement retains the complete previous
policy. Reload does not re-identify clients or switch startup modes.

Reconnects reuse the subject's balances. Unchanged policies preserve balances.
Finite changes first settle elapsed time using the old parameters. A smaller
burst caps the balance; a larger burst does not fill its new capacity. Unlimited
or deny freezes a finite balance without accrual. Returning to limited resumes
that balance under the new cap. A subject that previously had only unlimited or
deny starts its first finite balance at zero. The ADR is authoritative for the
complete lifecycle table.

Rolling back the startup mode requires a process restart with the intended old
flags and policy; an empty file or SIGHUP cannot switch modes. Production
rollout, prewarming, and rollback require separate approval. In particular, a
new Worker reads only `source:v2:` snapshots: old v1 evidence is neither upgraded
nor used as fallback. Before the first successful complete refresh, both wire
paths can cold-deny. Infinite last-known-good retention still enforces device
expiry and can delay revocation indefinitely. A health response alone does not
prove admission readiness. No existing connection is revoked by a refresh.

## Optional account layer (local increment, not deployed)

Before configuring this increment, read the
[account contract in the ADR](adr-derper-tailnet-aggregate-rate-limit.md#account-aggregation-increment-2026-11-14-local-only).
The parent-only callback above remains v1 by default. The new
`--tailnet-account-identity` capability defaults to false and is fixed at startup.
False rejects every `accounts` entry at initial load or reload. True requires
`--tailnet-rate-config` and full v2 evidence for all non-mesh clients, including
networks whose policy has no account layer. SIGHUP cannot change the capability.
Missing/unknown account evidence rejects v2; it never falls back to v1.

The following policy uses a synthetic account and fictitious subject. It is an
interface example, not production configuration or a rollout command:

```json
{
  "tailnets": {
    "network-0": {
      "upload_bytes_per_second": 10485760,
      "download_bytes_per_second": 10485760,
      "accounts": {
        "default": {
          "upload_bytes_per_second": 1048576,
          "download_bytes_per_second": 1048576
        },
        "overrides": {
          "Synthetic.User@example.test": {
            "upload_bytes_per_second": 0,
            "download_bytes_per_second": 0
          }
        }
      }
    }
  }
}
```

`accounts.default` is mandatory when accounts is present; `overrides` may be
omitted. These are the only accounts fields. Overrides replace complete leaves.
Leaf integer and burst validation matches the parent, and nested accounts is
invalid. An exact network policy without accounts does not inherit the top-level
default's accounts. An override key is the exact valid registering-account
string from `Devices.user`, not a derived key or a network subject. Operators
shall configure it directly under the intended network without trimming,
lowercasing, Unicode normalization, or alias merging.

The Worker sends `Account:{Kind:"user",Key:<raw registering account>}` for tagged
and untagged devices. `Devices.user` identifies the registering account, not the
current owner. Devices with the same `(network,user)` share one account bucket
pair; the same string in different networks remains isolated. Tags do not affect
rate classification. The undeployed `tagged/shared` identity and `accounts.tagged`
leaf have been removed; valid identities select a user override or
`accounts.default`.

In the example, the zero-rate user still obeys the network's 10485760-byte/s
parent in each direction. A zero parent with a finite child obeys the child;
only two zero layers remove both limits. A denied parent stays denied. Both
finite layers must allow before either is debited. Different accounts compete
for the parent; the policer does not guarantee fairness or minimum bandwidth.

A true-capability process may add, tighten, close, and reopen accounts by valid
policy reload while connections retain their admitted identity. Parent and
child balances follow the ADR's freeze/resume/no-refill rules. Closing accounts
retains historical child buckets. Reconnects do not replenish balances; buckets
are not garbage-collected. Mode changes require a different process, not an
empty policy or a reload.

A valid account is nonempty, has no leading/trailing whitespace (including U+FEFF), contains no
U+0000–U+001F or U+007F–U+009F control characters or isolated surrogate, and is at
most 1024 UTF-8 bytes. Valid Unicode, including paired surrogates, remains
unchanged. The wire and policy parsers reject isolated JSON surrogate escapes
rather than merge them into U+FFFD; a valid U+FFFD remains accepted. Missing or
invalid `Devices.user` yields unknown account evidence and rejects v2. Valid
legacy/v1 membership remains unchanged; local WhoIs is not an identity source.

Raw accounts enter snapshots and v2 responses, but not ordinary request logs.
The HTTPS callback and existing callback-token authentication are unchanged.
The account secret, HMAC/context derivation, offline tool, dedicated derivation
vectors, and related backup/rotation obligations have been removed.

The new Worker retains base snapshot schema v2 and writes exactly
`accountEvidence:{version:2}` after a complete successful refresh. Every
participating source requires this metadata for v2. Old evidence version 1,
version 2 with a context field, and missing account metadata reject v2 without
invalidating otherwise valid legacy/v1 membership. A failed refresh retains the
complete LKG, not proof that raw-account evidence is ready.

Before a consumer cutover, every configured source shall complete a real
successful refresh under the new Worker. Operators shall then read back v2
`Kind:user` and the expected raw registering account, and confirm identical keys
for tagged and untagged devices with the same registering account. Health
responses, a triggered Cron, an old LKG, or one successful source alone do not
establish readiness. Production still uses network identity v1; raw-account v2
has not been deployed. Publication, deployment, full-source prewarming/readback,
consumer cutover, and rollback preparation require subsequent authorization.
Infinite LKG retention can delay revocation indefinitely; user-string changes
or reuse can alter attribution while existing connections retain old identities.

The user-approved plain-account implementation passed independent redteam code
review, and its sole Unicode-collision finding was fixed and rechecked. That
review does not cover this documentation update or authorize rollout. Before the
Unicode fix, 267 unit, 79 real-workerd, 30 joint scenarios, and targeted Go tests
passed; the joint run had zero skips, ten reclaimed listeners, and zero cleanup
errors. After the fix, only 91 account tests, typecheck, and Go Account tests
(27 top-level plus 102 subtests) were rerun and passed. The 267/79/30 results are
not final-tree reruns. Earlier full Go/race and performance results remain
historical evidence. This documentation update reruns no tests.

Dated evidence and the accepted reload-cost limitation are retained in
[Issue #12](https://github.com/WooDragon/derper-plus/issues/12). The existing
writer-flood test-oracle defect is independently tracked in
[Issue #13](https://github.com/WooDragon/derper-plus/issues/13); it is not fixed by
this increment.

## Local verification and release boundary

The completed local verification includes regression and race tests, real
Worker/fork HTTP exchanges and handshakes, ten independent default subjects,
and a real binary's SIGHUP on existing connections. Independent review found no
required fixes. The dated results and limitations are recorded in existing
[Issue #10](https://github.com/WooDragon/derper-plus/issues/10), not as permanent
test-count requirements in this guide.

The earlier stable-baseline verification passed linux/amd64 and linux/arm64 OCI
builds; that execution did not run or publish a container. The default CGO command
build did not pass on the local machine without Xcode; the pure-Go build and the
two-package race run did pass. Those results describe that execution, not later
branch integration or release status. This documentation update installs no
system tools and makes no production change.
