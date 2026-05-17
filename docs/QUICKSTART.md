# Quickstart

Zero to a verified, immutable backup of one repo, in about 10 minutes.

gitdr is a single static Linux binary that runs as a one-shot job. On macOS, run the
container (`ghcr.io/gitdr-io/gitdr`).

## 1. Get the binary

```sh
# container
docker run --rm ghcr.io/gitdr-io/gitdr version
# or build from source (Go 1.26+)
make build && ./bin/gitdr version
```

## 2. Make a WORM bucket

gitdr strongly recommends an immutable destination. It warns if the bucket isn't WORM,
and `--require-worm` makes it fail closed. Immutability is the whole point of a
ransomware-resistant backup, so set one up. Pick your provider.

**AWS S3**, Object Lock (only settable at creation):
```sh
aws s3api create-bucket --bucket my-worm-bucket --region us-east-1 \
  --object-lock-enabled-for-bucket
aws s3api put-object-lock-configuration --bucket my-worm-bucket \
  --object-lock-configuration \
  '{"ObjectLockEnabled":"Enabled","Rule":{"DefaultRetention":{"Mode":"COMPLIANCE","Days":30}}}'
```

**Google Cloud Storage**, locked retention policy (the lock is irreversible):
```sh
gcloud storage buckets create gs://my-worm-bucket --location=US \
  --uniform-bucket-level-access --public-access-prevention --retention-period=30d
gcloud storage buckets update gs://my-worm-bucket --lock-retention-period
```

**Azure Blob**, create a storage account with version-level immutability plus blob
versioning, then a container ([Azure docs](https://learn.microsoft.com/azure/storage/blobs/immutable-version-level-worm-policies)).

Scope the credential gitdr uses to create/put only. It never needs delete.

## 3. Source credentials (read-only)

- GitHub. Create a GitHub App with read-only repo and metadata, install it, note the App
  ID and Installation ID, and download the private key (PEM).
- GitLab. A project or group access token with `read_api` and `read_repository`.

## 4. Manifest signing key

Every run writes an ed25519-signed manifest. Make the keypair once.

```sh
openssl genpkey -algorithm ed25519 -out manifest-signing.pem
openssl pkey -in manifest-signing.pem -pubout -out manifest-public.pem
```

Keep `manifest-signing.pem` secret and off the runner if you can. `verify` only needs the
public key.

## 5. Config

Copy `config.example.yaml` to `config.yaml` and set the source and destination. A minimal
GitHub to S3 setup:

```yaml
source:
  type: github
  repo: "acme/api"
  github: { appID: 123456, installationID: 7890123 }
destination:
  type: s3
  s3: { bucket: my-worm-bucket, region: us-east-1 }
  retention: { mode: COMPLIANCE, days: 30 }
manifest:
  publicKeyPath: ./manifest-public.pem
worm: { require: false } # warn on non-WORM and proceed. true means fail closed
```

## 6. Secrets (env only, never in the YAML)

```sh
export GITDR_GITHUB_APP_PRIVATE_KEY="$(cat github-app.pem)"
export GITDR_MANIFEST_SIGNING_KEY="$(cat manifest-signing.pem)"
export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...   # or use an instance role
```

## 7. Preflight, then back up

```sh
gitdr doctor --config config.yaml     # checks config, source auth, and the WORM lock
gitdr backup --config config.yaml     # clone, bundle, sha256, immutable upload, signed manifest
```

`doctor` writes nothing. `backup` exits non-zero on any failure, so treat that as a failed
backup. The run prints the manifest key.

## 8. Verify

```sh
gitdr verify --config config.yaml --manifest <manifest-key-from-step-7>
```

`signature valid: true, artifacts N/N ok` means the run is signed and intact. To prove a
real restore, follow [`../RESTORE-RUNBOOK.md`](../RESTORE-RUNBOOK.md).

## 9. Schedule it

- Kubernetes. The Helm chart in [`../charts/gitdr`](../charts/gitdr) (CronJob).
- VM. The systemd timer or cron sample in [`../deploy`](../deploy).
- CI. Call `gitdr backup` from your pipeline.

## Optional: client-side encryption

To keep the storage provider from reading your data, set `encryption.enabled: true` and
give it a 32-byte key in `GITDR_ENCRYPTION_KEY` (64-char hex, base64, or raw). `verify`
stays key-free. `restore` needs the key.
