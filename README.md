# Panel-Agent

**English** | [**简体中文**](README.zh-CN.md)

A Go control panel and polling agent for proxy nodes, user permissions, three-dimensional quotas, durable traffic accounting, and hidden one-hop exits. VLESS/Reality and Trojan use one Xray process per machine; hy2 uses the official Hysteria 2 core, with hy2-only and dual-core modes.

## Release status

Version **0.1.0** is a **prerelease**. Windows tests/vet, Linux integration tests with real cores, public four-host traffic and backup/restore checks have passed. The latest VM soak recorded 16,962 requests over 3,750 seconds before the user stopped it. The final client log contains an HTTP EOF with unresolved ordering relative to shutdown. The required two-hour soak and final release acceptance remain incomplete. See the [task index](tasks/README.md) and [retained evidence](bench/results/).

## Features

- Node management, scoped users, private subscriptions, and fail-closed permissions.
- Persistent accounting, replay protection, offline reconciliation, and quotas.
- Direct and one-hop Reality routes, official hy2, and bounded declarative protocol extensions.
- Ownership-aware installation, manual core upgrades, rollback, and encrypted backups.

## Platforms and resources

The installer targets Debian 12+/Ubuntu 22.04+, systemd, and amd64/arm64. Resource targets are 128 MiB/1 GB for a Reality agent and 1 CPU/512 MiB for a panel with local nodes. Capacity depends on workload; hy2 and dual-core deployments have separate resource gates. A 128 MiB hy2 guarantee is not established.

Bundled versions are Xray **26.10.21** and Hysteria **2.13.0**. Low-memory upgrade tests explicitly configured `GOMEMLIMIT=32MiB GOGC=50`; installation does not automatically set these variables.

## Build and installation

Go modules require Go 1.26+. Packaging requires Go 1.27.1, PowerShell 7, WSL Debian, and the pinned official core packages. Run `scripts/package-linux.ps1` with a new output directory inside this project; its parameters are documented in [installation](docs/installation.md) and [hy2 setup](docs/hysteria2.md). Packages include the programs, cores, installation scripts, dependency licenses, manifests, and SHA-256 checksums.

Download the matching Linux archive and verify the outer checksum, then extract it and run:

```sh
sha256sum -c SHA256SUMS
sudo sh ./install.sh . --role panel-agent
```

Follow [installation](docs/installation.md) and [panel administration](docs/panel-admin.md) for HTTPS, administrator initialization, pairing, and service startup. DNS, firewall, NAT, nginx, and system networking remain operator-managed. Package manifests retain `unaccepted_local_candidate` until full acceptance.

## Operation and validation

See [updates](docs/panel-upgrades.md), [backup/restore](docs/panel-backup.md), and [traffic ledgers](docs/agent-ledger.md). Normal uninstall retains data; explicit `--purge` removes owned role data. Basic checks are `go test ./...` and `go vet ./...`; real-core and resource harnesses live under `bench/`.

## Repository layout

`cmd/` contains program entry points; `internal/` contains application logic and tests; `scripts/` packages releases; `docs/` documents operations; `bench/` contains integration harnesses and evidence. The [task index](tasks/README.md) is the sole execution-status source. [Maintenance](维护文档.md) records the release workflow.

## Changes in 0.1.0

Initial prerelease with both cores, bounded traffic retention and durable identity proofs, counter-based core health checks, update compatibility checks, and externally leased test supervision.

## Licensing and acknowledgements

No project software license has been declared. Public visibility does not grant a project license. Third-party licenses are distributed separately with the packages. Thanks to Xray-core, Hysteria, Go, and the dependencies listed in `go.mod`.

## Usage

Operate only machines and networks you are authorized to manage. This prerelease retains the validation limits described above.
