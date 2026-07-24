# Contributing

a4s is an early architecture experiment. Contributions should strengthen one
small, testable control or runtime boundary rather than broaden the platform
surface prematurely.

Before changing code:

1. Read [project status](docs/project-status.md).
2. Read [the control protocol](docs/control-protocol.md).
3. Read [the security model](docs/security.md).
4. Follow [the development guide](docs/development.md).
5. Check the relevant [decision records](docs/decisions/README.md).

For a new action or trust-boundary change, describe its deterministic policy,
idempotency identity, crash window, evidence, compensation, and denial tests
before implementation.

Use Conventional Commits with subjects no longer than 50 characters. Keep one
goal per commit. Do not include secrets, private keys, real credentials,
sensitive inventory, or emojis.

No project license has been selected yet. Resolve licensing before accepting
third-party contributions or publishing the project broadly.
