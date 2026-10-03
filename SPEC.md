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
3. Fan out across repos with bounded concurrency. Each repository in two steps, and nothing is
   written until the first one has finished.
   - Read and prepare, writing nothing: metadata to JSON (issues, PRs/MRs, comments, labels,
     milestones, wikis, releases), `git clone --mirror`, `git lfs fetch --all`, the LFS objects
     into a tar, each removed once it is in it, `git bundle create`, then the mirror removed,
     encryption when it is on, and the SHA-256 of every artifact.
   - Upload with `PutImmutable` at deterministic, dated keys, largest first, and the `.sha256`
     sidecar last.
4. Write a signed run-manifest (per-repo status, checksums, versions, timing).
5. Emit structured logs and metrics. Exit non-zero on any failure.

A failure while reading and preparing leaves nothing under the date, so the same day's rerun
copies the repository cleanly. A failure among the uploads leaves part of a copy, whose keys are
create-only, and the same day's rerun fails the repository by name (§11); the next copy is made
the next UTC day. The sidecar goes last because a restore without the public key goes by it, and
a sidecar must not stand beside a partial copy. Up to v0.1.20 the bundle, the metadata and the
checksum were uploaded before the LFS objects were fetched, so an LFS failure, a token that
expired during the fetch for example, always left a partial copy. *Changed in v0.1.21.*

The metadata is fetched first, before the clone, so a wait for a rate limit holds no scratch
space. A failure likely to pass, a rate limit gitdr could not wait out or a server error that
outlasted its retries, fails the repository with nothing stored for it, and the next run tries
again. Any other metadata failure, a permission the credential lacks for example, would fail
every run the same way, so the code is stored anyway, the bundle, the LFS archive and the
checksum, and the repository then fails on its metadata. Up to v0.1.20 the metadata was fetched
after the bundle was stored, so a rate limit left a bundle under object lock with no metadata
beside it, and no checksum either. Only the GitHub source marks a failure as likely to pass, so on
GitLab every metadata failure stores the code first. *Changed in v0.1.21.*

### Object key layout

```
{host}/{org}/{repo}/{ISO8601-date}/{repo}.bundle
{host}/{org}/{repo}/{ISO8601-date}/{repo}.meta.json
{host}/{org}/{repo}/{ISO8601-date}/{repo}.sha256
{host}/{org}/{repo}/{ISO8601-date}/{repo}.lfs.tar          (when the repo has LFS)
{host}/{namespace}/manifests/{ISO8601-timestamp}.manifest.json   (signed, plus a .sig sidecar)
```

`{namespace}` is the deepest namespace that holds every repository in the run. On GitHub that is
the organisation or user. On GitLab it is the group above every subgroup the run covers, and a
run whose repositories share no namespace files its manifest under `{host}/manifests/`. Up to
v0.1.19 a run over several GitLab namespaces filed it under whichever namespace the source listed
first, which could change from one run to the next. The first such run after upgrading may not
find the previous manifest, and then copies every repository once instead of skipping the
unchanged ones. *Changed in v0.1.20.*

`{ISO8601-date}` is the UTC date the run started, read once, so every copy a run makes is filed
under the same date however long the run takes. Up to v0.1.20 the date was read again for each
repository, and a run that crossed midnight UTC filed the repositories it reached after midnight
under the next date, where a restore by the run's date did not look. *Changed in v0.1.21.*

## 3. Sources

| Source | Endpoints | Auth |
|---|---|---|
| GitHub | GitHub.com and Enterprise Server (configurable base URL, `/api/v3`) | GitHub App installation token. gitdr mints it from the App key, or reads it from a token file (below). Read-only: contents, metadata, issues, PRs, releases. |
| GitLab | GitLab.com and self-managed (configurable base URL) | Project/group access token or OAuth, read-only scopes. |

Metadata note. gitdr uses per-resource REST endpoints, which work with App-compatible
short-lived tokens. It does not use the GitHub org Migrations API. That one needs a
classic PAT with `repo` and `admin:org`, is a preview API with size limits and a 7-day
archive expiry, and doesn't work with App tokens or fine-grained PATs.

### GitHub token file

`source.github.tokenPath` (`GITDR_SOURCE_GITHUB_TOKENPATH`) names a file holding a GitHub App
installation token, and gitdr then works without the App's private key. It reads the file again
before every API request and every git command and keeps no copy in between, so whoever writes
the file can replace it during a run.

It exists for a caller that runs backups for many organisations from one App, like a hosted
scheduler. The App's private key can mint a token for every installation of the App, so a run
holding it can reach every organisation that installed it. A run needs read access to one. The
caller mints that token with read permissions, writes the file, and replaces it before the
token's hour is up.

- Set the token file or the App key, never both. With both set, `backup` and `doctor` refuse to
  start. `appID` and `installationID` are ignored when the token file is set.
- The file holds one installation token, and whitespace around it is ignored. Repositories are
  listed with `GET /installation/repositories`, which accepts nothing else, so a personal access
  token fails there.
- Replace the file by writing a new one beside it and renaming it over the old one. A missing,
  empty or unreadable file fails the request or git command that read it, and so that repository
  and the run. An expired token does the same, since GitHub answers 401. Nothing falls back.
- A git command reads the file once, when it starts. One that outlives its token, such as a very
  long `git lfs fetch`, can fail, so replace the file well before the hour.
- git receives the token as it always has, in an `http.extraHeader` passed through
  `GIT_CONFIG_*` environment variables and scoped to the source host. It never appears on a
  command line or in a URL. API requests carry it only over HTTPS to the configured API host.
- Errors name the file and what is wrong with it, never its contents. `doctor` checks the token
  with one request to GitHub. `verify`, `restore` and `drill` never read the file.

The manifest and `--output json` are unchanged. *Added in v0.1.20.*

## 4. Destinations and WORM

gitdr writes to any supported object store. WORM immutability is verified at runtime and
strongly recommended. If the destination isn't immutable, gitdr warns loudly and proceeds.
`worm.require` (`--require-worm`) makes it fail closed. Configuring WORM is the operator's
job.

| Destination | Mechanism | Notes |
|---|---|---|
| AWS S3 | Object Lock (Compliance/Governance + legal hold) | reference implementation |
| Google Cloud Storage | Bucket Lock (locked retention) + Object Retention | |
| Azure Blob | Time-based retention policy on the container, locked | the lock is read through Azure Resource Manager, see below |
| Wasabi | S3 Object Lock | compliance mode, Cohasset-assessed |
| Backblaze B2 | S3 Object Lock (Compliance/Governance) | turn it on for a new or existing bucket; B2 buckets are always versioned |
| MinIO / IDrive E2 | S3 Object Lock | self-hosted WORM |
| Cloudflare R2 | none (no S3 Object Lock) | bucket locks stop deletes, an administrator can remove them, gitdr cannot read them and reports unknown |

Most are S3-compatible, so one S3 backend (configurable endpoint) covers AWS, Wasabi, B2,
MinIO, and IDrive. Only GCS and Azure need separate backends.

WORM check. Before any write, gitdr probes the lock configuration (S3
`GetObjectLockConfiguration`, the GCS retention policy, the state of the Azure container's
immutability policy). If it can't confirm enabled-and-locked immutability, it warns loudly and
proceeds. `--require-worm` (`worm.require`, off by default) makes the run fail closed instead,
for people who want a hard immutability guarantee.

Two answers are not taken at their word, on S3, by `backup` and `doctor` alike.

- **`ObjectLockConfigurationNotFoundError` is believed once a listing shows the bucket exists.**
  MinIO gives that same answer about a bucket that does not exist, so gitdr reported a bucket
  nobody had created as `not-immutable`. Now such a bucket reads `unknown`, and the listing's own
  answer, `NoSuchBucket`, is its code. The listing is one request for one key. It needs
  `s3:ListBucket` on AWS and `listFiles` on Backblaze, which the credentials in
  [`docs/QUICKSTART.md`](./docs/QUICKSTART.md) already hold. HeadBucket is not used: it answers
  without a body, so a missing bucket and a refused one look alike, and on Backblaze it needs
  `listBuckets`, which those keys do not have.
- **An answer that is not S3's is no answer.** Where a lock configuration, a listing or an
  object's retention belongs, a success has to be that document and a failure an S3 error
  document. A web page, an empty body or another call's document fails the read instead. So a
  backup records the verdict `unknown` and an object's retention `not-checked`, never the earned
  negatives `not-immutable` and `absent`, and doctor gives no verdict and the code `not-s3`. A
  well-formed page used to parse as a lock configuration with nothing in it, which read as
  "Object Lock not enabled".

*Changed 2026-10-03. `gitdr.manifest/v5` is unchanged; these inputs now give it `unknown` and
`not-checked` where they gave `not-immutable` and `absent`.*

### Azure

A container is immutable only under a time-based retention policy that is **locked**. An
unlocked policy can be shortened or deleted by the account owner. Version-level immutability on
its own locks nothing: it lets blob versions carry policies, and none does until a default
policy is set on the container, and that policy is only a lock once it is locked. Up to v0.1.18
gitdr decided from the version-level flag alone and was wrong in both directions: a container
with the flag read `immutable` whether or not anything was locked, and a container under a locked
container-level policy, the usual setup, read `not-immutable`. §11 says what that means for
manifests already written.

Only Azure Resource Manager reports whether a policy is locked. The blob endpoint says whether a
container has a policy (`x-ms-has-immutability-policy`) and whether it has version-level
immutability, and never the policy's state or period. A written blob's properties do carry its
policy's mode and expiry on a version-level container, but only after the write. The WORM check
has to answer before the first one, because `--require-worm` refuses to write at all, and
nothing seen after a write ever raises a verdict (v5, below). So the verdict comes from:

| What gitdr read | Verdict |
|---|---|
| Resource Manager: the container's policy is `Locked`, with its period | `immutable` |
| Resource Manager: the policy is `Unlocked` | `not-immutable` |
| Either API: no policy and no version-level immutability | `not-immutable` |
| A policy or version-level immutability, and no Resource Manager read | `unknown` |
| Version-level immutability and no container policy (a policy on the storage account is not read) | `unknown` |
| Resource Manager refused (`AuthorizationFailed`, `ResourceNotFound`, ...) or could not be reached | `unknown` |

A legal hold counts as neither. It holds until someone with the right role clears it, which is
the same kind of protection as an unlocked policy.

The Resource Manager read is opt-in: set `destination.azure.subscriptionID` and
`resourceGroup`, with `account`. What it changes about credentials:

- It needs an Entra ID identity through `DefaultAzureCredential` (managed identity, workload
  identity, service principal). An account key, a SAS or a connection string cannot
  authenticate to Resource Manager. With a connection string for the blob endpoint, gitdr still
  asks `DefaultAzureCredential` for this one read.
- The RBAC action is `Microsoft.Storage/storageAccounts/blobServices/containers/read`, the one
  the blob endpoint already requires for Get Container Properties. It is in Storage Blob Data
  Reader and Contributor, so the role gitdr writes with already carries it, whether it is
  assigned on the container or the account.
- The request goes to `management.azure.com`, which a locked-down network has to allow as well
  as the blob endpoint.
- The blob endpoint has to be the account's own in Azure's public cloud, where Resource Manager
  is read: `https://<account>.blob.core.windows.net/`, its `privatelink` name or its Azure DNS
  zone endpoint, with no container in the path, or an emulator on this machine
  (`http://127.0.0.1:10000/<account>`). gitdr refuses any other, because a lock read off another
  account's container would be reported as protecting this one. Up to v0.1.20 any host whose
  first label was the account passed, `https://acme.example.org/` included, and so did another
  cloud's endpoint and a path naming another container. *Changed in v0.1.21.*

`account` must be a storage account name, 3 to 24 lowercase letters and digits, because the blob
endpoint's host is built from it and the credential is sent to that host. Up to v0.1.20 any value
was taken, and `x@example.org/` sent the requests to `example.org`. *Changed in v0.1.21.*

Blobs are uploaded in 1 MiB blocks until a blob would need more than Azure's 50,000, at
48.8 GiB, and above that in the smallest whole number of MiB that fits, so a blob can reach
Azure's limit of about 190.7 TiB. Up to v0.1.20 every blob went in 1 MiB blocks, and one over
48.8 GiB failed after all of it had been sent. *Changed in v0.1.21.*

After a write, on a version-level container, gitdr reads the first blob's policy back. A blob
that carries none is the earned negative of v5. Blobs under a container-level policy never carry
one, however locked the container is, so there it records `not-checked`.

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
| Wasabi | `https://s3.<region>.wasabisys.com` | ✅ | compliance mode, Cohasset-assessed |
| Backblaze B2 | `https://s3.<region>.backblazeb2.com` | ✅ | Object Lock can be turned on for a new or existing bucket; always versioned; `If-None-Match` answers 501, so gitdr checks each key with `HeadObject` before writing it |
| MinIO / IDrive E2 | `http(s)://host:9000` | ✅ | self-hosted, create the bucket with object lock |
| Cloudflare R2 | `https://<account>.r2.cloudflarestorage.com` | ❌ | bucket locks, not Object Lock; gitdr cannot read them and reports unknown |

Providers not in this list work too, but verify their Object Lock support before you rely
on WORM. Credentials are static keys via the standard `AWS_*` env (the SDK default chain).
Scope them create/put-only.

Every write carries the object's CRC32 in the `x-amz-checksum-crc32` header, which gitdr
computes before the upload. AWS and B2 want a checksum on a write under Object Lock. Asked for
the algorithm alone, the AWS SDK computes the checksum as it sends, and over TLS it sends the
body `aws-chunked` as a single chunk with the checksum in a trailer. MinIO refuses a chunk over
16 MiB, so before this no object over 16 MiB could be written to MinIO over TLS. *Changed in
v0.1.21.*

A write whose answer was lost is settled by asking the store. The SDK sends such a write again,
and when the first one had landed, AWS refuses the second with 412, because of `If-None-Match`.
gitdr then reads the object's size and CRC32 with `HeadObject` in checksum mode, and when they
are this write's, the write succeeded. A different object at the key is still refused, and so is
one the store will not give a checksum for. A write settled this way records the retention
`HeadObject` shows the object held to, and not the one it asked for: on AWS the object at the
key can be an earlier write's with the same bytes, held to that write's date and mode.
AWS shows the lock on `HeadObject` only to a key allowed `s3:GetObjectRetention`, and Backblaze
documents no lock headers on it at all. Where none is shown the write records none. *Changed in
v0.1.22.*

An object over 4 GiB is written in parts, because a single PutObject stops at 5 GiB on AWS, B2,
R2 and Wasabi. Parts are at least 64 MiB, and larger for a larger object so it stays under 9,000
of them. Four go at a time, each with its CRC32 in a header, and a failed part is sent again on
its own. The upload asks for a full-object CRC32, or for a composite one from a store that refuses
that. Create-only holds as it does for one PUT. The key is checked before the upload starts, on
every store, so gigabytes are never sent only to be refused. On AWS, completing the upload carries
`If-None-Match` and the object's size; elsewhere the key is checked again just before. Completing
is sent once, and a lost answer is settled as above. An upload is never aborted: one a stopped
run leaves behind is ended by the bucket's lifecycle rule, which `docs/QUICKSTART.md` gives.
`destination.s3.multipartThreshold` and `destination.s3.partSize`, in bytes from 5 MiB to 5 GiB,
move the threshold and the smallest part, so a test can write in parts without writing gigabytes.
*Changed in v0.1.22.*

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

Confirming an Azure lock needs the keyless path: Resource Manager does not accept an account key
or a SAS. See §4, Azure.

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
- Fail closed, bounded concurrency. A same-day rerun skips the copies a manifest records and
  fails by name any repository whose copy that day did not finish (§11). A run does not yet
  resume a copy another run left unfinished, so "resumable" is not claimed. *Changed in v0.1.21.*
- Rate limits. A GitHub API request refused for the primary rate limit waits until the reset
  GitHub names. The wait is measured from the `Date` of the refusal, so a host clock that is off
  from GitHub's neither retries early nor waits too long. A refusal go-github makes itself, without
  sending the request, has no `Date` and is measured on this host's clock. A request refused for a
  secondary limit waits out its `Retry-After`. Without one, go-github takes the wait from
  `X-RateLimit-Reset`, measured on this host's clock. A secondary limit that names no time, or a
  time already past, and a primary limit with no reset wait a minute, doubled for each wait
  already spent on the request: 1, 2, 4, 8 and 16 minutes. Every wait adds a second, and up to a
  second of jitter. A request still refused after five waits fails. When a wait would end after
  the run's deadline, or take the run past its budget for waits (below), the request fails at once
  and the error names the reset. A stop signal ends a wait early. A 5xx is tried four times, about
  one, two and four seconds apart, and so is a 5xx from the endpoint that mints the App's
  installation token. This covers the repository listing and every metadata request. The GitLab
  client retries a 429 and a 5xx itself. *Added in v0.1.21.*
- Run deadline. `gitdr backup --deadline <RFC 3339 time>`, or `GITDR_DEADLINE` when the flag is
  not given, stops the run's work at that time. A rate-limit wait that would end later is not
  started, and the repositories still in progress, or not yet reached, fail with the deadline as
  their error. The manifest is written after the work stops, so it records them. Writing it is not
  bounded by the deadline, so a caller that kills the process at a limit of its own should pass a
  deadline that leaves a minute for it. A deadline already past, or a value that is not an RFC 3339
  time, is refused before the run starts. Separately, `source.github.maxRateLimitWait`
  (`GITDR_SOURCE_GITHUB_MAXRATELIMITWAIT`, a Go duration, one hour when unset) bounds how long one
  run spends waiting for GitHub's rate limits. It counts the time something is waiting, once, however
  many requests wait together. *Added in v0.1.21.*
- Bounded transfers. gitdr runs git with `http.lowSpeedLimit=1000` and `http.lowSpeedTime=600`,
  so a clone, fetch or `ls-remote` that moves under 1000 bytes a second for ten minutes is
  aborted and its repository fails. Before this, a server that stopped sending while keeping the
  connection open could hold a run for good. git's own `GIT_HTTP_LOW_SPEED_LIMIT` and
  `GIT_HTTP_LOW_SPEED_TIME` override both values, as git documents, and gitdr has no setting of
  its own for them. git-lfs keeps its own inactivity timeout, `lfs.activitytimeout`, 30 seconds
  by default. *Added in v0.1.20.*
- What git sees. git and git-lfs parse whatever the source sends, so from gitdr's environment
  they get only `PATH`, `HOME`, `TMPDIR`, the proxy variables (`HTTPS_PROXY`, `HTTP_PROXY`,
  `ALL_PROXY` and `NO_PROXY`, in either case), the CA variables (`SSL_CERT_FILE`,
  `SSL_CERT_DIR`, `GIT_SSL_CAINFO` and `GIT_SSL_CAPATH`), and `GIT_HTTP_LOW_SPEED_LIMIT` and
  `GIT_HTTP_LOW_SPEED_TIME`. They also get gitdr's own `GIT_*` settings, which carry the source
  credential. A secret given to gitdr in its environment, such as the destination's credentials
  or the signing and encryption keys, never reaches them. They can still reach whatever gitdr's
  user can: a key given as a file (`manifest.signingKeyPath`, `source.github.privateKeyPath`, a
  cloud credentials file), and the identity and metadata endpoints of the machine or pod. So give
  keys in the environment, not as files. Up to v0.1.20 git got the whole environment. Any other
  variable that changed what git does, `GIT_CONFIG_GLOBAL` for instance, no longer reaches it,
  and gitdr names the ones it finds in a warning as it starts. Configure git in `/etc/gitconfig`,
  owned by root, and give each run an empty `HOME` of its own, so that a git compromised once
  cannot change what git does in the next run. git's messages are in English whatever the
  locale. *Changed in v0.1.21.*
- Reading gitdr. git runs as gitdr's user, and a process can read the environment and memory of
  another with the same user, through `/proc` or ptrace, unless that one is not dumpable. The
  kernel starts gitdr non-dumpable when the user it runs as cannot read its binary: the image
  installs `/usr/bin/gitdr` owned by root with mode 0711, and an install on a VM should do the
  same (`deploy/README.md`). gitdr also makes itself non-dumpable in its init, which covers a
  readable binary from then on but not the few milliseconds the Go runtime takes to get there.
  On a host that sets `fs.suid_dumpable=1` the mode does not help, and only the init does.
  Neither stops root, or a process holding `CAP_SYS_PTRACE`. *Changed in v0.1.21.*
- gitdr's own secrets leave its environment. Once read, `GITDR_GITHUB_APP_PRIVATE_KEY`,
  `GITDR_GITLAB_TOKEN`, `GITDR_MANIFEST_SIGNING_KEY`, `GITDR_ENCRYPTION_KEY` and
  `GITDR_DESTINATION_AZURE_CONNECTIONSTRING` are taken out of gitdr's environment, before any
  storage client is built. A process a cloud SDK starts for credentials, such as an AWS
  `credential_process` or the Azure CLI, runs with gitdr's environment, the cloud's own variables
  included, and without these. *Changed in v0.1.21.*
- Bounded memory. gitdr runs git with `pack.threads=2`, `pack.windowMemory=256m`,
  `pack.deltaCacheSize=128m`, `core.bigFileThreshold=64m`, `core.packedGitWindowSize=32m` and
  `core.packedGitLimit=256m`. git's defaults suit a workstation. It maps packs a gibibyte at a
  time with no limit short of 32 TiB, so `git pack-objects`, bundling a 6 GiB pack, kept 3.7 GiB
  of it resident in a container limited to 4 GiB, where it now keeps 265 MiB. And it sizes its
  threads by the CPUs it sees, the node's in a pod without a CPU limit. `git index-pack`, under
  every clone and restore, takes half of them, up to 20, each holding whole blobs and 96 MiB of
  cached bases, so a clone of large files stored as deltas took 1 GiB on 14 CPUs, and takes 297
  MiB on two threads. The settings change how much memory git uses, not the objects, refs or
  deltas it fetches and bundles. gitdr has no setting for them, and as `GIT_CONFIG_*` pairs they
  outrank any git configuration file. *Added in v0.1.22.*
- No telemetry.

## 7. Restore

`gitdr restore` fetches a bundle, verifies its checksum, `git clone`s it, and rehydrates
LFS. Git data restores faithfully.

Four checks run, and they are not the same check:

- **SHA-256 against the `.sha256` sidecar**, which the signed manifest covers. This is the
  integrity guarantee: any changed byte fails here.
- **`git bundle verify`**, which reads the bundle's header — format, prerequisites, refs — and
  stops. It does not read the packfile body, so it is a structural check, not an integrity one.
  Git also refuses to run it outside a repository, so gitdr runs it from a scratch one.
- **The ref comparison**, which is the one that proves a restore rather than asserting it.
  `git bundle list-heads` prints the ref-to-commit map the bundle itself declares, and gitdr
  checks every entry against the refs the restored repository actually has. Git is
  content-addressed, so a commit id transitively covers its tree, its blobs and its whole
  ancestry: equality of the ids is equality of the history, not a sample of it. A declared ref
  that is missing, or present at another object, fails the restore and names the first one
  that differs. It runs with or without a public key; without one the wording says the bundle
  it compared against was not itself verified against a signed manifest.
- **LFS pointers.** After the clone, and after the LFS objects are put back when the backup has
  them, restore reads the working tree back. A file under 1024 bytes that starts with a git-lfs
  pointer line is a pointer, not the file, and any pointer left fails the restore and the drill
  of it. When the backup holds no LFS archive for the repository, the error says the content was
  not backed up. Up to v0.1.19 that case passed, so a backup taken with `backup.lfs: false`, or
  without git-lfs installed, restored and drilled clean with 130-byte pointers where the files
  should be. The check does not need git-lfs. It reads only the checked-out tree, and a text file
  that is itself a pointer, such as a git-lfs test fixture, fails it. *Added in v0.1.20.*

The order matters: the checksum runs first, so a corrupt copy is refused before git is asked
anything. Removing or weakening the checksum would leave only a check that does not look at
the data. The ref comparison runs immediately after the clone, before LFS, because if the
history is wrong nothing after it matters.

Normalisation, since it is where the bugs live. A clone from a bundle files every branch
under `refs/remotes/origin/*` and materialises only the checked-out one under `refs/heads/*`
(none at all when the source's HEAD was detached), so a declared `refs/heads/X` is satisfied
by either name. Tags compare directly and without peeling, an annotated tag being recorded in
the bundle header as its tag object and read back as the same. The header's `HEAD` entry is
not a ref any repository stores, so it is compared against what the restored repository's HEAD
resolves to. Refs the restore has and the bundle does not are expected, not a failure: `git
clone` manufactures the whole `refs/remotes/origin/*` namespace itself, and only the
bundle-to-restore direction can catch lost history.

`git clone`'s default refspec creates nothing outside `refs/heads/*` and tags, so a bundle's
`refs/notes/*`, `refs/merge-requests/*`, `refs/pull/*` and `refs/keep-around/*` entries arrive
as objects with no ref pointing at them. Those are counted separately and named rather than
counted as matched — the score has to show the gap — but they are not a failure, because the
cause is the refspec and not the backup, and a check that fires on every healthy GitLab
restore is a check that gets switched off.

None of this changed the manifest schema or `--output json`. The counts travel in the human
output and on `RestoreResult.Refs`, which is `json:"-"`; putting them on the wire is a
separate, deliberate change.

The metadata JSON is for audit and manual reference
only. The GitHub and GitLab APIs can't recreate original issue/PR numbers, authors,
timestamps, or cross-references. That's true of every backup tool, and it's documented for
users so nobody is surprised.

### Which manifest a restore checks

With a public key configured, restore checks the bundle against the signed manifest of the run
that wrote it.

`-manifest <key>` names that manifest, with the key `backup` prints as `manifestKey`. `-repo`
then picks the repository in it, and the host and date come from the manifest, so `-host` and
`-date` are refused. `-manifest` needs the public key. The manifest has to verify, carry a
`gitdr.manifest/` schema and be named for its own `finishedAt`. This is the form for anything a
drill passed, since `gitdr restore -manifest <drilled key> -repo <slug> -out <dir>` restores
exactly what the drill restored.

Without `-manifest`, restore looks in the repository's own namespace, then each namespace above
it, then `{host}/manifests/`, for a manifest that finished on the backup's date or the day
after. That finds every run from v0.1.20 on that finished within a day of its copy. It does not
find a manifest an older engine filed under another namespace, or a run that took longer than
that. Use `-manifest` for those.

The manifest has to record the bundle as part of a copy its run finished: a `success` entry for
the repository that lists it. Up to v0.1.20 any entry that listed the bundle was taken, so a
repository that failed after its bundle was stored, on its metadata for example, restored by date
as if it had been copied, while `-manifest` refused the same entry. Both refuse it now, and a
same-day rerun uses this same search to decide what is already backed up (§11). *Changed in
v0.1.21.*

`-repo` splits at the last slash, so `acme/platform/api` is the project `api` in the group
`acme/platform`. It used to split at the first, and a project in a GitLab subgroup could not be
restored at all.

*Added in v0.1.20. The manifest and `--output json` are unchanged.*

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

## 11. Output contract

The signed documents (the run-manifest, `gitdr.manifest/v5`, and the drill report,
`gitdr.drill/v1`) and the `--output json` shapes are a stable public contract that downstream
tooling consumes. `internal/pipeline/manifest_test.go` pins the manifest's current field set.

**Schema versions name signed documents.** Adding a field to one, or changing what a field
means, takes a new version and a note here, because `schema` is how a reader asks whether a
document can carry a field, and a field that is absent is not the same as one that is empty.
A field printed to stdout beside a signed document changes no document, so it takes only a
dated note here. Examples are `backup`'s `manifestKey` and `drill`'s `reportKey`. The same
goes for a new command, flag or output shape. Removing or repurposing anything a consumer
already reads is a break under either rule, and is not done.

v2 records the immutability observed at write time, since gitdr now writes to non-WORM
destinations too (§4). `destination.wormImmutable` and `wormDetails` capture it. The
manifest is signed, so this is a tamper-evident answer to "was this backup on WORM
storage?". `verify` does not compare the schema *version*, so older manifests still verify — but from
2026-09 it does refuse a document that is not a manifest at all.

Before that, `verify -manifest` on a drill report printed `signature valid: true` and
`0 of 0 artifacts ok`, and exited zero. The signature check is schema-agnostic and correct,
because a signature is over bytes; the problem was what came after it. A drill report unmarshals
into a `Manifest` without error, having no artifacts, so the command reported success over a
document it cannot read — and would have gone on reporting success if that document were swapped
for anything else signed by the same key.

That is a behaviour change from exit 0 to non-zero for one input, and it is a fix rather than a
break: the previous answer was wrong. A drill report is signed evidence and deserves a real
check; `verify -manifest` is not it.

**v3 adds `repos[].refs` and `repos[].copiedAt`, and it is additive: every v2 field is
unchanged, both new fields are `omitempty`, and a v2 manifest still canonicalises to the bytes
it was signed over.** `internal/pipeline/manifest_test.go` pins that round trip, because
breaking it would make every artifact anyone has already stored unverifiable the day they
upgrade — which nobody would notice until they needed a restore.

`refs` is the ref-to-commit map the source advertised when the copy was made, from
`git ls-remote`. It exists so the next run can ask the same question and skip a repository
whose refs have not moved, rather than writing a byte-identical copy of its entire history.
Before this, `git bundle create --all` ran every night on every repository and the only thing
stopping a second write was a check on the date, so an organisation's whole history was
rewritten daily. On WORM storage the customer cannot delete any of it: object lock in
compliance mode holds against everyone, including the root of the account that owns the
bucket. Measured on three real organisations, roughly seventy per cent of one does not change
on a given day.

`copiedAt` is when the artifacts an entry relies on were actually written, and it is carried
forward through every skip. Without it each skip would restart the age of the copy, the
refresh below would never fire, and a repository that never changes would be skipped past its
object lock's expiry and end up with no copy at all.

**A copy is rewritten before it can expire, however unchanged the repository is.** Object lock
protects an object until its retain-until date and not one second longer, so skipping is only
correct while the copy being relied on still exists. gitdr refreshes after a third of the
retention period, capped at thirty days. See `internal/pipeline/unchanged.go`.

**The retention period there is the bucket's own lock when that is shorter, from v0.1.21
(2026-10-03).** A run skips a repository whose refs have not moved only while the copy it relies
on is younger than

    min(30 days, L / 3), where L = min(retention.days, P)

P is the lock period the destination reports for every copy it holds: a GCS bucket's retention
period, or an Azure container's immutability period as Resource Manager reports it, whatever the
policy's state. Where nothing reports one, L is `retention.days`. That is what gitdr locks each
object for on S3. On GCS and Azure gitdr locks nothing itself, so there `retention.days` only caps
the window. The copy's age runs from its `copiedAt`, the repository's own finish, so when a run
relies on a copy at least two thirds of its lock remain, less the time between the lock's start
and that finish. The read of the recent manifests stops at the same window, and a lock too short
to divide by three is never relied on. Up to v0.1.20 L was `retention.days` everywhere, ten days
at the default thirty, so in a bucket locked for one day a copy two days old was skipped a day
after its lock had ended. With a one-day lock the window is eight hours, and a daily schedule
copies every repository every day.

P is the period reported now. Cloud Storage and a container-level Azure policy apply it to every
copy they hold. Where a copy's lock was fixed when it was written, on S3 and in an Azure container
with version-level immutability, raising `retention.days` or the policy's period overstates the
lock of the copies written before, until they are refreshed. Without `subscriptionID` and
`resourceGroup` nothing reports an Azure period, and L is `retention.days`.

A same-day rerun's skip, `already backed up for this date`, relies on a copy made earlier that UTC
date, which a lock of a day or more still holds. Azure's shortest period is a day. A GCS bucket can
be given a shorter one, and Google does not promise to enforce a period under a day.

**Which runs the comparison reads, from v0.1.21.** The next run reads the recent manifests filed
in its own manifests directory, newest first, and each repository is decided by the newest one
that has an entry for it. After a `failed` entry that lists artifacts the repository is copied:
part of a copy was written, and an older copy of it is not believed over that. A `failed` entry
that lists no artifacts is passed over as if it were not there, and an older manifest decides:
nothing was written for the repository, as for one a stopped run never started, so the entry says
nothing about the copy it already has. The read stops once every repository the run selected is
decided, after ten manifests, or at the first manifest that finished longer ago than the refresh
bound, whose copies would all be refreshed anyway. A manifest that cannot be read, or that the
loader refuses, is passed over with a warning, and nothing in it is believed. Each manifest is
decoded one repository at a time, keeping only the selected repositories' refs and `copiedAt`. Up
to v0.1.20 only the newest manifest was read, so one run over a single repository, which files its
manifest in the organisation's directory, made the organisation's next run copy every other
repository in full. *Changed in v0.1.21. Amended 2026-10-04, before v0.1.21 was tagged: a failed
entry with no artifacts decided too, so a stopped run cost a full copy of every repository it
never started.*

**How large a manifest gitdr reads, from v0.1.21.** Every reader, the next run, a same-day rerun,
`restore`, `drill` and `verify`, reads at most 128 MiB of a manifest, about 1.5 million refs. One
past that is refused without being parsed, and unread where a listing already gave its size. The
next run warns, names the manifest and its size, and copies the repositories only it records. A
restore by date that passed one over says so and names the size, where it used to suggest a
rotated signing key. Up to v0.1.20 the cap was 32 MiB, about 390,000 refs, which an organisation
with a few large repositories passes because `--mirror` brings every `refs/pull/*`: the next run
logged it at debug and copied everything, and restore and drill refused the manifest, while
`verify` read a manifest of any size. *Changed in v0.1.21.*

**A skip relies only on a manifest the run can verify, from v0.1.21.** Every manifest the
comparison reads, and every manifest a same-day rerun relies on, has to verify with the public
half of the run's own signing key before anything in it is believed. One that does not is passed
over with a warning. Up to v0.1.20 the comparison read the previous manifest without its
signature, on the reasoning that a forged one could only make gitdr skip a repository. But a skip
relies on a copy, and a forged entry could name a copy that was never made and keep the
repository uncopied, every run green, until the refresh. A signing key rotated since a manifest
was written fails the check as a forgery does: the next run copies what that manifest recorded,
once, and a same-day rerun fails by name the repositories copied under the old key that day.

**A stopped backup files its manifest, from v0.1.21.** On SIGTERM or SIGINT, or at its deadline,
a backup starts no more repositories. The ones in flight are stopped and their git commands
killed, and a git-lfs or remote helper still holding git's stderr gets five seconds before the
pipe is closed on it. Every repository the run did not finish, in flight or never started, is
recorded as `failed` with an `error` that starts `stopped before it finished`. The manifest is
then written on a context the signal does not cancel, within 45 seconds of the stop, and the run
exits 1. The copies the run did finish are in that manifest, so `verify`, `restore` and `drill`
reach them, and the same day's rerun skips them. Up to v0.1.20 a stop cancelled the manifest's
own upload, so a stopped run filed nothing. A caller that kills the engine should wait a minute
after SIGTERM before it does.

Of those 45 seconds, the repositories in flight get 30. Their LFS archive, encryption and
checksums stop at their next read, as their git commands and their uploads do. One still running
30 seconds after the stop, in a step that does not stop at once, is recorded as `failed` with
`stopped before it finished: <cause>; still running 30s after the stop` and the artifacts it had
written by then, and the manifest is written without waiting for it any longer. *Amended
2026-10-04, before v0.1.21 was tagged: the run waited for every repository in flight without a
bound, and those steps ran to the end of their file, so a repository archiving or hashing many GiB
when the run was stopped could outlast the 45 seconds and take the manifest with it.*

Skipping is reported as `status: "skipped"` with a `reason`, the same shape already used for a
repository with no commits — additive, and a consumer switching on `status` sees a value it
already knows.

The two existing reasons are whole strings a consumer compares exactly. The new one carries the
date of the copy being relied on, so **`"unchanged since"` is a stable prefix and the date
follows it**. A consumer matches the prefix. Making it dynamic without saying so would have left
every consumer falling through to "skipped for some reason" on what is now the most common
outcome of a run.

### The drill report (`gitdr.drill/v1`)

`verify` reads the artifacts back and rechecks their checksums against the signed manifest.
That answers *is the copy intact*, which is SOC 2 A1.2. **It does not answer *does it
restore*, which is A1.3**, and this product does not blur the two.

`gitdr drill` restores. It writes each repository from the manifest into a temporary
directory, compares what came back, throws it away, and stores a signed report beside the
manifest it drilled — through the same create-only path as everything else, so the evidence is
as immutable as the thing it proves.

Three ref maps, and the drill checks both joins:

| | |
|---|---|
| **R** | what the source advertised when the copy was made — the manifest's v3 `refs` |
| **B** | what the bundle's own header declares — `git bundle list-heads` |
| **C** | what a fresh clone of that bundle contains — `git for-each-ref` |

`B == C` says the artifact restores to the history it claims to carry. `R == B` says it claims
to carry the history the source actually had — which nothing could check before v3 recorded R,
and which is the half an auditor is really asking about. **A bundle written from a half-fetched
mirror passes the first check perfectly and fails the second.**

Both joins report into the same `mismatches` array, and a source failure is prefixed
`source: `. That prefix is part of `gitdr.drill/v1`, not an implementation detail: it is the
only thing distinguishing "a ref the bundle declared came back at a different object" from "the
bundle never declared a ref the source advertised". A consumer that counts the array without
reading the prefix mixes the two, and the two are not the same finding - the first says what
came back is wrong, the second says something was never copied. Only the unprefixed entries
belong to the ref accounting, where `restoredRefs + unreferenced + mismatches == bundleRefs`.

A prefixed entry names the source as the side that recorded the object: *the source advertised
X, the restored repository has no such ref*. It said *bundle declares X* until v0.1.15, which
was backwards - the bundle not declaring that ref is the whole finding - and it said it inside a
signed report. The field, the prefix and the accounting are unchanged; only that clause is
corrected, and a consumer reading the ref name (the first token, since git forbids whitespace in
one) sees no difference.

The prefix itself was already true and nowhere written down.

Git is content addressed, so a commit hash covers its tree, its blobs and its whole ancestry.
Comparing ref maps is therefore not a sample of the data, it is a proof that the histories are
equal. A vendor holding backups in a proprietary blob store has nothing to compare against and
cannot let a customer reproduce the check without publishing the format and the reader.

What it does not prove, stated because the report is read by people who will act on it:

- **Not that the content is what somebody remembers.** Equal hashes mean equal content, but a
  source that was already wrong when the copy was made is faithfully wrong in the copy.
- **Not that unreferenced objects survive.** A clone's refspec creates no ref for a bundle's
  `refs/merge-requests/*` or `refs/notes/*`, so those are counted apart and named, never folded
  into the matched total.
- **Not the whole estate, unless it says so.** `-sample` caps how many are restored, and the
  report records `eligible` and `drilled` separately so a ten-repository sample cannot be read
  as a thousand-repository guarantee.
- **`sourceMatch` is null, not false, for a pre-v3 copy.** "Not recorded" and "did not match"
  are different answers.
- **Not LFS content off the checked-out tree.** The pointer check reads the working tree at
  HEAD, so a pointer that exists only on another branch or in history is not checked.

A manifest whose signature does not verify is **refused**, not drilled: evidence about an
artifact set nobody can attribute to gitdr is worse than no evidence.

**Which manifest a drill reads.** A drill refuses a document whose schema does not start with
`gitdr.manifest/`, as `verify` already did, and a manifest whose name is not its own
`finishedAt` in the form `20060102T150405Z.manifest.json`, which every manifest gitdr has
written satisfies. Without `-manifest` it takes the newest manifest filed directly under
`{host}/{namespace}/manifests/` and ignores names later than now. Before, a byte copy of an old
manifest stored under a later name stayed the newest for good, and every drill after it tested
the old run. Restore applies both refusals, and a backup's read of the previous manifest makes
the same choice of newest. *Added in v0.1.20. `gitdr.drill/v1` is unchanged.*

A drill no longer fails a repository because a restore without `-manifest` could not find its
manifest. That check stood in for the flag. *Changed in v0.1.20.*

```
{host}/{namespace}/drills/{ts}.drill.json      # the report
{host}/{namespace}/drills/{ts}.drill.json.sig  # ed25519 over the report, base64
```

**Where the report went.** `drill --output json` prints the report, then two fields after it,
the way `backup` prints `manifestKey` after the manifest:

| field | meaning |
|---|---|
| `reportKey` | the object key the signed report was stored under, the value `verify -drill` takes. `null` when no report was written |
| `reportNotWritten` | present only when `reportKey` is `null`, and one of two whole strings: `no-report` (the drill ran with `-no-report`) or `store-failed` (the destination refused the report: exit 3, or 1 if a repository also failed) |

`reportKey` is in every document from v0.1.19, as `null` rather than absent, because absent is
what an older engine prints and a consumer has to be able to tell "no report" from "too old to
say". Before it, the only place the key appeared was the `drill report written` log line, which
still carries it. That line is a stopgap for consumers written before 0.1.19 and not part of
this contract: logs are not an interface, and a consumer should read `reportKey`.

**`drill -no-report`** is the same drill with nothing written. No report is signed or stored, so
it needs a read credential and the public key, and it never loads the signing key, even when the
config names one. Everything else holds: the manifest's signature is checked and a manifest that
fails it is refused, both joins run, and the exit codes mean what they meant (3 cannot happen,
since nothing is stored). It lets an auditor re-run the restore from the customer's own bucket
with nothing but this binary, the public key and read access. The printed report is the only
record and it is not signed: evidence of what the auditor saw, not something anyone else can
verify later.

**Why `gitdr.drill/v1` did not move.** The version names the signed document, and the stored
report is byte for byte what v0.1.18 wrote. `-no-report` produces that same report without
storing it, and only stdout gains two fields, appended after the report's own. That is the
case of `backup`'s `manifestKey`, the `verify -drill` shape and exit 3, each added with a dated
note and no version change. A bump would also break something real: `verify -drill` accepts
exactly `gitdr.drill/v1`, so every report written after a bump would fail `verify -drill` on
every engine pinned before it.

*Added in v0.1.19. `gitdr.drill/v1` and `gitdr.manifest/v5` are unchanged.*

### Object layout (per run)

```
{host}/{org}/{repo}/{YYYY-MM-DD}/{repo}.bundle      # git data, git bundle --all HEAD
{host}/{org}/{repo}/{YYYY-MM-DD}/{repo}.meta.json   # per-resource metadata dump (gitdr.meta/v1)
{host}/{org}/{repo}/{YYYY-MM-DD}/{repo}.sha256      # sha256sum line for the bundle
{host}/{org}/{repo}/{YYYY-MM-DD}/{repo}.lfs.tar     # LFS objects, when present
{host}/{namespace}/manifests/{YYYYMMDDThhmmssZ}.manifest.json       # signed run-manifest
{host}/{namespace}/manifests/{YYYYMMDDThhmmssZ}.manifest.json.sig   # detached signature
```

Every object is written create-only under object-lock retention. `{namespace}` is the one §2
defines.

### Run-manifest (`gitdr.manifest/v5`)

```json
{
  "schema": "gitdr.manifest/v5",
  "runId": "20260613T120000Z-a1b2c3d4e5f6",
  "tool": { "name": "gitdr", "version": "v0.1.0 (abc123def456)" },
  "source": { "type": "github", "host": "github.com" },
  "destination": { "type": "s3", "bucket": "my-worm-bucket", "wormMode": "COMPLIANCE", "wormImmutable": true, "wormVerdict": "immutable", "wormDetails": "Object Lock enabled; default retention COMPLIANCE", "retentionObserved": "present" },
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

**`artifacts[].retainUntil` does not mean the same thing on every backend, and today the manifest
does not say which it is.**

- On **GCS** it is *observed*: the value the store returned for the object it just wrote
  (`RetentionExpirationTime`).
- On **S3** it is *requested*: the retain-until gitdr sent. `PutObject` returns no object-lock
  headers at all, so there is nothing to observe at write time, and the field records an
  instruction rather than an answer. The one exception is a write settled after its answer was
  lost or refused (§4): it records what `HeadObject` shows the object at the key held to, or
  `0001-01-01T00:00:00Z` when it shows none, so the field never claims more than the store
  showed. *Changed in v0.1.22.*

Both are written into a signed document, which makes the S3 case a claim gitdr has not earned:
if a store accepted the write and ignored `x-amz-object-lock-mode`, the manifest still names a
date, signed, and nothing in it distinguishes that from a date the store confirmed.

This is disclosed rather than fixed because the fix changes the schema. Nothing in the engine
reads this field — it is written once and consumed only by whoever believes the document — so
the correction is to observe the retention on one written object per run and downgrade the
manifest's verdict when a store says the object holds nothing. That needs a schema version and
is tracked separately.

Until then: **read `retainUntil` as the retention that applies if the destination honoured the
request, and `destination.wormVerdict` as the only statement about whether it said it would.**

- `status` (run-level and per-repo): `success`, `failed`, or `skipped`.
- `repos[].error` is present only when that repo's `status` is `failed`. An error from git carries
  at most the last 64 KiB or so of git's stderr, the end, where git says what went wrong, and
  then says how much was cut: `[git stderr cut: the first N bytes dropped, the last M kept]`. Up
  to v0.1.20 it carried all of it, and one noisy clone could make a manifest too large to read.
  *Changed in v0.1.21.*
- `repos[].reason` is present only when that repo's `status` is `skipped`, and says which of
  two cases applies:
  - `already backed up for this date` — a manifest records a complete copy of the repository
    under this run's date, made by an earlier run that day. *Narrowed in v0.1.21*, see below.
  - `repository has no commits` — nothing to bundle. A repository created and never pushed to
    has no refs, and `git bundle create` refuses to write an empty bundle. Skipping it is what
    keeps one unused project in an organisation from failing every backup of it for ever.
    The **metadata is still stored**: a repository with no code can still carry issues, labels
    and milestones, so such an entry has a `meta` artifact and no `bundle` or `sha256`.

  Neither case fails the run. Added as a field rather than as new `status` values, so a
  consumer switching on `status` is unaffected; the field is optional and absent on any repo
  that was not skipped.

  `reason` is a **prefix**, not a whole string: the unchanged path emits
  `unchanged since <RFC 3339>`, so a consumer matching this field matches on the prefix.

  **What `already backed up for this date` means, from v0.1.21.** The string is unchanged and its
  meaning narrows to "a recorded, complete copy exists". A same-day rerun skips a repository only
  when a manifest records the copy under the run's date, found the way a restore by date finds
  one (§7): in the repository's namespace, one above it or at the host, named for the date or the
  day after, with an entry for the repository that is a `success` listing the bundle, or for a
  repository with no commits a `repository has no commits` skip listing its metadata. Every object
  filed under the date has to be one that entry lists, and every artifact it lists has to be
  there. The skip then carries the copy's `refs` and `copiedAt`, as an unchanged skip does, so
  the next day still skips a repository that has not moved.

  Anything else under the date fails the repository, and is never skipped. Its `error` starts
  `an incomplete copy for <date> exists` and ends `its keys are create-only, so the next copy is
  on <date + 1>`, with what was found in between. Up to v0.1.20 any bundle under the date, or for
  a repository with no commits any metadata, counted as a finished copy. A copy whose checksum,
  metadata or LFS archive never landed was then reported as backed up, the run exited 0, and the
  skip carried no `refs`, so the next day copied the repository in full. The narrower meaning is
  a fix and not a break: the wider one reported copies that did not exist.

  Two more checks hold before a skip relies on a copy. An object under the date counts only if
  the destination says it wrote the object on that date: S3's `LastModified`, the creation time
  on GCS and on Azure. A run writes a date's objects on that date, so objects put there ahead of
  their date fail that day's run instead of being skipped by it, and so does an object the store
  gives no time for. The one legitimate case it refuses is two runs of the same date at once,
  where the second reaches a repository the first wrote after midnight. And a `copiedAt` later
  than the `finishedAt` of the manifest that holds it, or later than now, is not one a run
  recorded. A skip measures the copy's age from it, and an age that never grows never reaches
  the refresh, so one such manifest could have kept a repository skipped while every run
  stayed green. A same-day rerun fails such a repository by name, and the next run's comparison
  copies it. *Added in v0.1.21.*
- `artifacts[].kind`: `bundle`, `meta`, `sha256`, or `lfs`.

**What v3 added, and why the version moved.** Two optional fields on a repo entry:

- `refs` — the ref map the source advertised at copy time, sorted by name, each entry
  `{ "name", "commit" }`. Recorded only on a successful copy: a run that failed halfway has
  refs describing a repository nothing was written for, and trusting them would skip the retry.
  This is what lets `gitdr drill` check the second join — that the bundle declares the history
  the source actually had — rather than only that a bundle restores to its own header.
- `copiedAt` — when the artifacts this entry relies on were written. For a copy made this run
  it is when that repository's copy finished, once its last artifact was stored, so it is
  earlier than the run's `finishedAt` for every repository but the last one; for a repository
  skipped as unchanged it is **carried forward** from the run that made the copy. Without it
  each skip would reset the age of the copy, the refresh bound would never fire, and a
  repository that never changes would be skipped past its object lock's expiry and end up with
  nothing. Up to v0.1.20 this section said a copy's `copiedAt` was the run's finish time. The
  engine has always written the repository's own finish time, so the sentence was wrong and the
  field was not. *Corrected in v0.1.21.*

Both are `omitempty`, so a v2 manifest re-read and re-signed produces identical bytes. The
version moved anyway, because a consumer that needs `refs` has to be able to ask whether this
manifest can have them: absent is not the same as none, and a drill that treated a v2 manifest
as "the source advertised nothing" would report a pass it had not earned.

**v4 adds `destination.wormVerdict`**, one of exactly three values:

| value | what it means |
|---|---|
| `immutable` | the store answered, and what it described is enforced |
| `not-immutable` | the store answered, and said it locks nothing. An **earned** negative |
| `unknown` | the store refused the question. A statement about gitdr's visibility, not about the bucket |

Before it, the engine observed three distinguishable things and shipped two bits plus a
sentence, so a store that could not answer was recorded as one that had answered no. That is a
definite negative claim gitdr had not earned, and the only way a consumer could recover the
third state was to match on prose. Google's S3 surface is the case that made it visible: it
implements the lock call but reports Object Retention Lock, so a bucket protected by a locked
Bucket Lock policy answers exactly like an open one. The rule is about the protocol rather than
the provider — a store that answers has earned its negative, a store that refuses has told us
nothing — so `NotImplemented`, a 501, a 405 and `AccessDenied` all land in `unknown`.

`wormImmutable` keeps its exact v3 meaning: confirmed immutable, where `false` covers both of
the other two verdicts. It was added to rather than repurposed, because a nullable boolean would
change the meaning of a field every pinned consumer already reads, in the direction where absent
reads as falsy.

`--require-worm` passes only on `immutable`. This is not a new rule: invariant 4 already fires
the gate when immutability "cannot be confirmed", and `unknown` is the definition of that. On
the non-strict path the two negatives warn differently, because they send an operator to two
different places — `not-immutable` is local and says turn object lock on, `unknown` says ask the
provider. Both stay at WARN; `unknown` is not the quieter problem.

**On Azure the value changed in v0.1.19, not the schema, and in both directions.** Up to v0.1.18
the verdict came from the version-level immutability flag alone. `immutable` now needs a policy
that Resource Manager reports as locked (§4, Azure).

- A container with the flag read `immutable` whether or not any policy was locked. It now reads
  `unknown`, or `not-immutable` when its policy is unlocked. `--require-worm` runs that passed
  there now fail.
- A container under a locked container-level policy, which is the usual Azure setup, read
  `not-immutable`. It now reads `unknown` without `subscriptionID` and `resourceGroup`, and
  `immutable` with them. `--require-worm` runs that failed there now pass once both are set.

Neither older answer was earned, so the same container can carry one value in an older manifest
and another in a newer one, and the newer one is right. A reader can tell which is which without
trusting either: the signed manifest's `tool.version` names the engine that wrote it (its first
word is the version) and `destination.type` says `azure`. An Azure `wormVerdict` from an engine
before 0.1.19 is best read as `unknown`, whatever value it carries.

**v5 adds `destination.retentionObserved`**, one of exactly three values:

| value | what it means |
|---|---|
| `present` | the store returned a retention for each object the run asked about, one per write path it used |
| `absent` | the store implements the question and said one of those objects holds nothing. An **earned** negative |
| `not-checked` | nobody asked, or the store refused for at least one of them. A statement about gitdr's visibility |

It is the same failure as v4, one level down. `wormVerdict` is about the bucket's
*configuration*; this is about an object gitdr actually wrote, and the two can disagree: a store
can report Object Lock enabled, accept the write, and apply nothing. Nothing in v4 could tell
that apart, because `artifacts[].retainUntil` on the S3 path is the retention that was *asked
for* — `PutObject` returns no object-lock headers, so there was never anything to observe there,
while on GCS the same field is the expiry the write returned. One field, two meanings, both
signed.

**Checked once per write path, and only where the preflight said `immutable`.** The check is a
strong falsifier and a weak confirmer, and the design leans on that: a store applies object lock
in its write path, so one that drops the header for one object drops it for every object written
the same way and a negative generalises from one sample of that path — while a positive proves
only that this object is retained. Per-object checks would be thousands of extra requests buying
detection of an anomaly the protocol does not produce.

On S3 from v0.1.22 an object goes in one of two ways: up to the multipart threshold in one
`PutObject`, and past it in parts, with the lock headers on `CreateMultipartUpload` (§4). A store
can honour them on one call and not the other, so a run asks, on every backend, about the
smallest object it wrote, which went in one PUT unless every object went in parts, and the
largest, which went in parts if any object did. `absent` on either is `absent`; `present` needs
both; anything else is `not-checked`. *Changed in v0.1.22: a run asked about the first object it
wrote, a single PUT whenever the first repository was small, so a store that dropped the lock on
every upload in parts read `present`, and `--require-worm` passed it.*

**It only ever lowers a claim.** `absent` sets `wormVerdict` to `not-immutable` and
`wormImmutable` to `false`; `present` adds nothing, no badge and no upgrade. There is
deliberately **no fourth `wormVerdict` value** — every pinned consumer switches on three and a
fourth would fall through.

The read is `GetObjectRetention`, not `HeadObject`, and the difference decides whether the check
is usable. HeadObject returns the lock headers *only* to a caller holding
`s3:GetObjectRetention`; without it the response is a 200 with the headers omitted, which is
byte-identical to an object carrying no retention. §5 tells every operator to scope destination
credentials create/put-only, so a HeadObject-based check would report an unearned negative about
correctly configured, genuinely protected buckets belonging to the operators who followed that
advice.

Stores say "this object holds nothing" in their own words, and each reads `absent`: AWS and MinIO
answer `NoSuchObjectLockConfiguration` (a 404 and a 400), and Backblaze B2 answers
`ObjectLockConfigurationNotFoundError` (a 404). That is the code AWS gives the bucket's lock
question, and there it keeps its own meaning. *Changed in v0.1.22: B2's answer read
`not-checked`, so a B2 object holding no lock passed `--require-worm`.*

`--require-worm` fails on `absent` and passes on `not-checked`, for that reason: an earned
negative is the case the flag exists for, and silence is not one. Note what it cannot do — the
objects are written before the check runs and the destination is create-only, so failing here
means refusing to *report* a protection that is not there, never preventing the write.

Absent from a manifest means the engine was too old to say, and is **not** `not-checked`. Same
rule, and same trap, as `wormVerdict`.

The field is `omitempty` and is never written empty, so a v2 or v3 manifest re-read and
re-signed still produces identical bytes. **Absent must not be read as `unknown`**: absent means
the engine was too old to say, which is a different answer from the engine saying it cannot
tell, and only the version number separates them.

**Off S3 the destination block says where the copies went, from v0.1.21 (2026-10-03).** Values
changed and fields did not, so `gitdr.manifest/v5` is unchanged: each field means what v5 says,
and off S3 the values were wrong. Up to v0.1.20 the block was S3's wherever a run wrote.

- `destination.bucket` names the GCS bucket or the Azure container. It was empty off S3.
- `destination.wormMode` is absent off S3. It is the mode gitdr sets on each object, and gitdr
  sets one on S3 alone: on GCS and Azure the bucket's or the container's own policy locks every
  copy. A locked GCS bucket signed the configured `COMPLIANCE`, a mode Google was never sent.
- `artifacts[].size` on Azure is the size of the stored blob. It was 0.
- `destination.wormDetails` on GCS reads `bucket retention policy Locked, 30 days`, with the
  period the bucket reported, where it read `bucket retention 720h0m0s, locked=true`. Like every
  `wormDetails` it is a sentence for people, not a value to parse.

On a GCS or Azure manifest written before 0.1.21, read `wormMode` as absent whatever it carries,
and an empty `bucket` or an Azure `size` of 0 as not recorded. The first word of `tool.version`
names the engine, as it does for Azure's verdicts before 0.1.19.

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

A section that was read and held nothing is `null`. On GitHub, a section endpoint that answers
404 or 410 is a feature turned off for the repository: with pull requests off, GitHub answers 404
for them, and with issues off as well, 404 for the issue comments. Such a section is `null` too,
and the optional `unavailable` object names it, with GitHub's status and message, for example
`"unavailable": {"pullRequests": "404 Not Found"}`. The object is absent when every section was
read, so a v1 reader that ignores it sees the document it always did. Up to v0.1.20 such a
repository failed on every run. Any other refusal, a permission the installation lacks among them,
still fails the repository. *Added in v0.1.21. `gitdr.meta/v1` is unchanged.*

Wikis are a separate git repository and are out of scope for the metadata dump.

### `--output json` (stdout, structured logs go to stderr)

| Command | Shape |
|---|---|
| `backup`  | the run-manifest above, plus `manifestKey` |
| `drill`   | the drill report (`gitdr.drill/v1`), plus `reportKey` and `reportNotWritten` |
| `restore` | `{ "bundleKey", "sha256", "outDir", "verified" }` |
| `verify`  | `{ "manifestKey", "signatureValid", "artifactsChecked", "artifactsOk", "failures": [...] }` |
| `verify -drill` | `{ "drillKey", "signatureValid", "schema", "drillId", "manifestKey", "manifestSigned", "status", "eligible", "drilled", "failures": [...] }` |
| `doctor`  | `gitdr.doctor/v1`: `{ "schema", "ok", "checks": [ { "name", "ok", "detail" } ] }`, the `worm` check with `verdict`, `mode` and `code`, the `retention` check with `observed` (below) |

`verify -drill <key>` checks a drill report's signature and reports what the document claims.
It reads nothing back out of the bucket, which is why it has no artifact count: reusing
`artifactsChecked` to mean "repositories the report mentions" would put two different
measurements behind one name. It does not re-run the drill — that is a different and far more
expensive question, and conflating them would make the cheap check unavailable.

The two forms refuse each other's documents. A drill report unmarshals into a manifest as
happily as the reverse, so without that refusal each would report a valid signature over zero
artifacts and exit zero on a document it cannot read.

*Added 2026-09. `gitdr.manifest/v4` and `gitdr.drill/v1` are both unchanged, and the existing
`verify` output shape is untouched: this is a second shape under a new flag, not a change to
the first.*

### Events on stderr (`backup`)

`backup` writes three events among its log lines on stderr, so a caller can follow a run without
waiting for it to end. Their `msg` strings and fields are part of this contract. They are written
at level INFO, so a caller reads them with the log level at `info` or below, and in the default
JSON log format, one object per line. The other log lines are not part of the contract.

| `msg` | when | fields |
|---|---|---|
| `repos selected` | once, before the first repository | `count`: how many repositories the run will report on |
| `repo finished` | once per repository, after its last write | `slug`, `status`, `reason`, `error`, `artifacts` `[{ "kind", "key", "size" }]`, `copiedAt` |
| `manifest written` | once, after the manifest is stored | `key`, `status` |

A `repo finished` line says what the manifest then records for the repository, field for field:
`reason`, `error` and `copiedAt` appear only when the entry has them, `copiedAt` in the
manifest's format, and `artifacts` is `[]` when nothing was written. Lines come in the order
repositories finish, not the manifest's order. Every repository the run selected gets one, a run
that was stopped included: those it stopped or never started are `failed` with an error that
starts `stopped before it finished`. A caller can count the lines against `count`.

The lines are provisional until `manifest written`. The signed manifest is the record, and a
line for a run whose manifest was never stored records a copy nothing vouches for. For example:

```json
{"time":"2026-06-13T12:00:04.121Z","level":"INFO","msg":"repo finished","slug":"octo/empty","status":"skipped","reason":"repository has no commits","artifacts":[{"kind":"meta","key":"github.com/octo/empty/2026-06-13/empty.meta.json","size":41}]}
```

`internal/pipeline/events_test.go` pins the shape. *Added in v0.1.21, with `manifest written`
documented as the contract string it already was. The manifest and `--output json` are
unchanged.*

Exit codes are fail-closed. Any non-zero code means the run is not to be trusted; the specific
value narrows why.

| Code | Meaning |
|---|---|
| 0 | the command did what it was asked |
| 1 | the work failed |
| 2 | the command line was wrong; nothing ran |
| 3 | the work succeeded and a durable record of it could not be written |

Exit 3 exists because `drill` has two separable outcomes. A drill that restores every eligible
repository and then cannot store its signed report has proved the backups restore and has not
filed the proof. Reporting that as exit 1 tells an operator that a repository did not come back,
which gitdr did not observe and has not earned the right to say. Exit 3 is issued only when the
drill itself is clean; any repository failure is exit 1, whether or not the report was stored.

`backup` never emits exit 3. Its manifest is not a record of the work, it is part of it:
artifacts with no stored manifest cannot be verified, cannot be restored with signature
checking, and cannot be drilled. A backup that did not write its manifest failed, and exit 1 is
the true answer.

*Added after `gitdr.manifest/v2` and `gitdr.drill/v1`; both schemas are unchanged. Additive for
any consumer testing `!= 0`, which is every known one.*

`backup`'s `manifestKey` is the object key the manifest was stored under, and it is the value
`verify -manifest` expects. It is an addition to the *output*, not to the manifest: the
manifest is signed, and a document that named its own location would need re-signing every
time it was copied. The field sits alongside the manifest's own fields, which keep their
place, so a reader written before this change is unaffected.

Because of that split, **stdout is not the signed document**. The signature covers the
canonical bytes in the destination, which is what `verify` fetches. Never check a signature
against stdout.

*Added after `gitdr.manifest/v2`; the manifest schema is unchanged. Before it existed there
was no supported way to learn the key, and consumers were reading it out of the `manifest
written` log line.*

### The doctor document (`gitdr.doctor/v1`)

`gitdr doctor --output json` prints one document. It is not signed and not stored. It says what a
destination answered at one moment, which makes it a diagnosis and not evidence about any backup.
It carries a schema anyway, because a reader has to tell a document that can hold a verdict from
one printed by an older engine, which has the same `ok` and `checks` and no verdict at all. A
field that is absent is not the same as one that is null.

```json
{
  "schema": "gitdr.doctor/v1",
  "ok": true,
  "checks": [
    { "name": "config", "ok": true, "detail": "valid" },
    { "name": "worm", "ok": true, "detail": "immutable, Object Lock enabled; default retention COMPLIANCE",
      "verdict": "immutable", "mode": "COMPLIANCE", "code": null },
    { "name": "retention", "ok": true, "detail": "an object here is held until 2026-11-02T12:00:00Z",
      "observed": "present" }
  ]
}
```

`ok` and the exit code mean what they meant before v1. `ok` is false, and the exit code 1, when a
check failed, and a check fails only on what would fail a backup. So a bucket that locks nothing
passes unless `worm.require` is set. **To learn what a bucket locks, read `verdict`, never `ok`.**

A reader finds a check by its `name`: `git`, `git-lfs`, `config`, `encryption key` when encryption
is on, `source` or `source auth`, `destination` when the destination could not be set up, `worm`
and `retention`. A config that fails validation ends the document at `config`, and a destination
that cannot be set up ends it at `destination`. Either way there is no `worm` check, and so no
verdict to read.

Every check has `name`, `ok` and `detail`. `detail` is a sentence gitdr wrote, for a person, and
nothing should parse it. The `worm` check always has three fields more, each `null` when there is
nothing to say.

| field | values |
|---|---|
| `verdict` | `immutable`, `not-immutable` or `unknown`, meaning what `destination.wormVerdict` means in the manifest (v4 above). `null` when the check ended in an error instead of an answer, and then `code` says which |
| `mode` | `COMPLIANCE` or `GOVERNANCE` for an S3 bucket's default retention, `RETENTION` for a Cloud Storage retention policy, `IMMUTABILITY` for an Azure container policy. `null` when there is none, or the store named a mode gitdr does not know |
| `code` | `null` when the store answered the question. Otherwise the store's own error code (S3's `Code`, Azure's error code, the reason in a Cloud Storage error) if it matches `^[A-Za-z][A-Za-z0-9.]{0,63}$`, `unnamed` if the store's code does not, or one of the six below for a failure that carries no code |

| code | the failure |
|---|---|
| `dns` | the endpoint's host name did not resolve |
| `connect` | no connection, or it broke before the answer was complete |
| `tls` | the TLS handshake or the certificate was refused, or an `https` endpoint answered in plain HTTP |
| `timeout` | no answer in time, or the run was stopped before one came |
| `too-large` | an answer ran past 1 MiB, the most doctor reads of one |
| `not-s3` | an answer that is not the storage API's, such as a web page, an empty body, another call's document or a body that does not parse, or a failure none of the other codes names |

`unknown` with a code is a store that declined the question. That covers `AccessDenied` and
`NotImplemented`, and codes that say the key, the bucket name or the region is wrong, such as
`InvalidAccessKeyId`, `SignatureDoesNotMatch`, `NoSuchBucket`, `PermanentRedirect` and
`AuthorizationHeaderMalformed`. `not-immutable` comes without a code, since the store answered.
A bucket that does not exist reads `unknown` with `NoSuchBucket` even on a store that answers its
lock question as if it were there, since the code comes from the listing that tells the two
apart (§4). *Changed 2026-10-03.*

The `retention` check is there when the `worm` check says `immutable` and the destination can be
asked. It has `observed`.

| value | what it means |
|---|---|
| `present` | the store returned a retention for an object already under the destination |
| `absent` | the store said that object holds none. The earned negative of v5 above, and a failed check under `worm.require` |
| `unreadable` | the listing or the read failed, or the store would not say. On S3 the read needs `s3:GetObjectRetention`, which a create-only key does not have |
| `none` | the first page of the listing held no object, so there was nothing to look at |

What doctor guarantees about its reads and writes:

- **No text the store wrote reaches stdout**, in either output, except a `code` of the shape above.
  That shape can name a condition and carry nothing else. The store's own error goes to stderr,
  with the log.
- **Reads are bounded.** The object comes from one page of the listing, one key long, and doctor
  never asks for a second page. On S3 each answer is capped at 1 MiB, error answers included, and
  past the cap the check ends with `too-large`. Cloud Storage and Azure answers are not capped
  yet, and the Cloud Storage library asks again past a page that came back empty with a
  continuation, which gitdr cannot stop.
- **Nothing is written.** No probe object and no canary. On a compliance-locked bucket either would
  be undeletable for the whole retention window.

`gitdr doctor -only destination` runs the destination's checks and nothing else, which means
`config` for the destination block, `worm` and `retention`. It needs no source, no git and no
git-lfs, so a bucket can be checked before anything is connected to back up into it. `-only`
takes no other value, and any other exits 2.

doctor stops its checks after 45 seconds, or after `-timeout`, a duration such as `20s`. `0`
waits for the checks, and a negative value exits 2. A check that runs out of time ends with the
code `timeout`, and the document still goes out. A caller that stops doctor itself gets no
document at all, so the default is under the 60 seconds such a caller is likely to give it.

Under `--output json`, a config that cannot be read or parsed still gets a document. It has one
check, `config`, with `ok: false` and `code: "config"`, and that is the only time the `config`
check carries a `code`. doctor still exits 1, and the error goes to stderr and nowhere else,
since a parse error can quote the file. Text output is as it was.

*Added 2026-10-03: the deadline and the `config` code. Both add to `gitdr.doctor/v1` and change
nothing already in it.*

Before v1 the `worm` check said what it found only in `detail`, so a reader had to match prose.
The change is additive: every key an older reader used is where it was, with the value it had.
The one exception is `detail` where it quoted the store's error, which it now names by its code.
A store's code in `destination.wormDetails` is shaped the same way, so there a code that does not
match reads `unnamed` too. The codes S3 documents are single words such as `AccessDenied` and
`ObjectLockConfigurationNotFoundError`, which match, so only a code a store made up reads
differently.

*Added 2026-10-03. `gitdr.manifest/v5` and `gitdr.drill/v1` are unchanged.*
