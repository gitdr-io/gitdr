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

**Azure Blob**, a container with a time-based retention policy, then lock the policy
([Azure docs](https://learn.microsoft.com/azure/storage/blobs/immutable-policy-configure-container-scope)).
Version-level immutability on its own locks nothing, and an unlocked policy can be shortened or
deleted. Only Azure Resource Manager says whether a policy is locked, so set
`destination.azure.subscriptionID` and `resourceGroup` and run gitdr as an Entra ID identity
(managed identity, workload identity or service principal). Without them gitdr reports the
container as `unknown`.

### Scope the credential

gitdr never deletes, so its credential should not be able to. The lists below come from the
storage calls the engine makes (`internal/dest/s3/s3.go`) for `backup`, `verify` and `drill`.
**They are derived from the code and have not yet been tested against a live account.** Run
`gitdr doctor`, then one `backup`, `verify` and `drill`, before you rely on them.

**AWS S3**, for the job that runs `backup` (and `drill`, which stores a report):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "BucketLockAndListing",
      "Effect": "Allow",
      "Action": ["s3:GetBucketObjectLockConfiguration", "s3:ListBucket"],
      "Resource": "arn:aws:s3:::my-worm-bucket"
    },
    {
      "Sid": "ReadObjects",
      "Effect": "Allow",
      "Action": ["s3:GetObject", "s3:GetObjectRetention"],
      "Resource": "arn:aws:s3:::my-worm-bucket/*"
    },
    {
      "Sid": "CreateObjects",
      "Effect": "Allow",
      "Action": ["s3:PutObject", "s3:PutObjectRetention"],
      "Resource": "arn:aws:s3:::my-worm-bucket/*"
    }
  ]
}
```

| Action | S3 call | Why |
|---|---|---|
| `s3:GetBucketObjectLockConfiguration` | `GetObjectLockConfiguration` | the WORM check before every backup, and `doctor`. Without it the verdict is `unknown` and backups are written without retention |
| `s3:ListBucket` | `ListObjectsV2` | the previous manifest, the resume check, the manifest a drill or restore looks up, the LFS archive |
| `s3:GetObject` | `GetObject`, `HeadObject` | reading manifests, signatures and artifacts back. `HeadObject` is the create-only check before each write when `endpoint` is set |
| `s3:GetObjectRetention` | `GetObjectRetention` | confirming the first object of a run holds its lock. Optional: without it the manifest says `not-checked` |
| `s3:PutObject` | `PutObject` | artifacts, the signed manifest, the drill report |
| `s3:PutObjectRetention` | `PutObject` with `x-amz-object-lock-*` headers | AWS requires it to set retention on a new object, and `backup` does on every write to a bucket it confirmed immutable |

`s3:PutObjectRetention` also allows the `PutObjectRetention` call on existing objects. Under
COMPLIANCE that can only lengthen a lock, and under GOVERNANCE shortening one also needs
`s3:BypassGovernanceRetention`, which this policy leaves out. gitdr never makes that call.

Left out on purpose: `s3:DeleteObject`, `s3:DeleteObjectVersion`,
`s3:BypassGovernanceRetention`, `s3:PutObjectLegalHold`, `s3:PutBucketObjectLockConfiguration`,
`s3:PutLifecycleConfiguration`, `s3:PutBucketPolicy`.

Two things a policy cannot stop. `s3:PutObject` can always add a new version under an existing
key; Object Lock keeps the old one, and `verify` catches the swap by its checksum. And if the
bucket encrypts with SSE-KMS under your own key, add `kms:GenerateDataKey` for writing and
`kms:Decrypt` for reading, on that key.

An auditor re-running the proof (`verify`, `restore`, `drill -no-report`) needs only
`s3:ListBucket` on the bucket and `s3:GetObject` on its objects. `verify` alone needs only
`s3:GetObject`.

**Backblaze B2**, an application key restricted to the bucket:

```sh
b2 key create --bucket my-worm-bucket gitdr-backup \
  listFiles,readFiles,writeFiles,readBucketRetentions,writeFileRetentions,readFileRetentions
```

| Capability | S3 call | Why |
|---|---|---|
| `readBucketRetentions` | `GetObjectLockConfiguration` | the WORM check |
| `listFiles` | `ListObjectsV2` | as `s3:ListBucket` above |
| `readFiles` | `GetObject`, `HeadObject` | reading back. B2 answers `If-None-Match` with 501, so gitdr checks every key with `HeadObject` before it writes |
| `writeFiles` | `PutObject` | artifacts, manifests, drill reports |
| `writeFileRetentions` | `PutObject` with Object Lock headers | Backblaze documents it as required to set a retention on upload (for its native upload call; its S3 page does not say either way) |
| `readFileRetentions` | `GetObjectRetention` | the post-write lock check. Optional, as on AWS |

Left out on purpose: `deleteFiles`, `bypassGovernance`, `writeBucketRetentions`, `writeBuckets`,
`deleteBuckets`, `writeFileLegalHolds`, `shareFiles`, and every `*Keys` capability.
Backblaze says a key restricted to one bucket needs `listAllBucketNames` for "compatibility with
SDKs and integrations". gitdr never lists buckets, so try without it and add it only if the key
is refused.

One B2 difference: `writeFiles` also lets a key hide a file through B2's native API. Hiding is
not deleting, the earlier versions stay and a locked one cannot be removed, but a hidden bundle
reads as missing until it is unhidden. `verify` reports it.

The auditor's key needs `listFiles,readFiles`.

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

`signature valid: true, artifacts N/N ok` means the run is signed and intact.

## 9. Prove it restores

`verify` re-reads the artifacts and rechecks their checksums, which says the copy is intact.
It does not say the copy comes back. `drill` restores it and compares what came back:

```sh
gitdr drill --config config.yaml --manifest <manifest-key-from-step-7> --output json
```

Every repository is compared against the bundle's own header and against the ref map the
manifest signed. Refs a clone creates nothing for, `refs/merge-requests/*` and the like, are
counted apart and named rather than folded into the total. Non-zero exit if anything fails to
restore or comes back at a different commit. The signed report is stored beside the manifest,
and `reportKey` in the JSON says where.

An auditor can re-run the same proof from your bucket without your signing key. Give them read
access (see step 2) and the public key, and they run:

```sh
gitdr drill --config config.yaml --manifest <manifest-key-from-step-7> --no-report
```

It checks the manifest's signature and runs the same comparison, and writes and signs nothing,
which the output says.

For a restore you drive by hand, follow [`../RESTORE-RUNBOOK.md`](../RESTORE-RUNBOOK.md).

## 10. Schedule it

- Kubernetes. The Helm chart in [`../charts/gitdr`](../charts/gitdr) (CronJob).
- VM. The systemd timer or cron sample in [`../deploy`](../deploy).
- CI. Call `gitdr backup` from your pipeline.

## Optional: client-side encryption

To keep the storage provider from reading your data, set `encryption.enabled: true` and
give it a 32-byte key in `GITDR_ENCRYPTION_KEY` (64-char hex, base64, or raw). `verify`
stays key-free. `restore` needs the key.
