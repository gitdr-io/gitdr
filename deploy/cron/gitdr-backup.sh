#!/bin/sh
# gitdr backup wrapper for cron (cron doesn't load EnvironmentFile or sandbox like
# systemd). Install to /usr/local/bin, mode 0755. Prefer the systemd unit where you can.
set -eu

# Load secrets + GITDR_* config overrides. Own gitdr.env gitdr:gitdr, mode 0600.
set -a
. /etc/gitdr/gitdr.env
set +a

export TMPDIR="${TMPDIR:-/var/cache/gitdr}"
export HOME="$TMPDIR"
mkdir -p "$TMPDIR"

exec /usr/local/bin/gitdr backup --config /etc/gitdr/config.yaml
