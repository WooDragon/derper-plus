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

The following policy uses a synthetic fixture key and fictitious subject. It is
an interface example, not production configuration or a rollout command:

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
          "h1_596e6e2f00ef312162a11e49aee4fefd8f46b9ad117dc26307bbe80c6401f987": {
            "upload_bytes_per_second": 0,
            "download_bytes_per_second": 0
          }
        },
        "tagged": {
          "upload_bytes_per_second": 1048576,
          "download_bytes_per_second": 1048576
        }
      }
    }
  }
}
```

`accounts.default` and `accounts.tagged` are mandatory when accounts is present;
`overrides` may be omitted. Overrides replace complete leaves. Leaf integer and
burst validation matches the parent, and nested accounts is invalid. An exact
network policy without accounts does not inherit the top-level default's
accounts. Override keys are 67-character `h1_` user keys, not network subjects.
The `accounts.tagged` leaf in this example is required compatibility policy for
existing `tagged/shared` evidence, not the new Worker's production classification.
The new Worker derives `Kind:user` for both tagged and untagged devices from
valid `Devices.user`, the registering account rather than the current owner.
Devices with the same `(network,user)` share the unchanged HMAC key and bucket.
Tags do not affect rate classification. These identities select a user override
or `accounts.default`, not `accounts.tagged`. The server continues to accept
`tagged/shared` and applies the separate compatibility leaf to that identity.

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

The admission service derives user keys using a secret HMAC over the exact JSON
array defined in the ADR. Missing or invalid `Devices.user` yields unknown account
evidence and rejects v2; tags do not provide a fallback. Valid legacy/v1 membership
remains unchanged. Operators shall not substitute raw user/email strings
or infer keys from local WhoIs. The companion offline tool reads stdin JSON
only; its actual usage is documented with that service. No production account
secret was generated or uploaded. Protected backup, recovery, prewarming, and
coordinated migration require separate approval. Secret rotation is not
transparent: it changes account keys/overrides/bucket identity while old
connections retain old identities. Infinite LKG retention can delay revocation
indefinitely, and user-string changes or reuse can alter attribution.

Existing shared snapshots with a valid context remain legal, and a failed
refresh retains their LKG. Before a consumer cutover, every configured source
shall complete a real successful refresh under the new Worker. Operators shall
then read back v2 `Kind:user` and confirm identical keys for tagged and untagged
devices with the same registering account. Health responses, valid context, or
old shared evidence do not establish that readiness. These are prerequisites,
not authorization for production actions.

The registering-account correction passed 298 Worker unit tests, 84 real-workerd
tests, and 30 joint scenarios with zero skips. Five top-level Go compatibility
tests and 44 subtests passed. The joint runner reclaimed all ten listeners with
zero cleanup errors. Independent redteam review of the five-file implementation
and test increment returned APPROVED with no required fixes; it did not review
this documentation update or authorize rollout. Earlier full Go/race and
performance results remain historical evidence, not reruns for this correction.

The dated local results, fixed old-Worker commit, and accepted reload-cost
limitation are retained in
[Issue #12](https://github.com/WooDragon/derper-plus/issues/12). The existing
writer-flood test-oracle defect is independently tracked in
[Issue #13](https://github.com/WooDragon/derper-plus/issues/13); it is not fixed by
this increment. No account-layer commit, release, or deployment is claimed.

## Local verification and release boundary

The completed local verification includes regression and race tests, real
Worker/fork HTTP exchanges and handshakes, ten independent default subjects,
and a real binary's SIGHUP on existing connections. Independent review found no
required fixes. The dated results and limitations are recorded in existing
[Issue #10](https://github.com/WooDragon/derper-plus/issues/10), not as permanent
test-count requirements in this guide.

The source-content update to upstream stable is not a merge to `stable` or a
published release. Both linux/amd64 and linux/arm64 OCI builds passed, but no
container was run or published. The default CGO command build did not pass on
the local machine without Xcode; the pure-Go build and the two-package race run
did pass. This delivery installed no system tools and made no production change.
