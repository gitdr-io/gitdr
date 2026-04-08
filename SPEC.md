# gitdr, design and architecture

The why and the detail behind gitdr. If you just want to run it, the README and
[`docs/QUICKSTART.md`](./docs/QUICKSTART.md) are faster.

---

## 1. Purpose and scope

gitdr backs up Git VCS organizations to WORM-immutable object storage, for disaster
recovery and ransomware resilience. It targets CI and production, so Linux only,
container or binary, no GUI.

In scope: git data, Git LFS, repository metadata, immutable writes, restore and verify,
multiple sources and multiple destinations.

Out of scope: a web dashboard, scheduling UI, multi-tenant control plane, telemetry.

## 2. Core architecture

A single static Go binary, run as a one-shot job. Two pluggable interfaces.

- Source (read-only). `ListRepos(filters)`, `CloneURL(repo)`, `FetchMetadata(repo)`.
- Destination (create/put-only, no delete or overwrite method exists anywhere).
  `VerifyWorm()`, `PutImmutable(key, data, retention)`, `List(prefix)`.

### Pipeline (per run)

1. Resolve source auth, then enumerate repositories (apply include/exclude filters).
2. WORM check. Verify destination immutability. Warn and proceed if it isn't confirmed,
   or abort when `worm.require` is set.
3. Fan out across repos with bounded concurrency.
   - `git clone --mirror`
   - `git lfs fetch --all`
   - metadata to JSON (issues, PRs/MRs, comments, labels, milestones, wikis, releases)
   - bundle/tar, then SHA-256 checksum
   - `PutImmutable` at a deterministic, dated key
4. Write a signed run-manifest (per-repo status, checksums, versions, timing).
5. Emit structured logs and metrics. Exit non-zero on any failure.

### Object key layout

```
{host}/{org}/{repo}/{ISO8601-date}/{repo}.bundle
{host}/{org}/{repo}/{ISO8601-date}/{repo}.meta.json
{host}/{org}/{repo}/{ISO8601-date}/{repo}.sha256
{host}/{org}/{repo}/{ISO8601-date}/{repo}.lfs.tar          (when the repo has LFS)
{host}/{org}/manifests/{ISO8601-timestamp}.manifest.json   (signed, plus a .sig sidecar)
```

## 3. Sources

| Source | Endpoints | Auth |
|---|---|---|
| GitHub | GitHub.com and Enterprise Server (configurable base URL, `/api/v3`) | GitHub App installation token (preferred), or a token. Read-only: contents, metadata, issues, PRs, releases. |
| GitLab | GitLab.com and self-managed (configurable base URL) | Project/group access token or OAuth, read-only scopes. |

Metadata note. gitdr uses per-resource REST endpoints, which work with App-compatible
short-lived tokens. It does not use the GitHub org Migrations API. That one needs a
classic PAT with `repo` and `admin:org`, is a preview API with size limits and a 7-day
archive expiry, and doesn't work with App tokens or fine-grained PATs.

## 4. Destinations and WORM

gitdr writes to any supported object store. WORM immutability is verified at runtime and
strongly recommended. If the destination isn't immutable, gitdr warns loudly and proceeds.
`worm.require` (`--require-worm`) makes it fail closed. Configuring WORM is the operator's
job.

| Destination | Mechanism | Notes |
|---|---|---|
| AWS S3 | Object Lock (Compliance/Governance + legal hold) | reference implementation |
| Google Cloud Storage | Bucket Lock (locked retention) + Object Retention | |
| Azure Blob | Immutability policies (time-based retention + legal hold) | |
| Wasabi | S3 Object Lock | 90-day minimum retention |
| Backblaze B2 | S3 Object Lock (Compliance/Governance) | enable at bucket creation, versioning required |
| MinIO / IDrive E2 | S3 Object Lock | self-hosted WORM |
| Cloudflare R2 | none (no S3 Object Lock) | usable as a non-WORM destination, gitdr warns and proceeds |

Most are S3-compatible, so one S3 backend (configurable endpoint) covers AWS, Wasabi, B2,
MinIO, and IDrive. Only GCS and Azure need separate backends.

WORM check. Before any write, gitdr probes the lock configuration (S3
`GetObjectLockConfiguration`, GCS retention policy, Azure immutability policy). If it can't
confirm enabled-and-locked immutability, it warns loudly and proceeds. `--require-worm`
(`worm.require`, off by default) makes the run fail closed instead, for people who want a
hard immutability guarantee.

### S3-compatible providers

"S3-compatible" is a spectrum. Providers implement different subsets of the S3 API, and
the part gitdr leans on, S3 Object Lock (`GetObjectLockConfiguration` plus per-object
retention), is one of the least universally implemented. So AWS S3 is the reference
destination, and WORM is only guaranteed against providers that implement the Object Lock
API. A provider that doesn't simply can't be confirmed immutable, so gitdr warns and
proceeds (or fails closed under `--require-worm`).

Tested via `destination.s3.endpoint` plus `usePathStyle: true`, with no separate code (the
MinIO integration test exercises this path).

| Provider | endpoint | Object Lock | Quirks |
|---|---|---|---|
| AWS S3 | default | ✅ reference | none |
| Wasabi | `https://s3.<region>.wasabisys.com` | ✅ | 90-day minimum retention |
| Backblaze B2 | `https://s3.<region>.backblazeb2.com` | ✅ | enable at bucket creation, versioning required, no conditional writes |
| MinIO / IDrive E2 | `http(s)://host:9000` | ✅ | self-hosted, create the bucket with object lock |
| Cloudflare R2 | `https://<account>.r2.cloudflarestorage.com` | ❌ | no S3 Object Lock, usable as a non-WORM target, gitdr warns |

Providers not in this list work too, but verify their Object Lock support before you rely
on WORM. Credentials are static keys via the standard `AWS_*` env (the SDK default chain).
Scope them create/put-only.

## 5. Object storage authentication

Use each cloud SDK's default credential provider chain. One code path resolves static keys
and every workload-identity mechanism, now and future. Add an explicit static-key option
only for S3-compatible providers. Scope every credential create/put-only.

| Destination | Static keys | Keyless (preferred) |
|---|---|---|
| AWS S3 | IAM access key/secret (+ session token) | EC2 instance profile, EKS IRSA / Pod Identity, AssumeRole/STS, SSO |
| GCS | SA JSON key | Workload Identity Federation, GKE WI, metadata server |
| Azure Blob | account key / SAS | Entra ID with Managed Identity / workload identity / service principal |
| Wasabi / B2 / MinIO / IDrive | access keys only | none |

## 6. Security by design

- Least privilege. Source read-only, destination create/put-only. A compromised pipeline
  can't purge backups, and locked retention enforces this regardless.
- No long-lived secrets in the image. Prefer keyless workload identity. Static keys come
  from env or a mounted secret only, never logged.
- Encryption. TLS in transit, bucket SSE, and optional client-side envelope encryption
  before upload. A random per-file AES-256-GCM data key (chunked and streaming, so large
  bundles don't buffer) wrapped by a key from `GITDR_ENCRYPTION_KEY`. A KMS can wrap and
  unwrap the data key later without changing the format. Checksums cover the stored
  ciphertext, so `verify` stays key-free and `restore` needs the key.
- Integrity. SHA-256 per artifact plus a signed run-manifest, checked by `gitdr verify`.
- Hardened container. Wolfi/Chainguard base, non-root, read-only rootfs, no shell, plus
  `git` and `git-lfs`, pinned by digest.
- Fail closed, bounded concurrency, rate-limit aware, resumable.
- No telemetry.

## 7. Restore

`gitdr restore` fetches a bundle, verifies its checksum, `git clone`s it, and rehydrates
LFS. Git data restores faithfully. The metadata JSON is for audit and manual reference
only. The GitHub and GitLab APIs can't recreate original issue/PR numbers, authors,
timestamps, or cross-references. That's true of every backup tool, and it's documented for
users so nobody is surprised.

## 8. Supply chain and build

- Minimal pinned dependencies, committed `go.sum`, `-mod=readonly`, reproducible builds.
- `govulncheck` (fail on vuln), SBOM via syft.
- cosign keyless signing of the image and binaries (Sigstore OIDC), plus SLSA provenance.
- Base image pinned by digest.

## 9. Distribution

One GoReleaser config produces everything from one tag.

| Artifact | Variants |
|---|---|
| Container image (GHCR) | 1 multi-arch manifest (`linux/amd64` + `linux/arm64`) |
| Static binaries (Releases) | 2, `linux/amd64` and `linux/arm64` |
| Helm chart | OCI on GHCR |

One image, no flavor variants (no `-alpine`/`-debian`/`-slim`). Launch is container plus
binaries plus Helm. Homebrew, apt/dnf, Snap, AUR, and Nix come only if people ask.
Packaging files ship in-repo so downstream maintainers have little to do.

## 10. Licensing

AGPL-3.0. gitdr is free and open-source and stays that way. If the AGPL doesn't fit your
org, a commercial license is available from the maintainer.

## 11. Output contract (v2)

The run-manifest schema and the `--output json` shapes are a stable, versioned public
contract that downstream tooling consumes. Changes need a new schema version and a note
here. `internal/pipeline/manifest_test.go` pins the current field set.

v2 records the immutability observed at write time, since gitdr now writes to non-WORM
destinations too (§4). `destination.wormImmutable` and `wormDetails` capture it. The
manifest is signed, so this is a tamper-evident answer to "was this backup on WORM
storage?". `verify` doesn't check the schema string, so older manifests still verify.

### Object layout (per run)

```
{host}/{org}/{repo}/{YYYY-MM-DD}/{repo}.bundle      # git data, git bundle --all HEAD
{host}/{org}/{repo}/{YYYY-MM-DD}/{repo}.meta.json   # per-resource metadata dump (gitdr.meta/v1)
{host}/{org}/{repo}/{YYYY-MM-DD}/{repo}.sha256      # sha256sum line for the bundle
{host}/{org}/{repo}/{YYYY-MM-DD}/{repo}.lfs.tar     # LFS objects, when present
{host}/{org}/manifests/{YYYYMMDDThhmmssZ}.manifest.json       # signed run-manifest
{host}/{org}/manifests/{YYYYMMDDThhmmssZ}.manifest.json.sig   # detached signature
```

Every object is written create-only under object-lock retention.

### Run-manifest (`gitdr.manifest/v2`)

```json
{
  "schema": "gitdr.manifest/v2",
  "runId": "20260613T120000Z-a1b2c3d4e5f6",
  "tool": { "name": "gitdr", "version": "v0.1.0 (abc123def456)" },
  "source": { "type": "github", "host": "github.com" },
  "destination": { "type": "s3", "bucket": "my-worm-bucket", "wormMode": "COMPLIANCE", "wormImmutable": true, "wormDetails": "Object Lock enabled; default retention COMPLIANCE" },
  "startedAt": "2026-06-13T12:00:00Z",
  "finishedAt": "2026-06-13T12:03:00Z",
  "status": "success",
  "repos": [
    {
      "slug": "octo/hello",
      "status": "success",
      "artifacts": [
        { "kind": "bundle", "key": "github.com/octo/hello/2026-06-13/hello.bundle",
          "size": 12345, "sha256": "…", "retainUntil": "2026-07-13T12:00:00Z" }
      ]
    }
  ]
}
```

- `status` (run-level and per-repo): `success`, `failed`, or `skipped` (skipped means
  resume found this repo already backed up for the date).
- `repos[].error` is present only when that repo's `status` is `failed`.
- `artifacts[].kind`: `bundle`, `meta`, `sha256`, or `lfs`.
- Timestamps are RFC 3339 (UTC). The manifest is signed (Ed25519) over its exact stored
  bytes. The signature is base64 in the `.sig` sidecar and verified with the public key.

### Metadata (`gitdr.meta/v1`)

`{repo}.meta.json` is a per-resource dump for audit and reference, fetched via
App-compatible per-resource REST endpoints (never the Migrations API). It is not a
restorable snapshot, see §7. Sections hold the raw upstream objects.

- GitHub: `repo`, `labels`, `milestones`, `issues`, `comments`, `pullRequests`,
  `reviewComments`, `releases`.
- GitLab: `project`, `labels`, `milestones`, `issues`, `mergeRequests`, `releases`,
  `notes`.

Wikis are a separate git repository and are out of scope for the metadata dump.

### `--output json` (stdout, structured logs go to stderr)

| Command | Shape |
|---|---|
| `backup`  | the run-manifest above |
| `restore` | `{ "bundleKey", "sha256", "outDir", "verified" }` |
| `verify`  | `{ "manifestKey", "signatureValid", "artifactsChecked", "artifactsOk", "failures": [...] }` |
| `doctor`  | `{ "ok", "checks": [ { "name", "ok", "detail" } ] }` |

Exit codes are fail-closed, non-zero on any failure.
