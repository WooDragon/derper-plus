# Fork maintenance

This fork retains the upstream module path and license. Keep upstream behavior
unchanged unless the fork-specific user-limit contract requires a change.

- `main` follows upstream development.
- `qos` is the fork baseline for released user-limit work.
- Feature branches target `qos`; do not merge a feature directly into `main`.

Before changing per-user DERP policing, read
[the feature guide](docs/derper-user-qos.md) and
[the approved design](docs/derper-user-qos-plan.md).
