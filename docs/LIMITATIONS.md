# fastvm — limitations (v0.1)

fastvm v0.1 is a single-host prototype: the core lifecycle, storage, networking,
and API are implemented and tested end-to-end on KVM. The list below collects
what is deliberately not there yet. Intentional non-goals through v0.1 (multi-host
scheduling, live migration, GPU passthrough, Windows/arm64 guests, Kubernetes,
browser GUI, …) are in [DESIGN.md §4](DESIGN.md#4-non-goals-through-v01).

## Not implemented yet

- **Per-sandbox network namespaces.** TAPs share the daemon's network namespace.
  Isolation is enforced by the eBPF switch (or bridge port isolation in fallback
  mode) plus static neighbor/DNAT rules; full netns hardening is a later
  milestone.
- **`uffd`/`swap` memory offload.** Only `file` (the default: a Cloud
  Hypervisor snapshot restored paused with its memory image file-backed and
  demand-paged) and `never` are implemented; the other modes are config-validated
  but rejected by the daemon.
- **No VMM jailer.** Cloud Hypervisor ships a seccomp filter (enabled by default)
  and optional Landlock, but there is no Firecracker-style jailer. Run the daemon
  and VMM with minimal privileges; a hardened jail/namespace is part of the M7
  hardening milestone.
- **Host-directory volumes are static and allowlisted.** `--volume
  host:guest[:ro]` / `volumes` in the API are attached at sandbox create time;
  there is no hotplug or dynamic attach/detach. Host paths must be absolute and
  under `host_volumes.allowed_prefixes` (empty or absent denies all host mounts),
  `virtiofsd` should be sandboxed, and mounted host directories must be treated
  as trusted input. virtio-fs DAX is not supported by Cloud Hypervisor, so shares
  use a single queue.
- **Dormant snapshots are Cloud-Hypervisor-version specific.** The snapshot
  format (`config.json`, `memory-ranges`, `state.json`) restores only with the
  same CH version, CPU features, and disk/vsock paths. Pin Cloud Hypervisor and
  virtiofsd per host; there is no cross-host snapshot portability.
- **Boot/wake timing is being re-baselined.** With Cloud Hypervisor the measured
  cold boot to agent-ready is ~0.6 s (the old Firecracker claim was ~100 ms);
  pause/wake numbers in the design doc predate the migration and will be
  re-measured.
- **`dm-thin` CoW volumes.** Reflink and sparse-copy fallbacks are implemented;
  `dm-thin` returns `ErrDMThinUnsupported`.
- **Deep sleep with the VMM exited.** The VMM and its device queue stay alive so
  a wake packet has somewhere to land.
- **Cross-host snapshots, live migration, and live VMM adoption.** Memory images
  are local wake aids; after a crash or `kill -9` non-dormant sandboxes are
  marked failed.
- **Persistent sandbox disks.** Runtime volumes are ephemeral CoW clones; there
  is no disk snapshot/rollback, volume resize, or persistent user volume.
- **IPv6 in the network data path** and **non-x86_64 guests**.

## Behavior worth knowing

- **eBPF switch topology.** `network.datapath.mode: ebpf` (the default) uses one tc ingress
  classifier per switch port: TAP ingress sees guest→host frames (redirected to
  the uplink port), uplink ingress sees host→guest frames (redirected into the
  TAP, emitting a ringbuf wake event if the sandbox is asleep). There is no
  Linux bridge in this mode. `network.datapath.mode: bridge` is the automatic fallback
  when eBPF cannot be loaded (no `CAP_BPF`/BTF/verifier support).
- **Serial console.** Cloud Hypervisor runs with `--serial file=<serial.log>`,
  so the guest kernel console (`ttyS0`) and `FASTVM_BOOT_OK` are captured in
  `sandboxes/<id>/serial.log`; it is the first place to look when a boot fails.
  vsock reconnects on wake and exec/filesystem state is intact after restore.
- **Daemon restart.** A graceful stop snapshots Running/Idle/Paused sandboxes to
  Dormant (best-effort, up to 2 min each) and the next start adopts them
  (network re-registered, memory file preserved, wake lazily). A crash or
  `kill -9` leaves no live VMM to adopt.
- **user_data** assumes a POSIX shell; hardened images without `/bin/sh` need a
  custom `/init` or image that provides one.
- **Images are registry pulls, not container builds.** fastvm pulls OCI images
  directly from a registry and converts them into base disks; Dockerfile builds
  are not supported, and sandboxes are microVMs, not containers.
- **Not for hostile multi-tenancy yet.** Per-sandbox netns (M2) and VMM
  hardening (CH seccomp/Landlock, sandboxed virtiofsd, no jailer yet; M7) land
  later; mutually hostile tenants are a v1.0 concern.
