#!/bin/sh
set -eu
test "$#" = 1
run=$1
case "$run" in /tmp/panel-agent-d17-*) ;; *) exit 2;; esac
test "$(hostname)" = phoenix-free-vnic
ip -4 addr show | grep -F '10.0.0.61/' >/dev/null
test "$(uname -m)" = aarch64
cleanup() { case "$run" in /tmp/panel-agent-d17-*) rm -rf -- "$run";; esac; }
trap cleanup EXIT HUP INT TERM
mkdir -m 700 "$run/bundle"
tar -xzf "$run/package.tar.gz" -C "$run/bundle"
(cd "$run/bundle" && sha256sum -c SHA256SUMS >/dev/null)
cat /etc/os-release
head -n 1 /proc/meminfo
"$run/bundle/control" version
"$run/bundle/control-agent" version
"$run/bundle/xray" version
chmod 700 "$run/agent.test" "$run/control.test"
PANEL_AGENT_TEST_AGENT_PROGRAM="$run/bundle/control-agent" "$run/agent.test" -test.run TestAgentProgramUpdatePreservesIdentityAndUnacknowledgedLedger -test.v -test.timeout 60s
"$run/control.test" -test.run TestPanelSelfUpdatePublishesOrRestoresCompleteGeneration -test.v -test.timeout 90s
echo 'arm64_package_hashes=ok agent_update=ok panel_update=ok'
