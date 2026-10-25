#!/bin/sh
# Explicitly authorized 128MiB host only; caller owns this private run directory.
set -eu
if [ "$#" -ne 3 ]; then echo 'usage: d15-host-smoke.sh RUN_DIR HOSTNAME PRIVATE_IP' >&2; exit 2; fi
root=$1
case "$root" in /tmp/panel-agent-d15-*) ;; *) echo 'unsafe run directory' >&2; exit 1;; esac
test "$(hostname)" = "$2"
ip -4 addr show | grep -F "$3/" >/dev/null
test "$(uname -m)" = x86_64
test "$(cat /sys/fs/cgroup/memory.max)" = 134217728
grep -Fx '0::/' /proc/1/cgroup >/dev/null
mkdir -m 700 "$root/tmp"
cat /sys/fs/cgroup/memory.events > "$root/memory.before"
df -Pk "$root"
printf 'swap_limit_bytes='; cat /sys/fs/cgroup/memory.swap.max
sampler=
cleanup() {
    if [ -n "$sampler" ]; then kill "$sampler" 2>/dev/null || true; wait "$sampler" 2>/dev/null || true; fi
    for path in /proc/[0-9]*/exe; do
        executable=$(readlink "$path" 2>/dev/null || true)
        case "$executable" in "$root/"*) pid=${path#/proc/}; pid=${pid%/exe}; kill "$pid" 2>/dev/null || true;; esac
    done
}
trap cleanup EXIT HUP INT TERM
(
    while :; do
        printf '%s %s %s\n' "$(cat /sys/fs/cgroup/memory.current)" "$(cat /sys/fs/cgroup/memory.swap.current)" "$(df -Pk "$root" | tail -n 1 | awk '{print $4}')"
        sleep 0.1
    done
) > "$root/resource.samples" &
sampler=$!
set +e
env TMPDIR="$root/tmp" GOMEMLIMIT=32MiB GOGC=50 \
    PANEL_AGENT_TEST_XRAY_OLD="$root/old-xray" PANEL_AGENT_TEST_XRAY="$root/new-xray" \
    PANEL_AGENT_TEST_XRAY_PACKAGE="$root/current.zip" \
    "$root/agent.test" -test.run TestRealHistoricalCoreUpgrade -test.v -test.timeout 240s
result=$?
set -e
cleanup
sampler=
cat /sys/fs/cgroup/memory.events > "$root/memory.after"
awk 'NR==FNR {before[$1]=$2;next} $1=="oom" || $1=="oom_kill" {printf "%s_before=%s after=%s\n",$1,before[$1],$2;if ($2!=before[$1]) bad=1} END {exit bad}' "$root/memory.before" "$root/memory.after" || result=1
awk 'BEGIN {peak=0;swap=0;disk=0} $1>peak {peak=$1} $2>swap {swap=$2} disk==0 || $3<disk {disk=$3} END {printf "sampled_system_peak_bytes=%s swap_peak_bytes=%s minimum_disk_available_kib=%s samples=%s\n",peak,swap,disk,NR; if(NR<2) exit 1}' "$root/resource.samples" || result=1
df -Pk "$root"
exit "$result"
