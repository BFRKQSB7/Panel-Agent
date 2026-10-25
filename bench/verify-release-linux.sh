#!/bin/sh
set -eu
test "$#" = 1
test "$(id -u)" = 0
test "$(uname -m)" = x86_64
for unit in panel-agent-control.service panel-agent-agent.service panel-agent-control-cleanup.service panel-agent-control-cleanup.timer panel-agent-agent-cleanup.service panel-agent-agent-cleanup.timer; do
    test ! -e "/etc/systemd/system/$unit"
done
run=$(mktemp -d /var/tmp/panel-agent-d17-XXXXXXXX)
bundle="$run/bundle"
installer() {
    "$bundle/panel-agent-installer" "$@" --project-root "$run/program" --data-root "$run/data" --log-root "$run/logs" --service-user root --service-group root
}
cleanup() {
    result=$?
    trap - EXIT HUP INT TERM
    set +e
    for role in control agent; do
        if [ -f "$run/data/$role/ownership.json" ]; then installer uninstall --role "$role" --purge || result=1; fi
    done
    for unit in panel-agent-control.service panel-agent-agent.service panel-agent-control-cleanup.timer panel-agent-agent-cleanup.timer; do
        if [ -e "/etc/systemd/system/$unit" ]; then echo "test unit cleanup failed: $unit" >&2; result=1; fi
    done
    case "$run" in /var/tmp/panel-agent-d17-*) rm -rf -- "$run";; *) result=1;; esac
    exit "$result"
}
trap cleanup EXIT HUP INT TERM
mkdir -m 700 "$bundle"
tar -xzf "$1" -C "$bundle"
(cd "$bundle" && sha256sum -c SHA256SUMS >/dev/null)
test -x "$bundle/control"
test -x "$bundle/control-agent"
test -x "$bundle/panel-agent-installer"
test -x "$bundle/xray"
"$bundle/control" version
"$bundle/control-agent" version
installer install --role panel-agent --control-bin "$bundle/control" --agent-bin "$bundle/control-agent" --xray-bin "$bundle/xray"
printf '%s\n%s\n' 'isolated-release-fixture-password' 'isolated-release-fixture-password' | "$run/program/bin/control" admin init --data-dir "$run/data/control" --password-stdin
"$run/program/bin/control" check --data-dir "$run/data/control"
key_before=$(sha256sum "$run/data/control/master.key" | cut -d ' ' -f1)
sha=$(sha256sum "$bundle/control" | cut -d ' ' -f1)
printf '%s\n' 'isolated-release-backup-password' | "$run/program/bin/control" update --data-dir "$run/data/control" --package "$bundle/control" --target-version 0.1.0 --sha256 "$sha" --backup-password-stdin
test "$(sha256sum "$run/data/control/master.key" | cut -d ' ' -f1)" = "$key_before"
"$run/program/bin/control" check --data-dir "$run/data/control"
installer cleanup --role panel-agent
installer uninstall --role panel-agent
test -f "$run/data/control/panel.db"
test -f "$run/data/control/master.key"
test -f "$run/data/agent/ownership.json"
installer install --role panel-agent --control-bin "$bundle/control" --agent-bin "$bundle/control-agent" --xray-bin "$bundle/xray"
test "$(sha256sum "$run/data/control/master.key" | cut -d ' ' -f1)" = "$key_before"
"$run/program/bin/control" check --data-dir "$run/data/control"
installer uninstall --role panel-agent --purge
test ! -e "$run/data/control"
test ! -e "$run/data/agent"
test -f "$bundle/control"
echo 'release_archive_hashes=ok executable_modes=ok install_update_uninstall_reinstall_purge=ok preserved_key=ok'
