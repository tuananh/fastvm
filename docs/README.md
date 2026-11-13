# fastvm documentation

| Document | What it covers |
|---|---|
| [Design](DESIGN.md) | Goals, architecture, lifecycle, storage, networking, security, milestones |
| [Getting started](GETTING_STARTED.md) | Build, config, images, sandboxes, shell, wake-on-network, testing, troubleshooting |
| [Limitations](LIMITATIONS.md) | What v0.1 does not do yet, and intentional non-goals |
| [CoW volumes](SETUP_COW_VOLUME.md) | Loopback XFS setup, reflink verification, snapshot/CoW checks |

The top-level [README](../README.md) has the project overview and quickstart.

## Demo assets

- [`demo.tape`](demo.tape) — source of the terminal demo recording.
- [`demo.gif`](demo.gif) — the rendered demo shown in the README.
- [`hack/demo-vhs.sh`](../hack/demo-vhs.sh) — one-command recorder: throwaway
  daemon, image build, capture, cleanup.

## Repository layout

```
cmd/fastvm/            CLI client
cmd/fastvmd/           daemon
cmd/fastvm-agent/      guest agent (static, injected into base disks)
internal/api/          HTTP/JSON API
internal/build, image/ Dockerfile → OCI → ext4 base disk
internal/volume/       CoW runtime volumes
internal/template/     baked-in coding-agent templates
internal/vmm/          VMM interface; firecracker/ driver
internal/agent/        vsock wire protocol + host client
internal/network/      netns/veth/TAP/bridge/NAT + wake detection
internal/switch/       eBPF L2 switch backend + bridge fallback
internal/wake/         wake coalescing/rate limiting
internal/snapshot/     (mem-file restore is in the VMM driver today)
internal/lifecycle/    state machine + idle predicate
internal/daemon/       runtime orchestration
guest/                 guest init + workload config
bpf/                   L2 switch eBPF program
docs/                  DESIGN.md, GETTING_STARTED.md, LIMITATIONS.md, SETUP_COW_VOLUME.md
test/e2e/              gated end-to-end tests + asset fetch script
hack/                  runnable demo, config, and setup helpers
```
