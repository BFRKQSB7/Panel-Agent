#!/bin/sh
set -eu
test "$#" = 3
source_dir=$1
archive=$2
epoch=$3
case "$epoch" in ''|*[!0-9]*) exit 2;; esac
run=$(mktemp -d /var/tmp/panel-agent-release-XXXXXXXX)
trap 'case "$run" in /var/tmp/panel-agent-release-*) rm -rf -- "$run";; esac' EXIT HUP INT TERM
cp -R "$source_dir/." "$run/"
find "$run" -type f -exec chmod 644 {} +
find "$run" -type d -exec chmod 755 {} +
chmod 755 "$run/control" "$run/control-agent" "$run/panel-agent-installer" "$run/xray" "$run/install.sh" "$run/uninstall.sh" "$run/cleanup.sh"
tar --sort=name --mtime="@$epoch" --owner=0 --group=0 --numeric-owner -czf "$archive" -C "$run" .
