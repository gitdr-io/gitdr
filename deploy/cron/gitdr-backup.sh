#!/bin/sh
# gitdr backup wrapper for cron (cron doesn't load EnvironmentFile or sandbox like systemd).
# Runs as root: it reads the secrets, which only root can, then runs gitdr as the gitdr user.
# Install to /usr/local/bin, owned by root, mode 0755. Prefer the systemd unit where you can.
set -eu

# Load secrets + GITDR_* config overrides. Own gitdr.env root:root, mode 0600.
set -a
. /etc/gitdr/gitdr.env
set +a

# Clones and bundles go in the cache, made at setup (deploy/README.md).
export TMPDIR="${TMPDIR:-/var/cache/gitdr}"

# HOME is apart from the cache: an empty directory for this run alone, removed when it ends, so
# a git compromised once cannot leave a .gitconfig for the next run.
HOME=$(mktemp -d /tmp/gitdr-home.XXXXXX)
chown gitdr:gitdr "$HOME"
export HOME

as_gitdr() { setpriv --reuid=gitdr --regid=gitdr --init-groups --no-new-privs "$@"; }
trap 'as_gitdr rm -rf "$HOME"' EXIT
as_gitdr /usr/local/bin/gitdr backup --config /etc/gitdr/config.yaml
