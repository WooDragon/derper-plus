# DERP per-user bandwidth limits: implementation plan

## Scope and baseline

This public fork adds opt-in, single-process bandwidth limits to the official DERP server. It does not change the Tailscale control plane, clients, or tailnet ACL semantics.

- Upstream baseline: `v1.102.4`, commit `bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8`.
- Fork: `WooDragon/derper-user-qos`.
- `main` retains the upstream development line. The fork default branch, `qos`, starts at the stable baseline. Feature PRs target `qos`, never the upstream repository.
- Go requirement: `1.26.6`. Use the Go toolchain mechanism for this repository; do not upgrade the host installation as part of this change.
- This delivery does not deploy a server. At the user's request, it does not add or execute tests. Formatting, compilation, diff inspection, and focused review remain in scope. GitHub Actions is disabled for this initial delivery.

## Required behavior

A verified user has one upload bucket and one download bucket per DERP server process. All of that user's user-owned devices and simultaneous connections share those buckets. A reconnect does not create another allowance.

Upload means an authenticated client's DERP packet payload accepted for forwarding. Download means a DERP packet payload accepted for writing to the receiving client's connection. Both directions count `len(contents)`, including encrypted discovery packet payloads. They do not count TCP, TLS, DERP framing, or DERP control frames. Rates therefore describe relayed payload throughput, not exact physical interface utilization.

Each direction has an independent rate and burst. The token-bucket bound permits a configured burst above the sustained rate. A same-user device-to-device transfer consumes both that user's upload and download allowance. Owner exemption does not remove the other endpoint user's limit.

Without the new flag, authentication, packet forwarding, mesh, the existing per-connection rate configuration, and the wire protocol retain their upstream behavior.

## Configuration contract

Add `--user-rate-config=<path>` to `cmd/derper`. The file is strict JSON. A representative example is:

```json
{
  "default": {
    "upload_bytes_per_second": 1250000,
    "download_bytes_per_second": 1250000,
    "burst_bytes": 131072
  },
  "users": {
    "12345": {
      "upload_bytes_per_second": 0,
      "download_bytes_per_second": 0,
      "burst_bytes": 131072
    }
  }
}
```

`12345` is a fictitious example, not a deployment identity. The operator obtains real user IDs from their authenticated local Tailscale state. No real identity, deployment endpoint, or credential belongs in this public repository.

- `default` is required. `users` is optional and contains complete policy overrides, not partial overlays.
- Both direction fields are required in every policy. A nonnegative integer is required; explicit zero means unlimited in that direction. Missing fields do not silently grant unlimited bandwidth.
- `burst_bytes` is optional. Zero or omission selects `derp.MaxPacketSize`. An explicit positive value below `derp.MaxPacketSize` is rejected. Values that cannot fit the limiter's `int` burst are rejected.
- User override keys are canonical positive decimal Tailscale user IDs. Reject zero, negative IDs, overflow, and alternate spellings such as leading zeros.
- An optional `tagged` policy uses the same complete-policy schema. If omitted, it uses `default`. All tagged devices share one separate bucket pair. They never inherit the creating user's exemption, because a tagged node's creator is not its ACL identity.
- Reject unknown fields, trailing JSON values, malformed policies, and an empty/null configuration. Bound config file reads to 1 MiB. Validate the entire candidate before applying it.
- Owner and trusted-user exemptions are explicit user-ID entries. Do not infer account roles from email strings, public IPs, short node keys, or node creators.

The new mode requires `--verify-clients=true`. Reject combinations with the old `--rate-config`, which would otherwise also limit exempt users. The first version does not support mesh in user-limit mode. At startup, reject all effective mesh settings after flag defaults and key discovery have been resolved, including an automatically discovered `/home/derp/keys/derp-mesh.key`, not just explicitly supplied flags. Reject authenticated mesh peers when the mode is active as a second boundary. The unmodified mode retains mesh support.

## Implementation approach

### 1. Preserve authenticated identity

Keep the `WhoIsNodeKey` result already obtained by local client verification. Carry a small internal identity value through `verifyClient`, `accept`, and `sclient` construction. Update the existing consistency-check call site for the internal return signature.

When user limits are enabled, a user-owned node requires a valid positive `UserProfile.ID`. Tagged nodes select the separate tagged identity. Missing identity data fails admission rather than selecting an unlimited bucket. The original admission URL checks continue to apply. Do not add names, emails, or full node keys to feature diagnostics. The existing admission wrapper includes a full key in its error text; give the new identity/unsupported-mesh rejection paths a distinct error category and key-free formatting through `accept` and `Accept`. Do not change the upstream failure-log format when user limits are disabled.

The identity is a connection-time snapshot. Ownership/tag changes require reconnection for reassignment; this feature does not introduce a separate device revocation or ACL engine.

### 2. Keep a process-wide registry of persistent user buckets

Add a focused file under `derp/derpserver/` for the config parser, normalized policies, registry, and directional token buckets. Keep integration changes in the existing server file small.

One registry entry belongs to one user ID, or to the separate tagged-device identity. The registry keeps entries for the lifetime of the server. Entries are not created per node or per connection, and are not deleted on disconnect. This deliberately avoids timers, refcount races, and reconnect-induced burst resets. Memory grows with distinct authenticated users, not packets or connection churn.

Use the existing concurrency-safe `golang.org/x/time/rate` implementation. A registry RW lock linearizes membership/configuration changes against allowance checks: updates and bucket creation hold its write lock, and allowance checks hold its read lock. This lock is separate from `Server.mu`. Do not hold `Server.mu` while performing new limiter work. No new database, daemon, or goroutine per packet is needed.

### 3. Use nonblocking packet policing

The new mode is a packet policer, not a pacing scheduler. Use `AllowN` for the payload size. An excess packet is dropped without sleeping, disconnecting the client, or reserving future tokens. Document the difference: TCP application flows may back off; UDP packets can be lost. The feature does not promise fair sharing among devices belonging to the same user.

- Upload: check after `recvPacket` has consumed the entire `FrameSendPacket`, before destination lookup or forwarding. A denied packet returns normally to the read loop. This preserves framing and lets the read loop handle DERP control messages.
- Download: check at the start of the ordinary `sendPacket` helper, before writing any header, payload, or recording a successful send. Both ordinary and discovery queues use this helper. A denied packet returns normally to the send loop.
- Count discovery payloads too, so the priority queue cannot bypass the byte allowance.
- Keep DERP keepalive, ping, and peer-control frames outside this new payload budget. The existing upstream protocol validation remains unchanged. General abuse/DoS protection is not part of this change.
- Preserve existing successful-send statistics. Add only low-cardinality aggregate counters for packets/bytes denied by the new upload and download checks. Do not expose per-user labels or introduce a metrics service.

Dropping a packet after consuming it is not a way to cap bytes already received on the physical NIC. This feature limits accepted/forwarded payloads. Operators who need a physical ingress or billed-wire-byte ceiling still need network-level shaping.

### 4. Reload without replacing active buckets

Load the configuration before admitting connections. Reuse the existing SIGHUP mechanism for the new mutually exclusive mode.

On reload, validate the entire file before changing active state. Invalid input keeps the previous configuration and logs a non-sensitive error. Registry configuration publication, existing-bucket updates, and bucket lookup/creation share one registry write-lock critical section. Select a new bucket's policy only inside that section; a connection cannot publish an old-policy bucket after reload returns. Allowance checks take the registry read lock while consulting the directional limiter, so they cannot observe a partially applied reload. This registry lock is distinct from `Server.mu` and is never held across socket I/O.

Existing and newly arriving clients use the updated policies after a successful reload. Update the rates/bursts on existing bucket objects; do not replace bucket objects or rebind every connection. Preserve accumulated token state using the limiter's update APIs. Removing an override makes its existing bucket use the default policy.

The process stays in its startup-selected mode. Disabling user limiting or switching back to upstream per-connection limits requires a restart. Do not support mode toggling through an empty config.

## File boundary

Expected implementation surface:

- `derp/derpserver/derpserver.go`: authenticated identity propagation, connection bucket attachment, two packet hooks, aggregate metric registration.
- `derp/derpserver/user_rate.go` (or two short files in this package): configuration and user-bucket implementation.
- `cmd/derper/derper.go`: flag validation, startup loading, and SIGHUP reload routing.
- `docs/derper-user-qos.md`: semantics, configuration, local identity lookup guidance, build/use instructions, limitations, and rollback.
- `docs/derper-user-qos-plan.md`: this bounded design and its review decisions.
- Root `README.md`: a short fork-specific entry linking to the feature guide and this plan; retain upstream content.
- Root `CLAUDE.md`: a short maintenance entry, upstream/fork branch boundaries, and document navigation.

No dependency changes are expected. Keep the module path and upstream license intact. Do not modify existing tests, add a test harness, or add CI workflows in this delivery.

## Review and delivery sequence

1. Main conversation owns this plan and scope decisions.
2. A focused `redteam` review checks only this feature's identity, shared limits, packet framing, compatibility, reload behavior, and direct regressions. It does not redesign the control plane or demand distributed quotas, a UI, an alternative VPN, or the explicitly deferred tests.
3. A `dev` agent implements the approved plan, using existing source idioms. It does not start reviews, deploy, or change the agreed verification scope.
4. Perform formatting checks and native plus Linux/amd64 compilation of `cmd/derper` with the repository's required toolchain. Build artifacts stay outside tracked source. Do not execute `go test`, race tests, benchmarks, integration tests, or runtime traffic probes.
5. Track the feature in an Issue on this fork. Commits use the upstream directory-prefix style, an internal `Updates #N` reference, `Change-Id`, and DCO sign-off. Never create an Issue or PR on the upstream repository.
6. Open a PR from `feat/user-bandwidth-limits` to `qos`. Run the user-requested `pr-review` through the main conversation. Apply only accepted findings and repeat focused follow-up until no accepted findings remain. Do not amend or force-push review iterations.
7. Merge through the fork PR, not a local merge to the default branch. Verify the remote merge state. Record that compilation/review are not functional or production acceptance.

## Deferred validation and deployment

The requested no-tests boundary leaves user aggregation, concurrent reloads, reconnect behavior, packet loss characteristics, and real Owner/guest throughput unverified at runtime. Documentation and the PR should state that plainly. No production host, Caddy configuration, tailnet ACL, or bandwidth setting changes as part of this delivery.
