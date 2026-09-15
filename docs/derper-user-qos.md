# DERP per-user bandwidth limits

`main` remains the upstream development line. `stable` is the fork release
branch. Read [the container delivery guide](derper-container.md) for the
published container images and their separate runtime verification limits.

This fork adds an opt-in packet policer to `cmd/derper`. It applies a shared
upload bucket and a shared download bucket to each authenticated user within
one DERP server process. It does not modify Tailscale clients, ACLs, or the
control plane.

## Scope and limits

The server charges upload allowance after it fully reads a DERP packet payload
and before it looks up the destination or forwards the packet. An unknown
destination can therefore consume the sender's allowance. The server checks
download allowance before it writes a packet frame to the destination client.

The policer counts DERP packet payload bytes, including encrypted discovery
payloads. It does not count TCP, TLS, DERP frame headers, or control frames.
Therefore, it does not cap bytes already received by the network interface and
is not a billed-wire-byte limit.

Each direction has an independent token bucket. A bucket permits its configured
burst before applying its sustained rate. A packet that exceeds the available
allowance is dropped immediately. The server does not sleep, queue the packet
for later, or disconnect the client. TCP applications can back off. UDP traffic
can lose packets.

A user-owned device pair shares one bucket pair across all of that user's
connections in this process. Reconnecting does not create a new allowance. A
transfer between two devices owned by the same user consumes both directions of
the same pair. The implementation does not guarantee fair sharing among a
user's devices.

These limits are per process. They do not coordinate across DERP instances.
They apply only to DERP payloads in this `derper` process, including payloads
whose inner traffic is TCP or UDP. They do not apply to direct P2P traffic.
STUN is a connectivity probe, not a business relay path.

[Peer Relay](https://tailscale.com/docs/features/peer-relay) is an official
Tailscale feature with a separate UDP relay path. It does not invoke this
policer. This derper process is not a Peer Relay server. Native DERP clients use
TCP/TLS and an HTTP/1 Upgrade. This fork does not implement HTTP/3 or QUIC for
DERP. Enabling HTTP/3 in an external Caddy listener does not change the DERP
client path. See Tailscale's [connection type reference](https://tailscale.com/docs/reference/connection-types)
for the distinction between direct, DERP, STUN, and relay paths.

## Configuration

Enable the feature with `--user-rate-config=<path>`. The feature requires
`--verify-clients=true` because the server obtains the connection identity from
its local authenticated Tailscale state. It rejects `--rate-config` and all
mesh configurations, including a mesh key discovered from the default mesh-key
path.

The file is strict JSON and must be at most 1 MiB. It requires a `default`
policy. Each policy requires both directional rate fields. An omitted field is
an error, not an unlimited rate. A value of zero explicitly makes that direction
unlimited.

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
  },
  "tagged": {
    "upload_bytes_per_second": 250000,
    "download_bytes_per_second": 250000,
    "burst_bytes": 131072
  }
}
```

The ID `12345` is fictitious. Obtain real IDs from the authenticated local
Tailscale state. Do not place real user IDs, addresses, endpoints, or secrets in
this repository.

`users` keys must be canonical positive decimal user IDs. Leading zeros,
negative values, zero, overflow, and partial policies are rejected. `burst_bytes`
is optional. Omission or zero selects `derp.MaxPacketSize`. An explicit `null`,
a positive burst below that packet size, or a burst that does not fit Go's `int`,
is rejected.

The optional `tagged` policy controls every tagged device through one separate
bucket pair. If it is absent, tagged devices use `default`. Tagged nodes do not
use their creator's policy. This prevents a creator's explicit zero-rate
exemption from leaking to an ACL-tagged identity.

Use explicit `users` entries with zero directional rates for owners or trusted
users. The server does not infer exemptions from email addresses, node keys,
network addresses, or node creators.

## Start, reload, and rollback

Build the fork with the repository's required Go toolchain:

```sh
GOTOOLCHAIN=auto go build ./cmd/derper
```

Start `derper` with both `--verify-clients=true` and
`--user-rate-config=/path/to/user-rate.json`. Keep mesh flags and effective mesh
keys absent. Startup rejects an invalid configuration, a disabled verification
flag, the old rate configuration, or mesh settings. It does not begin serving
with a partial policy.

Send `SIGHUP` to reload the same configuration path. The server validates the
entire replacement before publishing it. A failed reload logs a non-sensitive
error and retains the previous policy. For an existing nonzero directional
limiter, a successful reload updates that limiter in place without rebuilding
it. A transition from zero (unlimited) to a nonzero rate creates a new limiter,
which receives its initial configured burst. Existing and new connections use
the new policies after the reload completes.

To roll back, remove `--user-rate-config`, restore any intended upstream mesh or
`--rate-config` settings, and restart the process. The mode is selected at
startup; an empty file cannot switch modes during reload.

The server exposes aggregate upload/download packet and byte drop counters. It
does not publish per-user metrics.

## Identity boundary

The user or tagged identity is selected when the connection is admitted. A
later ownership or tag change takes effect only after the device reconnects.
Unknown, incomplete, or non-positive user identity data fails closed. When this
feature is disabled, the upstream verification and admission log behavior is
unchanged.

## Future direction

[Issue #5](https://github.com/WooDragon/derper-plus/issues/5) proposes a future
policy input that maps `grants.app` data through `WhoIsResponse.CapMap`. It is
not implemented. The current policer accepts only local JSON configuration.

## Deferred operator validation

This delivery deliberately adds no tests and does not run runtime probes.
Before production use, operators should validate user aggregation, simultaneous
connections, reconnect behavior, reload behavior, owner/trusted exemptions,
packet loss behavior, and expected throughput in an isolated environment. The
operator remains responsible for network-level shaping when physical ingress or
wire-byte accounting is required.
