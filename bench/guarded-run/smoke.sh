#!/bin/sh
set -eu
test "$#" = 1
guard=$(realpath "$1")
test -x "$guard"
test -d .work
root=$(mktemp -d "$PWD/.work/guard-smoke-XXXXXXXX")
cleanup(){
 trap - EXIT
 case "$root" in "$PWD"/.work/guard-smoke-*) rm -rf -- "$root";; *) exit 1;; esac
}
trap cleanup EXIT
mkdir "$root/mount"
result=0
"$guard" -dir "$root/lost-control" -lease-seconds 3 -max-seconds 30 -unmount "$root/mount" -- sh -c 'mount -t tmpfs -o size=1m tmpfs "$1"; trap "" TERM; sleep 300 & echo $! > "$2"; wait' sh "$root/mount" "$root/child.pid" || result=$?
test "$result" = 124
grep -q lease_expired "$root/lost-control/status.json"
! mountpoint -q "$root/mount"
pid=$(cat "$root/child.pid")
if [ -r "/proc/$pid/stat" ];then test "$(awk '{print $3}' "/proc/$pid/stat")" = Z;fi
"$guard" -dir "$root/success" -lease-seconds 3 -max-seconds 10 -- sh -c 'exit 0'
result=0
"$guard" -dir "$root/hard-timeout" -lease-seconds 30 -max-seconds 2 -- sh -c 'sleep 300' || result=$?
test "$result" = 124
grep -q hard_timeout "$root/hard-timeout/status.json"
"$guard" -dir "$root/stopped" -lease-seconds 30 -max-seconds 30 -- sh -c 'sleep 300' &
pid=$!
for attempt in 1 2 3 4 5;do test ! -f "$root/stopped/status.json" || break;sleep 1;done
touch "$root/stopped/stop"
result=0;wait "$pid" || result=$?
test "$result" = 130
grep -q controller_stopped "$root/stopped/status.json"
result=0
"$guard" -dir "$root/failed" -lease-seconds 3 -max-seconds 10 -- sh -c 'exit 7' || result=$?
test "$result" = 1
grep -q test_failed "$root/failed/status.json"
cat > "$root/detached.sh" <<'CHILD'
trap '' TERM
sleep 300 &
echo $! > "$1"
wait
CHILD
result=0
"$guard" -dir "$root/detached-lost-control" -lease-seconds 2 -max-seconds 30 -- sh -c 'setsid sh "$1" "$2" & wait' sh "$root/detached.sh" "$root/detached.pid" || result=$?
test "$result" = 124
grep -q lease_expired "$root/detached-lost-control/status.json"
pid=$(cat "$root/detached.pid")
test ! -e "/proc/$pid"
"$guard" -dir "$root/detached-success" -lease-seconds 3 -max-seconds 10 -- sh -c 'setsid sh "$1" "$2" & while test ! -s "$2";do sleep .1;done;exit 0' sh "$root/detached.sh" "$root/detached-success.pid"
pid=$(cat "$root/detached-success.pid")
test ! -e "/proc/$pid"
echo GUARD_SMOKE_PASS_lease_expiry_force_kill_unmount_hard_timeout_manual_stop_success_failure_detached_sessions
