#!/bin/sh
set -eu

output_dir=${1:-dist}
architecture=${2:-amd64}
case "$architecture" in
    amd64|arm64) ;;
    *) echo "unsupported Linux architecture: $architecture" >&2; exit 2 ;;
esac

mkdir -p "$output_dir"
GOOS=linux GOARCH="$architecture" CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o "$output_dir/control" ./cmd/control
GOOS=linux GOARCH="$architecture" CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o "$output_dir/control-agent" ./cmd/agent
GOOS=linux GOARCH="$architecture" CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o "$output_dir/panel-agent-installer" ./cmd/installer

if [ -n "${PANEL_AGENT_XRAY:-}" ]; then
    test -f "$PANEL_AGENT_XRAY"
    cp -- "$PANEL_AGENT_XRAY" "$output_dir/xray"
    chmod 0755 "$output_dir/xray"
fi

(cd "$output_dir" && sha256sum control control-agent panel-agent-installer ${PANEL_AGENT_XRAY:+xray} > SHA256SUMS)
