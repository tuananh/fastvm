# fastvm documentation

| Document | What it covers |
|---|---|
| [Design](DESIGN.md) | Goals, architecture, lifecycle, storage, networking, security, milestones |
| [Getting started](GETTING_STARTED.md) | Build, config, images, sandboxes, shell, wake-on-network, testing, troubleshooting |
| [Limitations](LIMITATIONS.md) | What v0.1 does not do yet, and intentional non-goals |
| [Cloud Hypervisor migration](CLOUD_HYPERVISOR_MIGRATION.md) | Why the VMM moved from Firecracker to Cloud Hypervisor: kernel, tooling, virtio-fs host volumes, verified spike |
| [fastvmctl CLI](cli/README.md) | Manpage-style reference for every command |
| [`.fvmfile` configs](FVMFILE.md) | Declarative sandbox configs and examples |
| [Auto-publishing ports](AUTO_PUBLISH.md) | Discover guest TCP listeners and publish them on loopback |
| [Snapshots](SNAPSHOT.md) | Save and restore sandbox memory, device state, and writable disk |
| [Sandbox lifecycle](LIFECYCLE.md) | Running, idle, paused, dormant, wake, and auto-sleep |
| [Guest agent](GUEST.md) | Guest init, vsock protocol, and workload lifecycle |
| [REST API](API.md) | Unix socket and optional TCP REST transport with mTLS |
| [Examples](../examples/) | Coding agents, headless Chrome, GitHub Actions runner |
| [Prometheus metrics](METRICS.md) | Daemon and per-sandbox metrics, scrape config |
| [CoW volumes](SETUP_COW_VOLUME.md) | Loopback XFS setup, reflink verification, snapshot/CoW checks |
| [Web console](../console/README.md) | OIDC login, sandbox dashboard, live utilization, exec terminal, dev proxy |

The top-level [README](../README.md) has the project overview and quickstart.

## Repository layout

```
cmd/fastvm/            CLI client
cmd/fastvmd/           daemon
cmd/fastvm-agent/      guest agent (static, injected into base disks)
internal/api/          HTTP/JSON API
internal/image/       OCI registry pull → rootfs → ext4 base disk
internal/volume/       CoW runtime volumes
internal/vmm/          VMM interface; cloudhypervisor/ driver
internal/agent/        vsock wire protocol + host client
internal/network/      netns/veth/TAP/bridge/NAT + wake detection
internal/switch/       eBPF L2 switch backend + bridge fallback
internal/wake/         wake coalescing/rate limiting
internal/snapshot/     (mem-file restore is in the VMM driver today)
internal/lifecycle/    state machine + idle predicate
internal/daemon/       runtime orchestration
cmd/fastvm-agent/      guest init + agent
bpf/                   L2 switch eBPF program
docs/                  DESIGN.md, GETTING_STARTED.md, LIMITATIONS.md, GUEST.md, CLOUD_HYPERVISOR_MIGRATION.md, SETUP_COW_VOLUME.md
test/e2e/              gated end-to-end tests + Cloud Hypervisor kernel/rootfs fetch script
hack/                  runnable demo, config, and setup helpers
console/               React + Vite+ web console (OIDC, dashboard, live stats)
```
