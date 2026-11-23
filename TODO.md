# TODO

Longer-term ideas and research items. Check items off as they land.

## Hardware & display

- [ ] **GPU passthrough**
  - Expose host GPUs to Cloud Hypervisor sandboxes via VFIO.
  - Cover IOMMU groups, host driver binding, guest drivers, and reset/recovery.
  - Decide resource model: whole GPU, MIG slice, or vGPU; how to schedule/claim GPUs per sandbox.
  - Add GPU capability reporting to `fastvmd`/`fastvmctl`.

- [ ] **Virtio-GPU / Venus for shared graphics**
  - CH v54 has no native virtio-gpu. The Spectrum OS patchset is only published
    for CH 50.0 (and older), not v54.
  - That patchset needs a patched `vhost` crate, crosvm's virtio-gpu, and
    non-standard vhost-user commands; crosvm is not installed here.
  - Cheaper spike: try CH v54 `--generic-vhost-user` with QEMU's
    `vhost-user-gpu` backend, but protocol/device compatibility is unverified.
  - The guest test kernel currently lacks `virtio_gpu`; a kernel with
    `CONFIG_DRM_VIRTIO_GPU` is required.
  - Target Venus (Vulkan-over-virtio), not CUDA. Guest image needs Mesa Venus
    and a Vulkan loader; desktop additionally needs a compositor + VNC/Wayland
    path.
  - Decide: forward-port Spectrum patches to v54, pin a patched CH 50 for GPU
    nodes, or prototype generic-vhost-user first.

- [ ] **VNC / graphical console**
  - Use Cloud Hypervisor's VNC support (or virtio-gpu + a display path) instead of exec-only access.
  - Decide auth model (loopback-only? per-sandbox token? SSH tunnel?) and clipboard/security policy.
  - Wire it into the web console or expose a local port through the CLI.

## Secrets

- [ ] **Secret injection and proxying**
  - Define the threat model: secrets must not leak into command lines, environment metadata, disk images, or snapshot bundles.
  - Options to compare:
    - agent-mediated injection into a tmpfs/secure path at runtime
    - per-sandbox secret proxy with short-lived credentials
    - egress proxy / eBPF-assisted credential injection for APIs
  - Add rotation, scoping to a sandbox, and redaction in logs/events.

## Snapshots

- [ ] **Make snapshot restores less restrictive**
  - Current `run --from-snapshot` rejects most useful flags: ports, env,
    volumes, network/allowlist changes, resources, entrypoint, workdir,
    auto-publish, and `--no-auto-sleep`.
  - Restored sandboxes also drop explicit port mappings, env, auto-publish, and
    `no-auto-sleep` because they are not stored in the snapshot manifest.
  - Decide what can safely be overridden after restore and what is fixed by the
    snapshot (image, vCPUs, memory).
  - Persist safe metadata (ports, auto-publish, `no-auto-sleep`) in the
    manifest and re-apply it on restore.
  - Allow safe post-restore overrides, e.g. `--publish`, `--env`,
    `--auto-publish`, `--no-auto-sleep`, and workdir/command for the restored
    workload.
  - Consider host volumes explicitly: either support snapshot+restore with
    volumes or keep rejecting them with a clear error.
  - Add tests for restored ports, auto-publish, and network configuration.

## Observability

- [x] **Basic Prometheus metrics**
  - `/metrics` on the unix socket and mTLS listener.
  - Daemon sandbox counts by state and image/volume cache size.
  - Per-sandbox CPU, VMM RSS, network rx/tx, wake/sleep/exec counts.
  - Remaining: error counters, operation histograms, and dashboard/alerts.

## Storage

- [ ] **Explore device-mapper thin provisioning for layered CoW**
  - Use a dm-thin pool for image -> kernel/updates -> resized writable overlay
    layering instead of one reflink clone per sandbox.
  - Evaluate thin snapshots for the base image and per-sandbox writable layer,
    online resize, discard/TRIM, and discard-on-destroy.
  - Cover pool sizing/overcommit, leak detection, recovery after daemon restart,
    and cleanup of orphaned thin volumes.
  - Compare with the current reflink/copy backends and decide whether to
    implement `storage.cow: dm-thin`.
  - Add integration tests on a disposable thin pool; skip cleanly without
    device-mapper privileges.

## Dynamic images / Nix

- [ ] **Let users request runtimes instead of full images**
  - Example: `fastvmctl run --with node,go …` resolves to a bootable sandbox
    containing those runtimes without rebuilding an ext4 image for every
    combination.
  - Explore Nix flakes/profiles to produce a reproducible closure for the
    requested packages.
  - Avoid image assembly on every run:
    - keep a minimal base rootfs and mount a shared read-only Nix store via
      virtio-fs
    - or add the Nix closure as a read-only lower layer and use a writable
      overlay/CoW layer per sandbox
  - Cache closures by content hash and share them across sandboxes; add GC.
  - Keep the guest agent, init, kernel, and base OCI/rootfs model compatible.
  - Define how snapshots, persistent writes, package installs, and `user_data`
    interact with a shared read-only Nix store.
  - Evaluate trust: pinned expressions, binary caches, and whether builds happen
    on the host, in a builder VM, or never in the guest.
  - Compare with the existing OCI layer cache and decide whether this is a
    separate image source or a layer on top of OCI base images.

## Declarative sandbox config

- [x] **Per-sandbox config file (e.g. `codex.fvmfile`)**
  - Strict YAML format with image, command, env (host `${VAR}` expansion), volumes, ports, network/allow-hosts, resources, and `no_auto_sleep`.
  - `fastvmctl run -f <file>`; documented in `docs/FVMFILE.md`.
  - Added examples: `hack/hello.fvmfile`, `hack/codex.fvmfile`, `hack/claude.fvmfile`, `hack/copilot.fvmfile`, `hack/nodejs.fvmfile`.
  - Coding-agent examples use `node:26-slim`; compressed layer blobs are shared across image pulls, with a source index to skip repeat extraction.
  - `template` subcommand and `hack/templates` removed; examples live in `hack/*.fvmfile`.
  - Added `user_data` / `user_data_file` cloud-init-style first-boot scripts.
  - Added `hack/docker.fvmfile` (Docker-in-VM); validate it against a host that can pull `docker:27-dind`.

## Ecosystem

- [ ] **containerd / Kubernetes shim**
  - Investigate a containerd runtime shim v2 backed by fastvm sandboxes.
  - Map pod lifecycle, CNI networking, volumes, and logs onto fastvm primitives.
  - Consider snapshot/restore integration and whether a CRI implementation is warranted.
  - Start with a prototype shim before committing to a Kubernetes integration.
