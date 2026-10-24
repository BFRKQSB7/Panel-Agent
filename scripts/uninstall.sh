#!/bin/sh
set -eu

release_dir=${1:?usage: uninstall.sh RELEASE_DIR [uninstaller options]}
shift
release_dir=$(CDPATH= cd -- "$release_dir" && pwd)
installer=${PANEL_AGENT_INSTALLER:-$release_dir/panel-agent-installer}
test -x "$installer"
exec "$installer" uninstall "$@"
