#!/bin/sh
set -eu
test "$#" = 3
source_dir=$1
archive=$2
epoch=$3
case "$epoch" in ''|*[!0-9]*) exit 2;; esac
parent=$(cd "$(dirname "$archive")" && pwd)
run=$(mktemp -d "$parent/.archive-XXXXXXXX")
trap 'case "$run" in "$parent"/.archive-*) rm -rf -- "$run";; esac' EXIT HUP INT TERM
# Set archive modes explicitly: Windows-mounted source chmod is not reliable.
cd "$source_dir"
find . -type d -print0 | sort -z > "$run/directories"
find . -type f ! -path ./control ! -path ./control-agent ! -path ./panel-agent-installer ! -path ./xray ! -path ./hysteria ! -path ./install.sh ! -path ./uninstall.sh ! -path ./cleanup.sh -print0 | sort -z > "$run/files"
tar --mtime="@$epoch" --owner=0 --group=0 --numeric-owner --mode=0755 --no-recursion --null -cf "$run/bundle.tar" -T "$run/directories"
tar --mtime="@$epoch" --owner=0 --group=0 --numeric-owner --mode=0644 --null -rf "$run/bundle.tar" -T "$run/files"
set -- ./control ./control-agent ./panel-agent-installer ./xray ./install.sh ./uninstall.sh ./cleanup.sh
if [ -f ./hysteria ]; then set -- "$@" ./hysteria; fi
tar --mtime="@$epoch" --owner=0 --group=0 --numeric-owner --mode=0755 -rf "$run/bundle.tar" "$@"
gzip -n -c "$run/bundle.tar" > "$archive"
