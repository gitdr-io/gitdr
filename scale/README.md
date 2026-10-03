# Scale harness

This backs up many and large repositories through the engine and checks what it did. It is for the
defects that only show at scale: thousands of repositories, a hundred thousand refs, objects past
5 GiB, a run stopped halfway.

It is not part of `make ci`. Every file here carries the `scale` build tag, so `go build`,
`go test ./...` and the linters skip it.

## Running it

You need Docker with Compose v2, git, git-lfs and Go.

```
make scale SCALE_REPOS=50                    # the quick profile
make scale                                   # 2,500 repositories
SCALE_BIG=1 make scale SCALE_RUN=TestScale4  # the 6 GiB pack and the 8 GiB of LFS
make scale SCALE_RUN=TestScale3              # any one scenario
make scale-down                              # removes a stack that a killed run left behind
```

On a 14-core laptop the quick profile took under three minutes. 2,500 repositories took 30
minutes, most of it in scenarios 1 and 8, which copy the organisation several times over, and
scenario 4 took four. Without `SCALE_BIG` a few GiB of free disk is plenty. The big fixture adds
14 GiB, and the engine's scratch space peaked at 16.5 GiB copying it.

The summary at the end lists every check. A failed check names the known defect it shows. One that
says it is not a known finding is news, so start there. The measures go to
`scale-results/<timestamp>.json`, with a log of every engine run in `scale-results/<timestamp>/`.

## What runs

- MinIO with Object Lock, over TLS. The CA, the certificates and the credentials are made for each
  run in a temp dir and are gone when it ends.
- `s3limits/`, a proxy in front of MinIO that refuses what AWS refuses: a PUT over 5 GiB, a part
  number past 10,000, a part under 5 MiB that is not the last one, and a locked write with no
  checksum. It answers `If-None-Match` the way AWS does, or with a 501 the way Backblaze does when
  set to `b2`. It throttles to 8 MB/s and counts requests by operation. Through `/_s3limits/` a
  test can make it answer with an error, drop the response after a write has landed, or hold a
  request until it is released.
- The fake forge (`forge_test.go`), inside the test process. It mints GitHub App installation
  tokens and serves an installation's repositories in pages of 100, then each repository's
  metadata. Every API request spends from a rate-limit budget and carries GitHub's headers. Git
  goes through `git http-backend`, and LFS through a batch endpoint whose download links expire.

MinIO refuses an aws-chunked chunk over 16 MiB, and AWS takes any size. Over TLS the AWS SDK sends
a body of known length as a single chunk, so s3limits splits it and signs the request again with
MinIO's key. Scenario 0 turns that off to show what MinIO does on its own.

Scenarios that cross a simulated day call `pipeline.Backup` in the test process with an injected
clock. The binary has no flag that moves its date, and must not get one. Scenarios that stop the
engine or measure its memory run the `gitdr` binary built from this tree.

## Fixtures

They all come from `git fast-import` with fixed seeds and dates, so a profile gives the same
repositories on every run.

- `scale-small`: `SCALE_REPOS` small repositories, every hundredth with 200 branches and 200 tags,
  plus one with issues and no commits and one with LFS objects, one of them reachable only from a
  pull request. 30% of the small ones change on each simulated day.
- `scale-refs`: five repositories with 100,000 refs each, half of them `refs/pull/*`.
- `scale-kill`: twelve repositories of 256 KiB.
- `scale-rate`: sixteen small repositories under a budget of 40 requests every 10 seconds. It is
  the only installation with a budget a run can spend, so each scenario measures one thing. The
  others record the requests they make: day 1 of 2,500 repositories makes about 20,000, where
  GitHub gives an installation 5,000 to 12,500 an hour.
- `scale-big`, with `SCALE_BIG=1`: a 6 GiB pack of random data, and 8 GiB of LFS objects, an
  eighth of them only on a pull request.

## Scenarios

Most of them fail on today's engine. Each failure names the defect it shows, and the fix for that
defect turns the check green.

| # | What it checks | Today |
|---|---|---|
| 0 | A 20 MiB object over TLS, written the way the engine writes one: through the proxy, then straight to MinIO | fails: `minio-tls-chunk` |
| 1 | Day 1, a rerun the same day, day 2 and day 3: no full copy of an unchanged repository | fails: `empty-repo-rerun`, `resume-trusts-objects` |
| 2 | SIGTERM and SIGKILL mid-clone, mid-upload and after a bundle landed, then a rerun: no repository counted without a recorded copy | fails: `stopped-run-no-manifest`, `stop-waits-for-git`, `resume-trusts-objects` |
| 3 | 100,000-ref repositories: the manifest's size, and no copies on an unchanged next day | fails: `manifest-read-cap` |
| 4 | With `SCALE_BIG=1`, a 6 GiB pack and 8 GiB of LFS through AWS's 5 GiB limit on a single PUT | fails: `single-put-limit` |
| 6 | A rate limit spent in the middle of a run is waited out | fails: `rate-limit-not-waited` |
| 8 | An organisation run, a single-repository run, an organisation run: the third copies nothing | fails: `newest-manifest-only` |

Day 3 of scenario 1 is a control. It follows a day with no rerun and passes today, so the harness
can tell a skip from a copy.

The known defects:

- `resume-trusts-objects`: a same-day rerun counts any repository with a bundle under today's date
  as backed up, whether or not a manifest records the copy. Its skip records no refs either, so the
  next day copies the repository in full again. In `internal/pipeline/backup.go`.
- `empty-repo-rerun`: a repository with no commits fails its same-day rerun, because its
  `meta.json` already exists.
- `stopped-run-no-manifest`: a backup stopped by SIGTERM files no manifest, so nothing records the
  copies it finished.
- `stop-waits-for-git`: after SIGTERM the engine waits on git's HTTP helper, which keeps git's
  stderr open, and nothing bounds the wait.
- `manifest-read-cap`: a manifest over 32 MiB is not read back, so the next run copies everything.
  In `internal/pipeline/previous.go`.
- `newest-manifest-only`: the next run reads only the newest manifest, so one single-repository run
  makes the next organisation run copy everything.
- `single-put-limit`: every artifact is one PutObject, which AWS refuses over 5 GiB.
- `rate-limit-not-waited`: a GitHub rate limit fails the repositories that meet it.
- `minio-tls-chunk`: over TLS no artifact or manifest over 16 MiB can be written to MinIO, because
  of the single chunk described above.

## Measures

For each engine run:

- wall time, and the simulated time it ran at;
- repositories copied, skipped and failed, the ones copied again although nothing had changed, and
  the bytes sent twice;
- the manifest's key and size;
- the peak of the engine's scratch space;
- S3 requests and bytes by operation, the proxy's refusals, and open multipart uploads;
- API, git and LFS requests by endpoint, and how often the rate-limit budget ran out;
- for a run of the binary, the peak resident set of the engine or of any git process it waited
  for.

The summary compares wall times with the previous report.

## Settings

These are environment variables, and all of them are optional.

| Variable | Default | |
|---|---|---|
| `SCALE_REPOS` | 2500 | small repositories |
| `SCALE_CHANGE_PERCENT` | 30 | small repositories that change each simulated day |
| `SCALE_REFS_REPOS`, `SCALE_REFS_PER_REPO` | 5, 100000 | the refs fixture |
| `SCALE_KILL_REPOS`, `SCALE_RATE_REPOS` | 12, 16 | the kill and rate-limit fixtures |
| `SCALE_RATE_LIMIT`, `SCALE_RATE_WINDOW` | 40, 10s | the rate-limit budget |
| `SCALE_BIG`, `SCALE_BIG_PACK_BYTES`, `SCALE_BIG_LFS_BYTES` | off, 6 GiB, 8 GiB | the big fixture |
| `SCALE_S3_RATE` | 8000000 | the proxy's throttle in bytes a second, 0 for none |
| `SCALE_CONCURRENCY` | 4 | the engine's `backup.concurrency` |
| `SCALE_KEEP` | off | 1 leaves the stack and the fixtures in place |
| `SCALE_RESULTS_DIR` | `scale-results/` | where the report and the logs go |

The proxy and the fake forge check themselves without Docker, with
`go test -tags scale ./scale/s3limits` and `go test -tags scale -run Forge ./scale`.

## Not built yet

- `make scale-image`, the second phase: the released image under a 4 GiB memory cgroup with its
  `memory.peak` and `oom_kill`, a scratch budget below the free disk, and stops at named log
  events.
- Scenario 5, an LFS fetch that outlives its token, and scenario 7, two writers on a store without
  conditional writes. Scenario 9 is not part of this repository.
- The rest of the kill matrix: mid-LFS, mid-part, before the receipt and during the manifest.
- Verify and drill times, and `ListMultipartUploads` after a kill.
