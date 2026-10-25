#!/bin/sh
# Run the opt-in real upgrade suite inside a 128MiB cgroup on systemd Linux.
set -eu
if [ "${1:-}" = --worker ]; then
    shift
    export TMPDIR="$1" PANEL_AGENT_TEST_XRAY_OLD="$2" PANEL_AGENT_TEST_XRAY="$3" PANEL_AGENT_TEST_XRAY_PACKAGE="$4"
    shift 4
    group=$(awk -F: '$1 == "0" {print $3}' /proc/self/cgroup)
    cgroup="/sys/fs/cgroup$group"
    test "$(cat "$cgroup/memory.max")" = 134217728
    cat "$cgroup/memory.events"
    set +e
    "$1" -test.run TestRealHistoricalCoreUpgrade -test.v -test.timeout 240s
    result=$?
    set -e
    printf 'cgroup_peak_bytes='; cat "$cgroup/memory.peak"
    printf 'cgroup_swap_bytes='; cat "$cgroup/memory.swap.current"
    cat "$cgroup/memory.events"
    awk '$1 == "oom" || $1 == "oom_kill" {if ($2 != 0) exit 1}' "$cgroup/memory.events" || result=1
    exit "$result"
fi
if [ "$#" -ne 4 ]; then
    echo 'usage: d15-resource.sh AGENT_TEST OLD_XRAY NEW_XRAY OFFICIAL_ZIP' >&2
    exit 2
fi
test "$(id -u)" = 0
run=$(mktemp -d /var/tmp/panel-agent-d15-resource-XXXXXXXX)
trap 'case "$run" in /var/tmp/panel-agent-d15-resource-*) rm -rf -- "$run";; esac' EXIT HUP INT TERM
unit=$(basename "$run")
systemd-run --unit="$unit" --wait --pipe --collect -p MemoryMax=128M -p MemorySwapMax=0 \
    /bin/sh "$(realpath "$0")" --worker "$run" "$2" "$3" "$4" "$1"
