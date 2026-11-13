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

The callback sends `NodePublic`, `Source`, and the literal `IdentityVersion: 1`.
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
