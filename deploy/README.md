# Running gitdr on a VM (systemd / cron)

Sample units for scheduling `gitdr backup` on a plain server. On Kubernetes use the
Helm chart in [`../charts/gitdr`](../charts/gitdr) instead. First walk through
[`../docs/QUICKSTART.md`](../docs/QUICKSTART.md) to get config + credentials in place.

## Common setup

```sh
# dedicated unprivileged user, no login, no home
sudo useradd --system --no-create-home --shell /usr/sbin/nologin gitdr

sudo install -d -m 0750 -o root -g gitdr /etc/gitdr
sudo install -m 0640 -o root -g gitdr config.yaml /etc/gitdr/config.yaml
sudo install -m 0600 -o root -g root  gitdr.env   /etc/gitdr/gitdr.env    # secrets, root's only
sudo install -m 0711 -o root -g root  gitdr       /usr/local/bin/gitdr    # the binary
```

`gitdr.env` is `KEY=value` lines, the secrets from the quickstart
(`GITDR_GITHUB_APP_PRIVATE_KEY`, `GITDR_MANIFEST_SIGNING_KEY`, `AWS_*`, optionally
`GITDR_ENCRYPTION_KEY`).

git runs as the gitdr user and parses whatever the source sends, so nothing that user can read
should hold a secret. Only root reads `gitdr.env`: systemd reads it before it switches to the
gitdr user, and the cron wrapper reads it as root and then runs gitdr as gitdr. gitdr gets its
keys in its environment, which git never sees, rather than from files. `/etc/gitdr` and
`config.yaml` are root's as well, so the gitdr user can read the config and cannot change it.

The binary is root's with mode 0711, so the gitdr user can run it and not read it. The kernel
starts a program its user cannot read non-dumpable, and so git, which runs as the gitdr user too,
cannot read gitdr's environment through `/proc`, not even in the milliseconds before gitdr can
protect itself. The container image installs it the same way.

Each run gets an empty `HOME` of its own, which goes away when the run ends, apart from the cache
in `/var/cache/gitdr` where clones and bundles go. Otherwise a git compromised by one repository
could write a `.gitconfig` that the git of every later run would read. Put any git configuration
you need in `/etc/gitconfig`, which root owns.

## systemd (preferred, it sandboxes the run)

```sh
sudo cp systemd/gitdr.service systemd/gitdr.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now gitdr.timer

systemctl list-timers gitdr.timer        # next run
sudo systemctl start gitdr.service        # run once now
journalctl -u gitdr.service -f            # logs
```

The timer activates the oneshot service, don't `enable` the service itself. When the run ends,
systemd also stops every process it left behind.

## cron (where systemd isn't available)

```sh
sudo install -m 0755 -o root -g root cron/gitdr-backup.sh /usr/local/bin/gitdr-backup.sh
sudo install -m 0644 -o root -g root cron/gitdr.cron      /etc/cron.d/gitdr
sudo install -d -m 0700 -o gitdr -g gitdr /var/cache/gitdr
```

The wrapper runs as root. It reads `gitdr.env`, makes the run's empty `HOME`, and runs gitdr as
the gitdr user with `setpriv` (util-linux). Since root runs it, only root may be able to change
it. cron gives you none of systemd's sandboxing and leaves running whatever a run started, so
prefer the systemd path when you can.

## Did it work?

A run exits non-zero on any failure. For alerting, set `metrics.textfilePath` and watch
`gitdr_last_successful_run` via node_exporter's textfile collector, the one metric DR
alerting needs. Spot-check with `gitdr verify --manifest <key>`.
