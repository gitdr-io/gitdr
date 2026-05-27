# Security Policy

gitdr keeps immutable copies of people's source history, so its own security matters.
Reports are very welcome.

## Reporting a vulnerability

Please don't open a public issue for a security problem.

Report it privately, either way works.

- Email security@gitdr.io. Encrypt it with the key below.
- GitHub, the repo's "Report a vulnerability" button (a private security advisory).

PGP key: [`security-pgp-key.asc`](./security-pgp-key.asc)
Fingerprint: `FE90 9F5A 371E 83DB EAC9  1C3D C860 E582 7266 289A`

Include enough to reproduce it. Affected version (`gitdr version`), config with secrets
redacted, and the impact you saw. If you have a fix in mind, mention it. Never send real
credentials, tokens, or customer data.

I aim to acknowledge within 3 business days and agree a disclosure timeline with you.
Coordinated disclosure, and I'll credit you if you want it.

## Supported versions

Pre-1.0, only the latest release gets security fixes. Pin by digest and update promptly.
Once 1.0 ships, this section will define a support window.

## In scope

- The gitdr binary, container image, and the Helm chart in this repo.
- The WORM check, credential handling, manifest signing, and the optional client-side
  encryption. Anything that could let a backup be silently skipped, forged, weakened, or
  exfiltrated.

Out of scope: misconfiguration on your side (pointing gitdr at a non-WORM bucket without
`--require-worm`, giving the destination credential delete rights, leaving a bucket
unlocked), and the security of the upstream VCS or cloud providers.

## Design stance

gitdr fails closed and tries to make a compromise worth as little as possible. Read-only
on the source, create-only on the destination (no delete or overwrite method exists in the
code), no long-lived secrets in the image, secrets never logged, no telemetry. See
[`THREAT-MODEL.md`](./THREAT-MODEL.md) for the full analysis.
