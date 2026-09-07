# Contributing to Ooh

Thank you for helping make parallel agent work easier to understand and safer to review.

## Before opening a change

For bugs and small improvements, open an issue or pull request directly. For changes to the task/path/checkpoint model, integration safety, persistent schema, or CLI JSON contract, please open a design issue first. Those surfaces are deliberately small and compatibility-sensitive.

Keep the product language understandable to someone who does not know Git internals. Technical details belong in supporting text, not in the primary action or error message.

## Local setup

Requirements:

- Go 1.26+
- Git 2.31+
- Node.js 24+
- pnpm 10+

```sh
pnpm --dir web install
make check
make test
make build
```

The generated dashboard files in `internal/server/static/` are committed because release builds embed them. Run `pnpm --dir web build` after frontend changes.

## Pull requests

- Add or update tests for behavior changes.
- Exercise risky Git flows in a temporary repository.
- Preserve the stable `--json` envelope or clearly document a schema migration.
- Do not add network calls, telemetry, or a model provider without prior design agreement.
- Keep dependencies focused and explain new runtime dependencies.
- Update the README or architecture notes when behavior changes.

By participating, you agree to follow the [Code of Conduct](CODE_OF_CONDUCT.md).
