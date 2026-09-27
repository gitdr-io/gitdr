# Restore Runbook

How to get your repos back from a gitdr backup, one repo or a whole org gone dark.
Practice this before you need it.

## Before you start

You need:

- The gitdr binary or container, same major version that wrote the backup.
- Read access to the destination bucket. A read-only credential is enough, restore never
  writes to the destination.
- A config file pointing at that destination (`--config`), same as for backup.
- For `verify`, and for `restore --manifest`, the manifest public key
  (`manifest.publicKeyPath` in config).
- If backups were encrypted, the encryption key in `GITDR_ENCRYPTION_KEY`. `verify`
  doesn't need it, checksums cover the stored ciphertext.

## Object layout (what's in the bucket)

```
{host}/{org}/{repo}/{YYYY-MM-DD}/{repo}.bundle         git data (full mirror)
{host}/{org}/{repo}/{YYYY-MM-DD}/{repo}.sha256         checksum of the bundle
{host}/{org}/{repo}/{YYYY-MM-DD}/{repo}.meta.json      metadata (audit/reference)
{host}/{org}/{repo}/{YYYY-MM-DD}/{repo}.lfs.tar        LFS objects (if any)
{host}/{namespace}/manifests/{timestamp}.manifest.json signed run-manifest (plus .sig)
```

So a backup of `github.com/acme/api` from 2026-06-01 lives under
`github.com/acme/api/2026-06-01/`. On GitLab `{org}` can be a group path, so the project `api`
in the subgroup `acme/platform` lives under `gitlab.com/acme/platform/api/2026-06-01/`.

A run files one manifest, under the deepest namespace that holds every repository it covered.
On GitHub that is the organisation. On GitLab it is the group above every subgroup the run
covered, or `{host}/manifests/` when the run covered namespaces that share nothing, a group and
a user's projects for instance. The manifest is named for the moment the run finished, in UTC:
`20260601T020000Z.manifest.json`.

## 1. Find the run

`backup` prints the key of the manifest it wrote: `manifestKey` in `--output json`, and the
`manifest:` line of its text output. If you kept it, you have the run. Otherwise list the
manifests and pick the run you want (use your cloud's CLI, like `aws s3 ls`,
`gcloud storage ls`, `az storage blob list`):

```sh
aws s3 ls s3://my-worm-bucket/github.com/acme/manifests/
```

## 2. Verify before you trust it

```sh
gitdr verify --config config.yaml \
  --manifest github.com/acme/manifests/20260601T020000Z.manifest.json
```

This checks the ed25519 signature and re-downloads every artifact to confirm its SHA-256.
Exit code is non-zero on any signature or checksum mismatch. A clean `signature valid:
true, artifacts N/N ok` means the run is intact.

## 3. Restore a repository

With a public key configured, name the run's manifest and the repository in it:

```sh
gitdr restore --config config.yaml \
  --manifest github.com/acme/manifests/20260601T020000Z.manifest.json \
  --repo acme/api \
  --out ./restore/api
```

The manifest has to verify before gitdr believes any of it, and every artifact is then checked
against it. The host and date come from the manifest, so `--host` and `--date` are refused next
to `--manifest`. `--repo` is the repository's slug as the manifest records it, and it splits at
the last slash: `--repo acme/platform/api` is the project `api` in the GitLab group
`acme/platform`. This is also the form for a repository a drill passed: the drill report names
the manifest it drilled, and the restore of that manifest is the one the drill ran.

Without `--manifest`, give the host and the date of the copy:

```sh
gitdr restore --config config.yaml \
  --host github.com \
  --repo acme/api \
  --date 2026-06-01 \
  --out ./restore/api
```

With a public key, gitdr then looks for the manifest of the run that wrote that copy, in the
repository's namespace, each namespace above it and then `{host}/manifests/`, among runs that
finished on that date or the next day. That covers every run from v0.1.20 on that finished within
a day of the copy. A manifest that gitdr v0.1.19 or older filed under another namespace, or a longer run's,
is reached only with `--manifest`.

Either way gitdr downloads the bundle, re-checks its checksum, runs `git bundle verify`, clones
it into `--out`, and then compares the refs of what it just cloned against the ref-to-commit map
the bundle itself declares. If encryption was used, set `GITDR_ENCRYPTION_KEY` first.

That last check is the one to keep. It prints a line like:

```
refs: 8 of 8 declared by the bundle present at the same commit
```

Because git is content-addressed, a matching commit id covers that commit's tree, its files
and its entire ancestry. So this line is not a spot check, it is the statement that the
restored history is the backed-up history. A ref that is missing or points somewhere else
fails the command with a non-zero exit and names the first one that differs, so a restore that
did not reproduce the repository cannot be mistaken for one that did.

If the counts differ, gitdr says which refs it could not account for. Refs outside
`refs/heads/*` and tags — `refs/notes/*`, `refs/merge-requests/*`, `refs/keep-around/*` — are
reported separately and are not a failure: `git clone` does not create them, so their objects
are restored with nothing pointing at them. To get them back, fetch them from the bundle by
name.

Keep the output. SOC 2 A1.3.2, CIS v8.1 11.5, ISO 27001 A.8.13, CSA CCM BCR-08 and NIS2
implementing regulation (EU) 2024/2690 Annex 4.2.3 and 4.2.6 all want a tested restore with a
documented result, and this is that result without a screenshot.

## 4. Sanity-check the restored repo

gitdr has already compared every ref against the bundle. This is the independent look.

```sh
cd ./restore/api
git log --oneline -5
git fsck --full
git for-each-ref            # all branches and tags present?
```

### Check the LFS files are files

If the repository uses Git LFS, confirm the working tree holds real content and not pointers:

```sh
git lfs ls-files -n | while read -r f; do
  head -c 41 "$f" | grep -q '^version https://git-lfs' && echo "STILL A POINTER: $f"
done
```

Silence means every tracked file was materialised. A pointer file is about 130 bytes of text
where your data should be.

`gitdr restore` does this itself from v0.1.5 and fails the restore if anything is still a
pointer. **Before v0.1.5 it did not.** A clone from a bundle carries no LFS filter
configuration, and `git lfs checkout` exits 0 without doing anything when that configuration
is missing — so a restore onto a fresh machine, which is the usual machine in a disaster,
could report success over pointer files. If you restored with an earlier version, re-run the
check above against that copy. In that case the objects were backed up correctly; they are in
the run's `.lfs.tar` and nothing was lost from the bucket.

**Until v0.1.20 the check ran only when the backup had an `.lfs.tar`.** A backup taken with
`backup.lfs: false`, or where git-lfs was not installed, stores no LFS objects at all, and its
restore passed with pointers in place of those files. From v0.1.20 every restore reads the tree
back, and that case fails with `came back as git-lfs pointers` and says the content was not
backed up. The restored tree stays in `--out`. The files' content is not in the bucket; get it
from the source, if it still exists, and turn LFS on for the next backup.

## 5. Re-home to a new VCS

Point the restored repo at a fresh remote and push everything:

```sh
git remote add new https://new-host/acme/api.git
git push --mirror new
```

`--mirror` pushes all branches and tags. Re-create the org or project on the new host
first. gitdr restores git data, not org settings.

## Restoring a whole org

For a full DR event, drive the per-repo restore from the run-manifest. It lists every repository
the run covered. Pull a copy to read the slugs from, then restore each one the run copied with
`--manifest`, and do step 5 per repository:

```sh
key=github.com/acme/manifests/20260601T020000Z.manifest.json
aws s3 cp "s3://my-worm-bucket/$key" manifest.json
jq -r '.repos[] | select(.status == "success") | .slug' manifest.json |
  while read -r slug; do
    gitdr restore --config config.yaml --manifest "$key" --repo "$slug" --out "./restore/$slug"
  done
```

The local copy is only a list. Each `gitdr restore` fetches the manifest again and verifies it
before it trusts any of it.

A repository the run skipped has no copy in that run. Its `reason` says why, and
`unchanged since <date>` says when the copy it relies on was made. Restore that copy with the
manifest of the run that made it. Restore is independent per repo, so it parallelizes safely.

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
| `restore` fails with `no signed manifest records ...` | Without `--manifest`, restore looks for the run's manifest in the repository's namespace, each namespace above it and `{host}/manifests/`, among runs that finished on `--date` or the next day, and the error lists where it looked. A manifest gitdr v0.1.19 or older filed under another namespace, or one from a run that finished more than a day after the copy, is not found that way. Pass `--manifest <key>` with the key backup printed, or find it as in step 1. If warnings above the error say manifests did not verify, point `manifest.publicKeyPath` at the key that signed this backup. |
