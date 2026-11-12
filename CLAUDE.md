# Fork maintenance

This fork retains the upstream module path and license. Keep upstream behavior
unchanged unless a documented fork-specific contract requires a change.

- `main` follows upstream development.
- `stable` is the fork release branch.
- Feature branches target `stable`; do not merge a feature directly into `main`.
- Label every issue and pull request that carries a fork-local change
  `downstream`. The label marks work that is absent from
  `tailscale/tailscale`, so it identifies what must be re-applied or re-argued
  when rebasing onto a new upstream release. Changes that only track upstream
  do not take the label.
- The `ghcr.io/woodragon/derper-plus` delivery workflow builds DERP images only.
  It does not provide runtime acceptance or authorize deployment.

Before changing container delivery, read
[the container delivery guide](docs/derper-container.md). Before changing
per-user DERP policing, read [the feature guide](docs/derper-user-qos.md). The
guide also links the separate future-policy proposal.
