# Contributing

Short version. gitdr is kept small and boring on purpose. The best way to help is to
open an issue.

## Issues welcome

- Found a bug or hit something weird? Open an issue.
- Want a provider supported, or have an idea? Open an issue.
- Security problem? Don't open a public issue, follow [`SECURITY.md`](./SECURITY.md).

## Pull requests

I'm not taking code contributions right now. Keeping the contributor surface small is
part of the security story. This is a backup tool, so supply chain matters. No hard
feelings, issues are genuinely the most useful thing you can send.

## Building it yourself

You're very welcome to build, audit, and poke at the code. You need Go 1.26+ and
`git`/`git-lfs`.

```sh
make build            # static binary -> bin/gitdr
make test             # unit tests
make lint             # golangci-lint (pinned, via go run)
make vuln             # govulncheck
make ci               # tidy + fmt + lint + test + vuln
make test-integration # full loop against MinIO (set GITDR_TEST_S3_ENDPOINT + AWS_*)
```

## What gitdr will never do

A few things are locked, so you know what you're relying on.

- No delete. The destination interface has no delete, remove, or overwrite method,
  anywhere. Backups are append-only by construction.
- The WORM check stays. gitdr verifies immutability and warns loud when it's missing.
  `--require-worm` is the opt-in for fail-closed.
- No secrets in code, image, or logs.
- No telemetry, analytics, or phone-home. Ever.
- Linux only, fully static, amd64 and arm64.
