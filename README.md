# gitdr

[![CI](https://github.com/gitdr-io/gitdr/actions/workflows/ci.yml/badge.svg)](https://github.com/gitdr-io/gitdr/actions/workflows/ci.yml)
[![CodeQL](https://github.com/gitdr-io/gitdr/actions/workflows/codeql.yml/badge.svg)](https://github.com/gitdr-io/gitdr/actions/workflows/codeql.yml)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/gitdr-io/gitdr/badge)](https://scorecard.dev/viewer/?uri=github.com/gitdr-io/gitdr)
[![SLSA 3](https://slsa.dev/images/gh-badge-level3.svg)](https://slsa.dev)
[![License](https://img.shields.io/github/license/gitdr-io/gitdr)](./LICENSE)

**Back up your whole git org to storage nobody can delete.**

Everyone lives in git. Almost nobody backs it up. gitdr copies your entire GitHub or GitLab org into object storage you control, and it checks the bucket is write-once before it writes a thing. Deleted repo, popped account, ransomware, and your history is still sitting somewhere they can't reach.

> Pre-1.0, so interfaces can still shift. The core (backup, restore, verify) is solid and runs against real S3, GCS, Azure, Backblaze B2, and Cloudflare R2. Star it to follow along.

## Why

Your repos are the control plane now. Infra, deploys, runbooks, all of it lives in git. But your host runs on shared responsibility. They keep the lights on. If your data goes because someone deleted it, took over an account, or dropped ransomware, that part is on you.

gitdr writes backups to a bucket you own and checks it's WORM (write-once-read-many) before writing, so you never get a false sense of safety. WORM is strongly recommended. Turning it on is your call, and `--require-worm` makes gitdr refuse anything softer if you want the hard guarantee.

## What it does

- Backs up every repo in an org. Full git mirror, LFS objects, and metadata (issues, PRs and MRs, comments, releases, labels, milestones) as JSON.
- Writes dated, immutable objects plus a signed manifest.
- Checks WORM before writing and warns loudly when the bucket isn't immutable, or stops cold with `--require-worm`.
- Restores and verifies with `gitdr restore` and `gitdr verify`.

> Metadata is for reference, not replay. Git history and LFS come back exactly. The issue and PR JSON can't be pushed back into a host with the original numbers, authors, and timestamps. No git backup tool can do that, so it isn't really a gitdr limit.

## Sources

- GitHub, both github.com and Enterprise Server.
- GitLab, both gitlab.com and self-managed.

## Where it stores

gitdr writes to any S3-compatible or major-cloud object storage. Immutability (WORM / Object Lock) is a switch you flip on the bucket. The table is a reference for which providers can do it, so you can pick one that clears a ransomware-resistant bar.

| Provider | Integration | WORM / Object Lock¹ | Notes |
|---|---|:---:|---|
| Amazon S3 | S3-compatible | ✅ | Reference Object Lock (SEC 17a-4 assessed) |
| Google Cloud Storage | Native API | ✅ | Bucket Lock + Object Retention |
| Azure Blob Storage | Native API | ✅ | Immutability policies |
| Oracle Cloud (OCI) | Native / S3-compat | ✅ | Retention Rules |
| IBM Cloud Object Storage | Native / S3-compat | ✅ | Immutable Object Storage |
| Alibaba Cloud OSS | Native / S3-compat | ✅ | Retention / WORM |
| Backblaze B2 | S3-compatible | ✅ | Enable at bucket creation |
| Wasabi | S3-compatible | ✅ | 90-day minimum retention |
| MinIO | S3-compatible | ✅ | Self-hosted |
| Ceph (RGW) | S3-compatible | ✅ | Self-hosted |
| IDrive e2 | S3-compatible | ✅ | |
| Tigris | S3-compatible | ✅ | Zero-egress + object lock |
| Impossible Cloud | S3-compatible | ✅ | EU |
| Scaleway | S3-compatible | ✅ | EU |
| Cloudian HyperStore | S3-compatible | ✅ | Enterprise / on-prem |
| NetApp StorageGRID | S3-compatible | ✅ | Enterprise / on-prem |
| Dell ECS | S3-compatible | ✅ | Enterprise / on-prem |
| Pure Storage FlashBlade | S3-compatible | ✅ | Enterprise / on-prem |
| Storj | S3-compatible | ⚠️ | Verify (object lock added recently) |
| OVHcloud | S3-compatible | ⚠️ | Verify current parity |
| Exoscale SOS | S3-compatible | ⚠️ | Ceph-backed; verify |
| Hetzner Object Storage | S3-compatible | 🔜 | Object Lock on roadmap |
| DigitalOcean Spaces | S3-compatible | ❌ | No object lock |
| Linode / Akamai | S3-compatible | ❌ | No object lock |
| Vultr | S3-compatible | ❌ | No object lock |
| Cloudflare R2 | S3-compatible | ❌ | No object lock or versioning |
| Fastly Object Storage | S3-compatible | ❌ | No object lock |
| Garage / SeaweedFS | S3-compatible | ⚠️ | Open-source; limited, verify |

¹ WORM / Object Lock is a feature you enable on your own bucket. gitdr writes to any
supported destination regardless. This column only says which providers can give you
immutability.

✅ supported · ⚠️ verify · 🔜 announced/roadmap · ❌ not available

---

*All product names, logos, and brands are property of their respective owners, used here
only for identification and interoperability. gitdr is independent and not affiliated
with, endorsed by, or sponsored by any listed provider.*

## How you run it

One static Linux binary, one-shot job. Run it however you already run jobs.

- Kubernetes CronJob or Job (Helm chart included)
- systemd timer or cron on a plain box
- docker run on a schedule
- straight from your existing CI

Linux only, amd64 and arm64. On a Mac, run the container. This is a tool for CI and servers, not laptops.

## Install

New here? Start with [`docs/QUICKSTART.md`](./docs/QUICKSTART.md). The artifacts:

- Container image `ghcr.io/gitdr-io/gitdr` (multi-arch)
- Static binaries on the [Releases](https://github.com/gitdr-io/gitdr/releases) page, `linux/amd64` and `linux/arm64`, with checksums and cosign signatures
- Helm chart `oci://ghcr.io/gitdr-io/charts/gitdr`

Everything ships signed with cosign (keyless) and carries an SBOM.

## Security

Built to be boring and auditable. Read-only on your VCS, create-only on storage (it has no way to delete or overwrite a backup), no long-lived secrets baked into the image, workload identity preferred over static keys, and no telemetry of any kind. The code is open, go read it.

See [`THREAT-MODEL.md`](./THREAT-MODEL.md) for the analysis, [`SECURITY.md`](./SECURITY.md) to report something, and [`SPEC.md`](./SPEC.md) for the design.

## License

AGPL-3.0. A commercial license is available if the AGPL doesn't fit, just ask.

---

*gitdr is a solo project. The code is short enough to read end to end, so audit it yourself. Found a bug or have an idea? Open an issue.*
