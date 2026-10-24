#!/bin/sh
set -eu

release_dir=${1:?usage: install.sh RELEASE_DIR [installer options]}
shift
release_dir=$(CDPATH= cd -- "$release_dir" && pwd)
test -f "$release_dir/SHA256SUMS"
(cd "$release_dir" && sha256sum -c SHA256SUMS)
test -x "$release_dir/panel-agent-installer"

role=panel-agent
previous=
for argument in "$@"; do
    if [ "$previous" = "--role" ]; then
        role=$argument
    fi
    previous=$argument
done

if [ "$role" = control ]; then
    exec "$release_dir/panel-agent-installer" install --control-bin "$release_dir/control" "$@"
fi

test -x "$release_dir/control-agent"
test -x "$release_dir/xray"
exec "$release_dir/panel-agent-installer" install \
    --control-bin "$release_dir/control" \
    --agent-bin "$release_dir/control-agent" \
    --xray-bin "$release_dir/xray" \
    "$@"
