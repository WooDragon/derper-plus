# Fork maintenance

This fork retains the upstream module path and license. Keep upstream behavior
unchanged unless a documented fork-specific contract requires a change.

- `main` follows upstream development.
- `stable` is the fork release branch.
- Feature branches target `stable`; do not merge a feature directly into `main`.
- The `ghcr.io/woodragon/derper-plus` delivery workflow builds DERP images only.
  It does not provide runtime acceptance or authorize deployment.

Before changing container delivery, read
[the container delivery guide](docs/derper-container.md). Before changing
per-user DERP policing, read [the feature guide](docs/derper-user-qos.md) and
[the approved design](docs/derper-user-qos-plan.md).
