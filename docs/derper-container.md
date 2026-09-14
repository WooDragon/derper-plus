# derper-plus container delivery

## Scope and publication status

`derper-plus` provides a DERP container delivery path for this fork. The intended
image name is `ghcr.io/woodragon/derper-plus`. The image is built for
`linux/amd64` and `linux/arm64`. Registry publication does not establish runtime
acceptance.

The image contains a static `derper` executable and CA certificates. It does not
contain configuration, identities, credentials, `tailscaled`, a shell, or an
entrypoint wrapper. The process runs as root by default because DERP can use low
ports and a privileged local Tailscale socket. Operators may configure another
user only when that user can write every required state path and access the
chosen socket.

A successful build or publish does not prove that the package is public. The
first GHCR publication can require a package administrator to change visibility
in the GitHub UI. The package page is
<https://github.com/users/WooDragon/packages/container/package/derper-plus>.
Do not claim anonymous availability until an anonymous pull has succeeded.

## Build and publishing policy

Pull requests to `stable` build both target platforms without registry login or
publication. Only a `stable` push or a manual dispatch from `stable` can publish
`edge` and `sha-<full-commit-sha>`. A release tag must match
`v<upstream-version>-plus.<revision>`, for example `v1.102.4-plus.1`.

The publication workflow verifies that a release commit is reachable from
`origin/stable` before it logs in to GHCR. A stale `stable` event skips
publication instead of moving `edge` backward. Release tags publish the version
and commit tags. They do not move `edge`. The workflow never publishes `latest`.

OCI labels identify the source repository, checked-out source revision, version,
and BSD-3-Clause license. The job summary records the resulting manifest digest.
A digest is immutable. A SHA-named tag is only a traceability aid because an
authorized publisher can rebuild a tag.

## Minimal standalone example

The image uses `/var/lib/derper` as `HOME` and as its working directory. Persist
that directory because DERP stores its key and default certificate cache there.
The image exposes metadata for TCP `443`, TCP `80`, and UDP `3478`. It does not
publish host ports automatically.

Use a digest after publication rather than treating the placeholder below as an
available public image:

```sh
docker run --rm \
  --publish 443:443/tcp \
  --publish 80:80/tcp \
  --publish 3478:3478/udp \
  --volume derper-state:/var/lib/derper \
  ghcr.io/woodragon/derper-plus@sha256:REPLACE_WITH_PUBLISHED_DIGEST \
  --hostname=derp.example.invalid
```

This example is not runtime-verified. `derp.example.invalid` is deliberately
fictional. An operator must provide an appropriate certificate and network
configuration before exposing a DERP service.

## Compose example

The following Compose file is an example. Do not treat it as a deployment
procedure or runtime acceptance.

```yaml
services:
  derper:
    image: ghcr.io/woodragon/derper-plus@sha256:REPLACE_WITH_PUBLISHED_DIGEST
    command:
      - --hostname=derp.example.invalid
    ports:
      - "443:443/tcp"
      - "80:80/tcp"
      - "3478:3478/udp"
    volumes:
      - derper-state:/var/lib/derper

volumes:
  derper-state:
```

## Optional per-user rate mode

Per-user rate mode is optional. It requires both `--verify-clients=true` and a
real, authenticated external `tailscaled` socket that is compatible with the
upstream baseline. The image does not include or authenticate `tailscaled`.

Mount the configuration directory read-only instead of a single configuration
file. That form permits an operator to replace the configuration atomically.
Mounting the socket directory read-only does not make the local Tailscale API
read-only. The socket service still determines which requests it authorizes.

```yaml
services:
  derper:
    image: ghcr.io/woodragon/derper-plus@sha256:REPLACE_WITH_PUBLISHED_DIGEST
    command:
      - --hostname=derp.example.invalid
      - --verify-clients=true
      - --socket=/var/run/tailscale/tailscaled.sock
      - --user-rate-config=/etc/derper-qos/user-rate.json
    volumes:
      - derper-state:/var/lib/derper
      - /srv/example-derper/qos:/etc/derper-qos:ro
      - /var/run/tailscale:/var/run/tailscale:ro

volumes:
  derper-state:
```

Read [the per-user rate guide](derper-user-qos.md) before enabling this mode.
After an operator atomically replaces the configuration file, send `SIGHUP` to
the DERP process to reload the same path. The examples do not demonstrate that
signal or any runtime behavior.

## Rollback and verification limits

Roll back by changing the image reference to a previously recorded, known-good
manifest digest. Preserve the state volume unless the operator intentionally
rotates DERP identity or certificate state.

This repository does not run container startup, traffic probes, race checks,
benchmarks, or runtime tests for this delivery. The authoritative evidence for
multi-platform assembly is the PR image workflow. A later publication acceptance
must separately verify the published source revision, digest, both platform
descriptors, OCI labels, and anonymous package access without executing the
image.
