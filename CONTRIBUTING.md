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
make test-ci          # what CI runs: everything, and no test is allowed to skip
```

### Running the whole suite

Some tests need a storage emulator, and without one they skip. Go prints nothing for a
skipped test and still reports `ok`, so a green `make test` does not by itself mean
everything ran. Two containers cover it:

```sh
docker run -d --name azurite -p 10000:10000 \
  mcr.microsoft.com/azure-storage/azurite \
  azurite-blob --blobHost 0.0.0.0 --skipApiVersionCheck

docker run -d --name minio -p 9000:9000 \
  -e MINIO_ROOT_USER=minioadmin -e MINIO_ROOT_PASSWORD=minioadmin \
  quay.io/minio/minio server /data

export AZURITE_BLOB_ENDPOINT=http://127.0.0.1:10000
export GITDR_TEST_S3_ENDPOINT=http://127.0.0.1:9000
export AWS_ACCESS_KEY_ID=minioadmin AWS_SECRET_ACCESS_KEY=minioadmin AWS_REGION=us-east-1
make test-ci
```

Azurite needs `--skipApiVersionCheck`. It runs behind the Azure SDK and rejects the API
version the SDK sends, so every request is a 400 without it.

## What gitdr will never do

A few things are locked, so you know what you're relying on.

- No delete. The destination interface has no delete, remove, or overwrite method,
  anywhere. Backups are append-only by construction.
- The WORM check stays. gitdr verifies immutability and warns loud when it's missing.
  `--require-worm` is the opt-in for fail-closed.
- No secrets in code, image, or logs.
- No telemetry, analytics, or phone-home. Ever.
- Linux only, fully static, amd64 and arm64.
