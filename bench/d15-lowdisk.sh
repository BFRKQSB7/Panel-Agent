#!/bin/sh
set -eu
if [ "$#" -ne 3 ]; then echo 'usage: d15-lowdisk.sh AGENT_TEST OLD_XRAY NEW_XRAY' >&2; exit 2; fi
run=$(mktemp -d /var/tmp/panel-agent-d15-lowdisk-XXXXXXXX)
mounted=false
cleanup() {
    if "$mounted"; then umount "$run"; fi
    rmdir "$run"
}
trap cleanup EXIT HUP INT TERM
mount -t tmpfs -o size=48m tmpfs "$run"
mounted=true
env PANEL_AGENT_TEST_LOW_DISK_ROOT="$run" PANEL_AGENT_TEST_XRAY_OLD="$2" PANEL_AGENT_TEST_XRAY="$3" \
    "$1" -test.run TestRealCoreUpgradeLowDisk -test.v -test.timeout 45s
