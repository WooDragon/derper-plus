# DERP per-tailnet aggregate payload limits

This guide is for DERP operators who use a trusted external admission service.
The mode is implemented and locally verified. Plain-account v2 was published,
deployed, and accepted in Chengdu on 2026-11-14 at revision
`d483b6463b8d1be90ba91115a975ae2532e8b503`; see the
[deployment record in Issue #12](https://github.com/WooDragon/derper-plus/issues/12#issuecomment-6877133383).
That acceptance does not constitute throughput certification. Other images and
deployments shall not be assumed to contain these flags.

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

By default, the static limits are per process, not a network-wide total. The
optional coordinator below adds a cross-node soft limit, not a strict global
bucket. Wire overhead, direct P2P,
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

## Optional account layer (v2 adopted in Chengdu on 2026-11-14)

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
establish readiness. The separately authorized raw-account v2 rollout completed
full-source prewarming/readback and consumer cutover in Chengdu on 2026-11-14.
The [deployment record in Issue #12](https://github.com/WooDragon/derper-plus/issues/12#issuecomment-6877133383)
records the accepted revision, production acceptance, and rollback boundary.
This documentation update authorizes no further rollout and reruns no tests.
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

## Optional cross-node coordinator (local implementation, not published)

As of 2026-11-15, the two-repository implementation, required local verification,
and independent code review are complete. The coordinator code and documentation
remain uncommitted and unpublished on `feature/derp-global-soft-rate`. No
production coordinator has been deployed. The existing Chengdu account-v2
adoption above does not imply coordinator adoption or approve another rollout.

> **Required reading:** Before configuring or changing the coordinator, read
> `services/derp-admission-worker/README.md`, section “可选跨节点动态全局软限速”, in
> [the personal-infra repository](https://github.com/WooDragon/personal-infra).
> That README is the sole authority for coordinator wire fields, HTTP behavior,
> global configuration, allocation, fencing, persistence, and errors. The new
> content currently exists only on the local feature branch; the remote default
> branch does not yet contain it. After publication, use the corresponding release
> revision. Do not assume that the repositories are siblings on a deployed host.

### Startup configuration

The coordinator defaults to disabled. To opt in, the process requires both
`--tailnet-rate-config=<local-policy-path>` and
`--tailnet-account-identity=true`, in addition to the existing external-admission
startup requirements above. Add `--tailnet-coordinator-config=<config-path>`.
An incompatible combination or invalid startup file fails startup.

The coordinator file is a complete strict JSON object of at most 1 MiB. Exactly
three nonempty string fields are required. Unknown or duplicate fields, trailing
JSON, and invalid Unicode are rejected. This example uses synthetic values and
file paths; it is not a production deployment command:

```json
{
  "endpoint": "https://coordinator.example.test/coordinator/report",
  "nodeId": "relay-a",
  "tokenFile": "/etc/derper/admission-token"
}
```

- `endpoint` must use HTTPS and the exact `/coordinator/report` path. The client
  rejects userinfo, query, fragment, opaque URLs, and encoded paths. It does not
  follow redirects. Only tests can explicitly permit IP-loopback HTTP; no
  production flag enables that exception.
- `nodeId` uses 1–64 ASCII characters from `[A-Za-z0-9_-]` and must appear in the
  Worker's global node whitelist. Each nodeId shall have only one running
  instance. Hosts shall keep synchronized clocks: a clock rollback or identical
  boot timestamp can trigger fencing rejection. There is no startup lease or
  local persistent fencing file.
- `tokenFile` contains the existing Worker's `ADMISSION_TOKEN` value, not a new
  secret or a credential extracted from the verify URL. The client reads at most
  4096 bytes and permits one conventional trailing LF/CRLF. The remaining token
  must be nonempty printable ASCII without whitespace. The new endpoint uses
  Bearer authentication; the existing verify query-token contract is unchanged.

The process reads this config and token at startup only. SIGHUP does not reread
or rotate them. A process restart is required to change endpoint, nodeId, or
token. The global configuration belongs to the Worker; copying local policy
into that configuration is not an automatic synchronization mechanism.

### Reporting and response validation

The production reporter exchanges complete activity windows every 30 seconds,
starting after the first ticker interval. It allows one in-flight request with
a 5-second total timeout. Shutdown cancels the request. It does not retry old
reports or maintain a pending queue; the next tick samples a new window.
Intervals are not production tuning flags.

The trusted packet path counts attempted bytes before rate rejection and admitted
bytes only when both parent and account allow. Admitted does not mean successful
socket delivery. Parent counters do not double-count parent/child rejection.
Only the current activity set enters the report, not every historical bucket.
An invalid duration, overflow, or capacity excess discards the complete window
rather than submitting truncated demand. Missing entries represent no demand in
that window; later attempts, including deny rejections, restore activity.

The client requires HTTP 200, bounds the response to 1 MiB, and validates the
complete strict document before applying any allocation. It checks version,
nodeId, bootId, acceptedSeq, generation, numeric ranges, modes, duplicate subjects,
capacity, and registration of every allocated identity. Generation cannot go
backward. policyRevision is validated and logged; the client does not maintain a
second revision high-water mark. The README defines the full wire contract,
including the coordinator's independent `version:1`, which is not admission v2.

### Effective limits, lifecycle, and failure behavior

The in-memory overlay intersects the local static policy independently for each
network/account direction: deny wins, unlimited acts as infinity, and finite
rates use the minimum. A global unlimited allocation cannot remove a finite
local limit. A local unlimited account cannot bypass a finite parent. Dynamic
zero share is deny; static JSON rate 0 remains unlimited.

Static SIGHUP reloads and coordinator responses compose under the same registry
lock. Reloads retain the overlay; responses retain the static policy. Neither
operation re-identifies or reconnects established clients. Unchanged effective
policies preserve balances; finite transitions follow the existing settlement,
freeze/resume, and no-refill rules. The local burst remains authoritative. When
static accounts is absent, a dynamically finite account leaf defaults to
`derp.MaxPacketSize` capacity rather than a zero-size burst.

A response omission retains the identity's last overlay. New identities and a
restarted process use static policy until their first valid allocation; the
process does not persist its overlay. Timeouts, non-200 responses, and invalid
responses retain the last valid overlay and leave data service running. A new
boot rejected because of clock rollback therefore continues with static policy;
a running process that already has an overlay retains that overlay on errors.
Worker reports older than 90 seconds leave demand calculation but do not revoke
local overlays or erase boot/seq fencing.

Normal propagation aims for approximately 30–60 seconds, not a hard SLA. Cold
start, new identities, budget changes, stale-node exclusion, recovery, and
asynchronous handoff can exceed the global budget. During coordinator failure,
excess can last indefinitely. This is the approved availability tradeoff, not a
strict global bucket. Aggregate burst, device fairness, cross-layer optimal
utilization, minimum bandwidth, and public-network throughput certification are
not guaranteed. Direct traffic, Peer Relay, and nonparticipating DERPs remain
outside the scope. All token holders share a trusted operations domain and can
impersonate another whitelisted node; nodeId is not cryptographic isolation.

To return to static mode after separate rollout approval, remove the coordinator
startup flag and restart the intended process. SIGHUP or an empty response is not
a mode switch. Disabling the Worker route alone leaves running nodes' last
overlays in place. No production operation is authorized by this guide update.

### Local verification evidence

The supplied prior evidence records Worker typecheck, 327 unit tests, and 99
runtime tests; Go four-package results of 167 top-level tests / 721 test nodes
with three existing skips; and post-fix targeted 27/102 plus race results. This
documentation update reruns no suite. The real joint run passed all 36 scenarios
(the original 30 plus six coordinator scenarios) using two real binaries,
production 30-second ticks, a Miniflare SQLite DO, and an HTTPS test CA. Each
node received account rates of 65536 B/s in each direction, totaling R=131072
B/s. It verified idle-share return to R, HTTP 503 retention and recovery, a real
OS SIGHUP with six original connections retained, and a finite parent constraining
an unlimited account. API concurrency tests are not OS-signal evidence.

The joint run exposed a zero-burst drop when static accounts was absent; that
case was fixed and reverified, with failure evidence retained. DO eviction/
restoration and transaction rollback are established by runtime tests, not by
the joint runner's temporary SQLite directory: its `durableObjectsPersist` was
not consumed by the current Miniflare converter. These results do not certify
public throughput. Dated evidence and unpublished status belong in the new
same-repository [Issue #17](https://github.com/WooDragon/derper-plus/issues/17),
separate from the completed account task. That Issue remains OPEN: commit,
publication, and production adoption have not occurred. A later commit shall
reference only the same-repository Issue number.

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
