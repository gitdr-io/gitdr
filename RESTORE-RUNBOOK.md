# Restore Runbook

How to get your repos back from a gitdr backup, one repo or a whole org gone dark.
Practice this before you need it.

## Before you start

You need:

- The gitdr binary or container, same major version that wrote the backup.
- Read access to the destination bucket. A read-only credential is enough, restore never
  writes to the destination.
- A config file pointing at that destination (`--config`), same as for backup.
- For `verify`, the manifest public key (`manifest.publicKeyPath` in config).
- If backups were encrypted, the encryption key in `GITDR_ENCRYPTION_KEY`. `verify`
  doesn't need it, checksums cover the stored ciphertext.

## Object layout (what's in the bucket)

```
{host}/{org}/{repo}/{YYYY-MM-DD}/{repo}.bundle      git data (full mirror)
{host}/{org}/{repo}/{YYYY-MM-DD}/{repo}.sha256      checksum of the bundle
{host}/{org}/{repo}/{YYYY-MM-DD}/{repo}.meta.json   metadata (audit/reference)
{host}/{org}/{repo}/{YYYY-MM-DD}/{repo}.lfs.tar     LFS objects (if any)
{host}/{org}/manifests/{timestamp}.manifest.json    signed run-manifest (plus .sig)
```

So a backup of `github.com/acme/api` from 2026-06-01 lives under
`github.com/acme/api/2026-06-01/`.

## 1. Find the run

List the manifests and pick the run you want (use your cloud's CLI, like `aws s3 ls`,
`gcloud storage ls`, `az storage blob list`):

```sh
aws s3 ls s3://my-worm-bucket/github.com/acme/manifests/
```

## 2. Verify before you trust it

```sh
gitdr verify --config config.yaml \
  --manifest github.com/acme/manifests/2026-06-01T02:00:00Z.manifest.json
```

This checks the ed25519 signature and re-downloads every artifact to confirm its SHA-256.
Exit code is non-zero on any signature or checksum mismatch. A clean `signature valid:
true, artifacts N/N ok` means the run is intact.

## 3. Restore a repository

```sh
gitdr restore --config config.yaml \
  --host github.com \
  --repo acme/api \
  --date 2026-06-01 \
  --out ./restore/api
```

gitdr downloads the bundle, re-checks its checksum, runs `git bundle verify`, and clones
it into `--out`. If encryption was used, set `GITDR_ENCRYPTION_KEY` first.

## 4. Sanity-check the restored repo

```sh
cd ./restore/api
git log --oneline -5
git fsck --full
git for-each-ref            # all branches and tags present?
```

LFS objects (if any) are in the run's `.lfs.tar`. Unpack and `git lfs` them back per the
LFS docs for that repo.

## 5. Re-home to a new VCS

Point the restored repo at a fresh remote and push everything:

```sh
git remote add new https://new-host/acme/api.git
git push --mirror new
```

`--mirror` pushes all branches and tags. Re-create the org or project on the new host
first. gitdr restores git data, not org settings.

## Restoring a whole org

For a full DR event, drive the per-repo restore from the run-manifest. It lists every repo
and the date. Pull the manifest, iterate its `repos[]`, and run step 3 per repo, then step
5. Restore is independent per repo, so it parallelizes safely.

## What restores faithfully, and what doesn't

- Faithful: all git history, branches, tags, and LFS blobs. This is a true mirror.
- Audit and reference only: the `*.meta.json` (issues, PRs/MRs, comments, labels,
  milestones, releases). No tool can replay these into a VCS with the original numbers,
  authors, timestamps, or cross-references. Treat recovered metadata as a read-only record,
  not a re-import.

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `verify` reports a checksum failure | Object was altered or truncated at rest. Restore from an earlier good run, then investigate the bucket. |
| `restore` fails decrypting | Wrong or missing `GITDR_ENCRYPTION_KEY`. It must be the KEK used at backup time. |
| `git bundle verify` fails | Bundle is corrupt. Restore the same repo from a different run or date. |
| Access denied listing or getting | Restore credential lacks read on the bucket or prefix. |
| Can't find the date | Backups are dated by run. List the repo prefix to see available dates. |
