# Migrating fastvm from Firecracker to Cloud Hypervisor

Status: **implemented on this branch** (not released). Firecracker is removed as
the VMM backend; Cloud Hypervisor (CH) is the only backend. Host-directory
volumes are mounted with virtio-fs.

Reference: [Cloud Hypervisor docs](https://github.com/cloud-hypervisor/cloud-hypervisor/tree/main/docs),
[virtio-fs / virtiofsd](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/fs.md),
[virtio-vsock](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/vsock.md),
[snapshot/restore](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/snapshot_restore.md).

## Why

- Firecracker has no virtio-fs device, so host-directory mounts are impossible
  without an out-of-band copy mechanism.
- Cloud Hypervisor provides virtio-fs (`--fs` + `virtiofsd`), virtio-blk/net/
  vsock/console, ACPI/PCI, snapshots, live migration and device hotplug.
- The fastvm VMM contract is already narrow (`internal/vmm/vmm.go`), and the
  guest↔host protocol is vsock-based, so the data plane and guest agent do not
  change.

Trade-offs: CH is a larger device model than Firecracker (boot/wake timing must
be re-baselined), it has **no jailer** (seccomp + optional Landlock; run it with
minimal privileges), and virtio-fs adds a per-share `virtiofsd` process plus
`--memory shared=on`.

## Kernel

Use the kernel published by the Cloud Hypervisor project — **do not build one**:

| Item | Value |
|---|---|
| Project | <https://github.com/cloud-hypervisor/linux/releases> |
| Release | `ch-release-v7.2.8-20261009` |
| x86_64 image | `vmlinux-x86_64` (ELF, ~46 MiB) |
| SHA-256 | `edfeb8a8a6c04ff2979e9f1c700feaa29b8f564f952261834ef0b7d2e673a853` |

This kernel is the one CH's own integration tests use. It has virtio-pci,
virtio-fs (incl. FUSE), virtio-blk/net/console/vsock built in — required because
CH uses a PCI transport (the old Firecracker kernel only had virtio-mmio and no
virtio-fs). `make assets` downloads and verifies this image into
`test/e2e/assets/vmlinux.bin`.

Kernels can also be managed at runtime with `fastvmctl kernel remote-ls`,
`download`, and `use`; the daemon stores them under its kernel directory and
switches the `current` symlink.

Kernel cmdline stays close to the Firecracker one
(`console=ttyS0 root=/dev/vda rw init=/init …`); CH is launched with
`--serial file=<serial.log>`, so `ttyS0` remains the console.

## Host requirements

- Cloud Hypervisor v54.x (`cloud-hypervisor`), KVM.
- `virtiofsd` ≥ 1.10 (the distro package ships `/usr/lib/virtiofsd`; make it
  available as `virtiofsd` on `PATH` or set `vmm.virtiofsd`).
- Root/`CAP_NET_ADMIN` for TAP/nftables, as before.

## What changed

| Area | Change |
|---|---|
| VMM driver | `internal/vmm/firecracker` → `internal/vmm/cloudhypervisor` (`--kernel/--cmdline/--cpus/--memory/--disk/--net/--vsock/--fs/--serial/--console/--api-socket`) |
| Control plane | CH REST API over the unix socket (`/vm.pause`, `/vm.resume`, `/vm.snapshot`, `/vm.shutdown`, `/vm.info`) instead of Firecracker's pre-boot config API |
| Boot | Launch one CH process with all devices; no multi-step pre-boot configuration |
| vsock | `--vsock cid=<n>,socket=<uds>`; CH uses the same `CONNECT <port>` host-UDS protocol as Firecracker, so the guest agent is unchanged |
| Disk | `--disk path=<cow volume>,image_type=raw` |
| Network | `--net tap=<tap>,mac=<mac>`; fastvm still creates TAP/veth and passes guest IP via the kernel cmdline (`fastvm.ip=`), so the eBPF/bridge data plane is unchanged |
| Snapshots | CH snapshot dir = `config.json`, `memory-ranges`, `state.json`; restore is a new CH process with `--restore source_url=file://<dir>` (starts paused, then `/vm.resume`) |
| Dormant/wake | Same state machine: pause → snapshot → stop VMM → restore paused (or on wake) → resume |
| Config | `vmm.binary`, `vmm.virtiofsd`; volume allowlist |
| CLI/API | `--volume host:guest[:ro]` on `run`, `volumes` in `POST /v1/sandboxes`, volumes in sandbox JSON |
| Console | create-VM volume fields and volume display |

## virtio-fs host volumes

Design (approved):

1. A sandbox can carry `volumes: [{host_path, guest_path, read_only}]`.
   `host_path` must be absolute and under one of `host_volumes.allowed_prefixes`
   (empty = deny); `guest_path` must be absolute; `read_only` defaults per API.
2. For each volume the driver spawns
   `virtiofsd --shared-dir=<host> --socket-path=<sandbox>/fs-<n>.sock` and passes
   `--fs tag=fastvm<n>,socket=...` plus `--memory size=…,shared=on` to CH.
   virtiofsd exits when the VMM disconnects, so the driver restarts it at the
   same socket path **before** a snapshot restore.
3. The daemon encodes mounts into the kernel cmdline as
   `fastvm.volume.<n>=<tag>:<guest_path>:<ro|rw>`; the static agent-init mounts
   them with the `virtiofs` filesystem before serving, so workloads and `exec`
   see them from the first command.
4. Snapshots preserve guest mounts across dormant/wake (validated in the spike:
   host file read, guest write visible on host, restore + resume + mount again).
   `virtiofsd` is restarted before restore; no host page cache is required for
   correctness.

## Verified implementation

End-to-end on this host (CH v54.0, CH kernel v7.2.8, virtiofsd 1.14.0):

- `fastvmd` boots, runs, execs and deletes sandboxes on CH.
- `--volume` mounts a host directory; guest init mounts it before the agent starts;
  host→guest and guest→host I/O work; `read_only` is enforced by virtiofsd and
  the guest mount.
- Shallow pause/resume and deep pause (dormant) work; snapshots contain
  `config.json`/`memory-ranges`/`state.json`; the daemon clears the reused
  snapshot dir before re-snapshotting.
- Restore re-spawns virtiofsd first, restores paused, resumes on wake, and the
  guest still has its virtio-fs mount.
- Daemon restart while Dormant adopts the sandbox, re-spawns virtiofsd, and
  resumes with the volume intact; delete stops CH + virtiofsd.
- The default static agent-init auto-mounts volumes; custom init scripts can
  still do the same. Bumping `image.formatVersion` invalidates cached disks
  built with the old init.

## Verified spike (before coding)

On this host (CH v54.0, CH kernel v7.2.8, virtiofsd 1.14.0, KVM):

- CH boot to guest agent ready: **~0.6 s**.
- Existing `internal/agent` connected over CH vsock and ran `exec` unchanged.
- virtio-fs: guest mounted the host share, read a host file and wrote a file that
  appeared on the host.
- Pause/resume preserved the guest; the agent was usable after resume.
- Pause → snapshot (`config.json`/`memory-ranges`/`state.json`) → stop VMM →
  restart virtiofsd → `--restore` → `vm.resume` → agent exec worked, and the
  virtio-fs share was usable again.

## Verification plan

- `go test ./...`, `go vet ./...`, `make build`.
- Real-CH e2e: boot, agent ready, exec, stats, pause/resume, snapshot/dormant,
  wake, daemon-restart adoption, delete.
- Volume e2e: host→guest read, guest→host write, read-only enforcement, allowlist
  validation, dormant/wake with an attached share, cleanup of virtiofsd.
- Console e2e: create VM with a volume, see volumes in the table/drawer, use the
  exec terminal.
- Re-baseline boot/wake timing and update the README claim accordingly.

## Open risks / follow-ups

- **Boot/wake timing**: CH's PCI/ACPI device model is heavier than Firecracker's;
  expect boot and wake numbers to change.
- **Security**: CH has seccomp (default) + optional Landlock, but no jailer;
  virtiofsd must be sandboxed (`--sandbox namespace/chroot`) and host paths
  restricted by the allowlist. Treat mounted host directories as trusted input.
- **Hotplug**: CH supports `vm.add-fs`/`add-disk`; volumes are static at create
  time for now.
- **Multi-queue / DAX**: virtio-fs DAX is not supported by CH; keep single queue
  unless benchmarks justify more.
- **Version pinning**: CH and virtiofsd versions should be pinned; the snapshot
  format is CH-version specific.
