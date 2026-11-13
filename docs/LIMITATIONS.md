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
- **`uffd`/`swap` memory offload.** Only `file` (the default, Firecracker
  File-backend mmap restore) and `never` are implemented; the other modes are
  config-validated but rejected by the daemon.
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

- **eBPF switch topology.** `switch.mode: ebpf` (the default) uses one tc ingress
  classifier per switch port: TAP ingress sees guest→host frames (redirected to
  the uplink port), uplink ingress sees host→guest frames (redirected into the
  TAP, emitting a ringbuf wake event if the sandbox is asleep). There is no
  Linux bridge in this mode. `switch.mode: bridge` is the automatic fallback
  when eBPF cannot be loaded (no `CAP_BPF`/BTF/verifier support).
- **Serial console after restore.** Snapshot restore does not reattach the guest
  serial console (a Firecracker limitation); vsock reconnects on wake and
  exec/filesystem state is intact.
- **Daemon restart.** A graceful stop snapshots Running/Idle/Paused sandboxes to
  Dormant (best-effort, up to 2 min each) and the next start adopts them
  (network re-registered, memory file preserved, wake lazily). A crash or
  `kill -9` leaves no live VMM to adopt.
- **Guest init** assumes a POSIX shell and busybox `mount`; images without them
  need a custom `/init` via `init_path`.
- **Image builds are not a container runtime.** fastvm uses BuildKit/Docker to
  build base disks and converts the resulting OCI image; there is no
  from-scratch Dockerfile engine, and sandboxes are microVMs, not containers.
- **Not for hostile multi-tenancy yet.** Per-sandbox netns (M2) and jailer
  hardening (M7) land later; mutually hostile tenants are a v1.0 concern.
