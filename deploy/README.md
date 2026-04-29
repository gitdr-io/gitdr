# Running gitdr on a VM (systemd / cron)

Sample units for scheduling `gitdr backup` on a plain server. On Kubernetes use the
Helm chart in [`../charts/gitdr`](../charts/gitdr) instead. First walk through
[`../docs/QUICKSTART.md`](../docs/QUICKSTART.md) to get config + credentials in place.

## Common setup

```sh
# dedicated unprivileged user, no login, no home
sudo useradd --system --no-create-home --shell /usr/sbin/nologin gitdr

sudo install -d -m 0750 -o gitdr -g gitdr /etc/gitdr
sudo install -m 0640 -o gitdr -g gitdr config.yaml /etc/gitdr/config.yaml
sudo install -m 0600 -o gitdr -g gitdr gitdr.env  /etc/gitdr/gitdr.env   # secrets
sudo install -m 0755 /usr/local/bin/gitdr /usr/local/bin/gitdr           # the binary
```

`gitdr.env` is `KEY=value` lines, the secrets from the quickstart
(`GITDR_GITHUB_APP_PRIVATE_KEY`, `GITDR_MANIFEST_SIGNING_KEY`, `AWS_*`, optionally
`GITDR_ENCRYPTION_KEY`).

## systemd (preferred, it sandboxes the run)

```sh
sudo cp systemd/gitdr.service systemd/gitdr.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now gitdr.timer

systemctl list-timers gitdr.timer        # next run
sudo systemctl start gitdr.service        # run once now
journalctl -u gitdr.service -f            # logs
```

The timer activates the oneshot service, don't `enable` the service itself.

## cron (where systemd isn't available)

```sh
sudo install -m 0755 cron/gitdr-backup.sh /usr/local/bin/gitdr-backup.sh
sudo install -m 0644 cron/gitdr.cron /etc/cron.d/gitdr
```

cron gives you none of systemd's sandboxing, so prefer the systemd path when you can.

## Did it work?

A run exits non-zero on any failure. For alerting, set `metrics.textfilePath` and watch
`gitdr_last_successful_run` via node_exporter's textfile collector, the one metric DR
alerting needs. Spot-check with `gitdr verify --manifest <key>`.
