# fastvm — Getting Started

A complete walkthrough: build the binaries, start the daemon with a ready-made
config, pull a base image, boot a real microVM, exec into it,
pause it, spill guest RAM to disk, and wake it from a network request.

Design and roadmap: [DESIGN.md](DESIGN.md). CLI/API/limits summary: [README.md](../README.md).

---

## 1. Requirements

| Requirement | Notes |
|---|---|
| Linux x86_64 + KVM | `/dev/kvm` must be accessible. |
| **root / `CAP_NET_ADMIN`** | The daemon creates TAP devices, a Linux bridge, and nftables rules. Run `fastvmd` as root (or grant it `CAP_NET_ADMIN`). Required for booting sandboxes and for `--publish` ports. |
| Cloud Hypervisor v54+ | `cloud-hypervisor`, or the static build `cloud-hypervisor-static` (this host: `/usr/bin/cloud-hypervisor-static`, v54.0). Check with `cloud-hypervisor --version` / `cloud-hypervisor-static --version`. The daemon searches `cloud-hypervisor` then `cloud-hypervisor-static` on `PATH`; override with `vmm.binary` (absolute path) or `FASTVM_CLOUD_HYPERVISOR`. |
| virtiofsd ≥ 1.10 | Serves host-directory volumes over virtio-fs. The distro package often ships `/usr/lib/virtiofsd` instead of putting `virtiofsd` on `PATH`; the daemon falls back to it. Only needed for `--volume` sandboxes. Override with `vmm.virtiofsd` / `FASTVM_VIRTIOFSD`. |
| Go 1.24+ | Only to build from source. |
| Kernel image | `make assets` downloads the pinned Cloud Hypervisor test kernel (`vmlinux-x86_64`, sha256 `edfeb8a8…a853`) into `test/e2e/assets/vmlinux.bin` and links it at `/tmp/fastvm/kernels/current` for the demo config. Use `fastvmctl kernel download/use` to manage kernels through the daemon, or point `kernel_path` at your own pinned kernel with virtio-pci/virtio-fs built in. |
| e2fsprogs (`mke2fs`), `nft` | Base disk creation and DNAT rules. |

> The public sandbox this repo was developed in runs as an unprivileged user, so
> it cannot create TAP/bridge/nftables. Run the walkthrough below on a normal
> KVM host as root (or a VM with the required capabilities). Unit tests run
> unprivileged; the gated e2e tests handle their own isolation.

---

## 2. Build

```sh
git clone <this repo> fastvm && cd fastvm
make build            # bin/fastvmctl, bin/fastvmd, bin/fastvm-agent
make test             # unit tests (no KVM/root needed)
make assets           # Cloud Hypervisor kernel + Alpine rootfs for demos/e2e (cached, gitignored)
sudo make install     # /usr/local/bin (or: make install PREFIX=$HOME/.local)
```

`make assets` downloads the prebuilt Cloud Hypervisor test kernel
(`ch-release-v7.2.8-20261009/vmlinux-x86_64`, SHA-256 verified) and an Alpine
rootfs into `test/e2e/assets/`, then prints those paths plus whether a Cloud
Hypervisor binary and `virtiofsd` are available on the host (it never installs
them).

You can also manage kernels through the daemon:

```sh
fastvmctl kernel remote-ls
fastvmctl kernel download
fastvmctl kernel use ch-release-v7.2.8-20261009
fastvmctl kernel ls
```

`kernel use` switches the `current` symlink that `kernel_path` points at. No sudo
is required: the daemon writes its own kernel directory.

---

## 3. Start the daemon

A ready-to-use demo config is checked in at [hack/config.yaml](../hack/config.yaml).
It uses `/tmp` for throwaway state so you can try fastvm without installing
anything system-wide:

```yaml
data_dir: /tmp/fastvm/data
socket_path: /run/fastvm/fastvm.sock
kernel_path: /tmp/fastvm/kernels/current
agent_path: bin/fastvm-agent
vmm: {binary: cloud-hypervisor, virtiofsd: virtiofsd}
host_volumes: {allowed_prefixes: []}   # empty = deny all host-directory mounts
lifecycle: {idle_window: 60s, pause_after: 10m, offload_after: 30m, destroy_after: 0s}
# ... see the file for the lifecycle/memory/network sections
```

The demo `kernel_path` is absolute (`/tmp/fastvm/kernels/current`); the relative
`agent_path` is resolved from the directory you start the daemon in, so run from
the repo root. For a real installation, copy it
to `/etc/fastvm/config.yaml` and point the paths at `/var/lib/fastvm/...`.

`vmm.binary` / `vmm.virtiofsd` name the executables the daemon launches: it
prefers the configured value, otherwise searches `PATH` for `cloud-hypervisor`
then `cloud-hypervisor-static`, and for `virtiofsd` then `/usr/lib/virtiofsd`
(`/usr/libexec/virtiofsd`). Absolute paths are accepted, and
`FASTVM_CLOUD_HYPERVISOR` / `FASTVM_VIRTIOFSD` override both. `virtiofsd` is
only started when a sandbox has host-directory volumes.

Start the daemon as root (it runs in the foreground; `Ctrl-C` stops it):

```sh
make build                 # once, as your user
sudo make start            # runs bin/fastvmd --config hack/config.yaml
```

Or, in a root shell, the equivalent:

```sh
sudo -s
cd /path/to/fastvm
bin/fastvmd --config hack/config.yaml
```

What happens:

- The daemon creates `$DATA_DIR` (`/tmp/fastvm-demo-data`), writes
  `/run/fastvm/fastvm.sock` (mode 0600) and serves the HTTP/JSON API.
- With `network.datapath.mode: ebpf` (the default) the daemon loads the eBPF L2 switch.
  TAP ingress sees guest→host frames and redirects them to the uplink veth;
  uplink ingress sees host→guest frames, redirects them into the sleeping
  sandbox's TAP and emits a ringbuf wake event. Without `CAP_BPF`/BTF it logs
  `eBPF switch unavailable; using Linux bridge` and switches to the bridge +
  AF_PACKET wake path.
- On the first sandbox it creates TAP devices, the uplink veth pair, `/30`
  gateway addresses/static neighbors and nftables DNAT rules (bridge mode also
  creates `bridge fastvm0`).
- Each sandbox is one Cloud Hypervisor process launched with all of its devices
  at once (`--kernel`, `--cmdline`, `--cpus`, `--memory`, `--disk`, `--net`,
  `--vsock`, `--serial file=…`, plus one `--fs`/`virtiofsd` per host volume).
  The guest console (`ttyS0`) is written to `$DATA_DIR/sandboxes/<id>/serial.log`;
  pause/resume and snapshot/restore go through Cloud Hypervisor's REST API on a
  per-sandbox unix socket.
- `hack/config.yaml` uses generous windows (60 s idle, 10 min pause, 30 min
  dormant) so a coding-agent VM is not paused a few seconds after you stop
  typing. Tune `lifecycle.*` for your workload.

Because the daemon owns the socket as root, run the CLI from the same root
shell (or with `sudo`). In another root shell:

```sh
export FASTVM_SOCKET=/run/fastvm/fastvm.sock
bin/fastvmctl version
# fastvm 1.2.3-4-ga6f516a        (nearest tag/commit; -dirty for a dirty tree)
# fastvmd 1.2.3-4-ga6f516a
```

The build identity is injected by `make build` from `git describe --tags
--always` (plus `-dirty` when the worktree has changes) and is also served by
`GET /v1/version`. `bin/fastvmctl --version` and `bin/fastvmd --version` print only
that binary's build identity; `fastvm version` prints the CLI and daemon
versions together. Plain `go build` falls back to the VCS revision Go embeds.

Every command also accepts `--socket /path/fastvm.sock` **before** the
subcommand.

---

## 4. Pull a base image

With the daemon running (see §3), pull an OCI image and turn it into a bootable
base disk:

```sh
bin/fastvmctl image pull alpine:3.20
# alpine:3.20  sha256:<64-hex>  8589934592 bytes

bin/fastvmctl image ls
# TAG            DIGEST        SIZE      SOURCE
# alpine:3.20    sha256:...    8192 MiB  registry
```

`run --image` also pulls on demand when the image is not cached, so an explicit
`image pull` is optional.

How it works: fastvm talks directly to the OCI registry (no Docker daemon or
CLI), flattens the image layers into a rootfs, injects the guest agent + init,
and creates a **content-addressed ext4 base disk** in `$DATA_DIR/images/`.
Identical inputs hit the cache; the base disk is read-only and shared by
sandboxes. Compressed layers are cached by digest under
`<data_dir>/images/blobs/` and reused across images, so pulling a new tag that
shares base layers does not re-download them. If one cached blob is deleted, only
that blob is fetched again. A source-index cache (`<data_dir>/images/sources/`)
maps image manifest + agent/init + format + size to the finished disk, so a
repeated `image pull` can return the disk without extracting layers. Registry
credentials are read from
the Docker config keychain (`~/.docker/config.json`), so private registries work
without the Docker daemon.

Optional flag: `--size-gb N` (base disk size; defaults to `storage.root_size_gb`,
8 GiB). To shrink the boot disk, set `storage.root_size_gb: 2`, restart the daemon,
and rebuild images; existing cached images keep their old size until re-pulled.
Remove an image
once no sandbox references it; `--force` overrides the reference check and
deletes the disk plus every tag pointing at its digest:

```sh
bin/fastvmctl image rm alpine:3.20
bin/fastvmctl image rm --force sha256:<hex>
```

For an offline rootfs, the ingest API is still available:

```sh
curl --unix-socket /run/fastvm/fastvm.sock -X POST \
  -H 'content-type: application/json' \
  http://fastvm/v1/images/ingest \
  -d '{"rootfs_dir":"/path/to/rootfs","tag":"imported:1"}'
```

---

## 5. Run, exec, sleep, wake

### Boot a sandbox

```sh
bin/fastvmctl run -d \
  --image alpine:3.20 \
  --name demo \
  --no-auto-sleep \
  -- /bin/sh -c 'while true; do sleep 5; done'
# 82090a0f3d5e7805896a0a8d	Running	demo

bin/fastvmctl ps
# ID            NAME  STATE    IMAGE         IP         VCPUS  MEM
# 82090a0f3d5e  demo  Running  sha256:ba919  10.77.0.2  2      512M
```

`run` boots a real Cloud Hypervisor microVM, waits for the guest agent over
virtio-vsock, then runs the command after `--`. By default it waits and streams
stdout/stderr; use `-d/--detach` to start it as a background workload and print
only the sandbox ID/state/name.
`--vcpus`, `--memory`, `--kernel <release>`, `--publish host:guest[/tcp|udp]`, `--env K=V`,
`--volume host[:guest][:ro]` / `-v` (repeatable), `--no-auto-sleep`,
`-w/--workdir <dir>`, `--auto-publish`, and `--entrypoint <cmd>` (override the
image/fvmfile entrypoint) are available. Auto-published guest TCP listeners on
ports >= 1024 map 1:1 to `127.0.0.1`; privileged ports require an explicit `-p`.
Occupied host ports are skipped and retried, never reassigned. Inspect mappings
with `fastvmctl get`; see [auto-publishing ports](AUTO_PUBLISH.md).

### Host-directory volumes (virtio-fs)

Host paths can be mounted into the guest at create time (read-write by default,
read-only with `:ro`). The host path must be absolute and under one of the
prefixes in `host_volumes.allowed_prefixes` — the demo config ships with an empty
list, which **denies all host mounts**; add e.g. `/tmp` to try it:

```yaml
host_volumes:
  allowed_prefixes: [/tmp]
```

```sh
mkdir -p /tmp/fastvm-share && echo hello > /tmp/fastvm-share/host.txt
bin/fastvmctl run -d --image alpine:3.20 --name shared \
  --volume /tmp/fastvm-share:/mnt/share:ro -- /bin/sleep 3600

bin/fastvmctl exec shared -- cat /mnt/share/host.txt   # hello
```

Each volume gets a `virtiofsd` process and a Cloud Hypervisor `--fs` device; the
daemon passes the mounts over the kernel cmdline and the guest agent-init
mounts them before serving, so `exec` and the shell see them immediately.
Volumes are attached at create time (no hotplug) and survive pause/dormant wake;
virtio-fs DAX is not supported, and mounted host directories must be treated as
trusted input. The same array is accepted by `POST /v1/sandboxes` as
`volumes: [{host_path, guest_path, read_only?}]`.

### Exec into it

```sh
bin/fastvmctl exec demo -- /bin/sh -c 'echo before-sleep > /tmp/demo.txt; cat /etc/motd'
# hello from fastvm
```

### Open an interactive shell

```sh
bin/fastvmctl exec -it demo -- /bin/sh    # Docker-style interactive PTY; Ctrl-D exits
bin/fastvmctl shell demo                  # shorthand for exec -it demo -- /bin/sh
bin/fastvmctl exec -it demo -- /bin/sh   # or any other command
```

`exec -it` allocates a PTY in the guest, puts your terminal into raw mode,
forwards window-size changes, and returns the guest exit code. While the shell
is open the sandbox is kept exec-active, so auto-sleep will not pause it
underneath you. `fastvm shell` is just shorthand for `exec -it <vm> -- /bin/sh`.
This is the easiest way to debug a sandbox (and to run the coding-agent TUIs
like the coding-agent examples interactively).

### Pause, spill RAM to disk, wake

```sh
bin/fastvmctl pause demo --deep      # Running -> Paused -> Dormant
sleep 0.5
bin/fastvmctl get demo | grep -E '"state"|"wake_count"|"snapshot_dir"'
# "state": "Dormant"
# "snapshot_dir": "/tmp/fastvm-demo-data/sandboxes/<id>/memory"
# "wake_count": 0

bin/fastvmctl resume demo
bin/fastvmctl get demo | grep -E '"state"|"wake_count"'
# "state": "Running"
# "wake_count": 1
```

At `Dormant`, Cloud Hypervisor snapshotted the paused VM (guest memory plus
device state in `snapshot_dir`) and the VMM was restarted from that snapshot in a
**paused** state (`--restore`); only touched pages are resident. The VMM and its
virtio-net device queue stay alive, so an inbound packet still lands in the
guest's queue and wakes it (§6). Snapshot restore is Cloud-Hypervisor-version
specific, and any `virtiofsd` processes are restarted before the restore.

Prove state survived the whole cycle:

```sh
bin/fastvmctl exec demo -- cat /tmp/demo.txt
# before-sleep
```

`fastvm pause demo` (without `--deep`) only pauses: RAM stays resident and wake
is faster.

### Observe the lifecycle and clean up

```sh
bin/fastvmctl events -n 12
# TIME          NAME  ID            TYPE   MESSAGE
# 13:33:52.600  demo  <id>          state  sandbox running
# 13:33:52.615  demo  <id>          state  Idle: idle
# 13:33:52.616  demo  <id>          state  Paused: paused:
# 13:33:52.897  demo  <id>          state  Dormant: memory offloaded to disk: .../memory
# 13:33:53.407  demo  <id>          state  Running: woken

bin/fastvmctl rm demo     # stops VMM, deletes TAP/network, volume, memory image, record
bin/fastvmctl ps          # empty
```

Stop the daemon with `Ctrl-C`; a graceful stop first snapshots active
sandboxes to `Dormant` (best-effort; this can take a moment), then stops their
VMMs and removes host-side network state. The next daemon start adopts those
dormant sandboxes, so sandboxes survive a normal restart. Dormant memory
images/volumes remain on disk either way.

### Declarative sandboxes (`.fvmfile`)

Reusable sandboxes can be described in a strict YAML `.fvmfile` and launched
with `fastvmctl run -f`:

```sh
bin/fastvmctl run -f examples/hello.fvmfile
bin/fastvmctl run -f examples/coding-agents/codex.fvmfile -it
```

Examples live under `examples/`:

- [`examples/hello.fvmfile`](../examples/hello.fvmfile) — minimal image + command
- [`examples/coding-agents/codex.fvmfile`](../examples/coding-agents/codex.fvmfile) — OpenAI Codex coding-agent shape
  with env/network/resource settings
- [`examples/coding-agents/claude.fvmfile`](../examples/coding-agents/claude.fvmfile) — Anthropic Claude Code shape
- [`examples/coding-agents/copilot.fvmfile`](../examples/coding-agents/copilot.fvmfile) — GitHub Copilot CLI shape
- [`examples/nodejs.fvmfile`](../examples/nodejs.fvmfile) — Node.js REPL sandbox
- [`examples/docker.fvmfile`](../examples/docker.fvmfile) — Docker-in-VM (dind) sandbox; the pinned kernel has no iptables/bridge modules, so dockerd runs with `--bridge=none --iptables=false` and nested containers use `--network host`

The file supports `image`, `command`, `env` (`${HOST_VAR}` expansion), host
volumes, published ports, network/allowlist, vCPU/memory, and auto-sleep
policy. The config image must be pulled separately if it is not already in the
daemon image store. See [`.fvmfile` configs](FVMFILE.md) for the full field reference.

---

## 6. Auto-sleep and wake-on-network

The flagship behavior: an idle sandbox pauses itself, pages its memory to disk,
and a network request wakes it and is answered.

### Walkthrough

With the daemon from §3 running, run a tiny HTTP server in the guest and publish
it on host port 8080:

```sh
bin/fastvmctl run -d --image alpine:3.20 --name web --publish 8080:80 -- \
  /bin/sh -c 'echo hello from a sleeping VM > /tmp/index.html;
              exec /bin/busybox httpd -f -p 0.0.0.0:80 -h /tmp'
```

> Guest servers must listen on IPv4. Busybox `httpd -p 80` binds IPv6-only, so
> use `-p 0.0.0.0:80` as above.

Wait for auto-sleep, or force it with `fastvm pause web --deep`. With
`hack/config.yaml` the automatic windows are 60 s idle → 10 min pause → 30 min
dormant. `fastvmctl get web` shows `Paused` then `Dormant`:

```sh
fastvmctl get web | grep state      # "state": "Dormant"
curl http://127.0.0.1:8080/      # hello from a sleeping VM
fastvmctl get web | grep -E 'state|wake_count'
# "state": "Running"
# "wake_count": 1
```

The first request is queued in the live host→guest path, the switch/bridge
notification wakes the sandbox, and the request is serviced after resume — the
client gets a normal response, not a reset.

The same path is exercised programmatically by:

```sh
FASTVM_E2E=1 go test ./test/e2e/ -run TestDaemonNetworkWakeRealVM -v
# PASS: real TAP + bridge + guest NIC, paused VM, queued TCP connect wakes it
```

`hack/config.yaml` is only a demo profile. For a real deployment copy it to
`/etc/fastvm/config.yaml` and point `data_dir`/`kernel_path`/`agent_path` at
`/var/lib/fastvm`, then start `fastvmd --config /etc/fastvm/config.yaml` from a
systemd unit (or any service manager) as root.

---

## 7. CLI reference

```text
fastvmctl [--socket PATH] <command> [flags]

version                                     daemon version
image pull [--size-gb N] <tag|digest>          pull an OCI image into the base-disk store
image ls                                    list cached base disks
image rm [--force] <tag|digest>...          delete images and their tags
run    --image <tag|digest> [flags] -- <cmd...>          create a sandbox
       --name, --vcpus, --memory, --kernel <release>, --publish h:g[/tcp|udp], --env K=V, --no-auto-sleep
ls                                          list sandboxes
get <sandbox>                               full sandbox JSON record
exec <sandbox> -- <cmd...>                  run a command in the guest (auto-wakes if asleep)
exec -it <sandbox> -- <cmd...>              same, but with an interactive PTY
shell <sandbox> [-- <cmd...>]               shorthand for exec -it <sandbox> -- /bin/sh
run -f <file.fvmfile>                       create+boot from declarative config
pause <sandbox> [--deep]                    pause; --deep also offloads guest RAM to disk
resume <sandbox>                            resume a paused/dormant sandbox
rm <sandbox>                                delete sandbox + volume + memory image
logs [--source stdout,stderr,console,system] <sandbox>   captured logs (boot, app, lifecycle)
events [-n N]                               recent lifecycle events
```

Sandboxes are addressable by full ID, full name, or any unique ID/name prefix
(Docker-style) — e.g. `fastvmctl get web`, `fastvmctl get 128e1f980d2c`,
`fastvmctl exec 128e1f -- ...`, or `fastvmctl exec api -- ...`. An ambiguous
prefix returns an error listing the matching sandboxes. `FASTVM_SOCKET` or `--socket` selects the daemon socket. The demo
config puts it at `/run/fastvm/fastvm.sock`.

---

## 8. Sandbox lifecycle

| State | Meaning | What's live |
|---|---|---|
| `Running` | workload executing | VMM, TAP, switch port, volume |
| `Idle` | quiet; pause countdown running | same |
| `Paused` | vCPUs halted; memory resident | VMM, TAP, device queue, wake detection |
| `Dormant` | Cloud Hypervisor snapshot restored **paused** (guest memory file-backed, device state in the snapshot dir) | VMM, TAP, device queue, wake detection |
| `Running` | woken by traffic or API | same as Running |
| `Stopped` | deleted | — |

Defaults: idle after **30 s**, pause after **2 min**, offload to disk after
**10 min**, destroy after **24 h** of no activity (all configurable; per-sandbox
`--no-auto-sleep` disables auto-sleep). The idle predicate is debounced over
network payload frames, exec/API, block I/O and CPU; pure ACKs/keepalives don't
count, stdout logging doesn't count, and a disk-I/O-heavy job won't be slept
just because its CPU is low.

---

## 9. Configuration

`fastvmd --config hack/config.yaml` (what `make start` uses); CLI flags override (`--socket`, `--data-dir`,
`--kernel`, `--agent`, `--log-level`). The checked-in demo config is the quickest
starting point; full schema is in `internal/config/config.go`.

| Key | Demo value | Meaning |
|---|---|---|
| `data_dir` | `/tmp/fastvm-demo-data` | state, images, volumes, sandbox dirs |
| `socket_path` | `/run/fastvm/fastvm.sock` | API unix socket |
| `kernel_path` | `/tmp/fastvm/kernels/current` | demo kernel symlink installed by `make assets`; switch with `fastvmctl kernel use` |
| `agent_path` | `bin/fastvm-agent` | guest agent injected into images |
| `vmm.binary` | `cloud-hypervisor` | VMM executable (absolute path or name; falls back to `cloud-hypervisor-static`, override with `FASTVM_CLOUD_HYPERVISOR`) |
| `vmm.virtiofsd` | `virtiofsd` | virtio-fs server for host volumes (falls back to `/usr/lib/virtiofsd`; override with `FASTVM_VIRTIOFSD`) |
| `host_volumes.allowed_prefixes` | `[]` | absolute host paths allowed as `--volume` shares; empty/absent denies all host mounts (distinct from `storage.*`, which configures writable storage) |
| `init_path` | empty | custom `/init`; empty uses the built-in script |
| `lifecycle.idle_window` | `60s` | Running→Idle debounce |
| `lifecycle.pause_after` | `10m` | Idle→Paused |
| `lifecycle.offload_after` | `30m` | Paused→Dormant |
| `lifecycle.destroy_after` | `0s` (demo) / `24h` | auto-delete (0 = never) |
| `lifecycle.on_workload_exit` | `idle` | idle / pause / offload / stop |
| `lifecycle.idle_detection.cpu_threshold_pct` | `1.0` | CPU must be below this to sleep (veto) |
| `lifecycle.idle_detection.cpu_wake_pct` | `5.0` | CPU spike that returns Idle→Running |
| `lifecycle.idle_detection.block_io_bytes` | `65536` | block I/O budget per window |
| `lifecycle.idle_detection.ignore_ports` | `[]` | ports that never count/wake (host probes) |
| `memory.offload_mode` | `file` | `file` or `never` (uffd/swap not implemented) |
| `lifecycle.wake_policy.max_wakes_per_minute` | `6` | scan protection per sandbox |
| `lifecycle.wake_policy.coalesce_window` | `100ms` | burst coalescing |
| `storage.cow` | `reflink` | `reflink` on XFS/btrfs, `copy` fallback |
| `storage.root_size_gb` | `1` | base/root disk size used for new image builds; images that need more fail (existing images must be re-pulled) |
| `storage.discard_on_destroy` | `true` | remove the writable volume with the sandbox |
| `network.datapath.mode` | `ebpf` | `ebpf` (default; automatic `bridge` fallback without CAP_BPF) or `bridge` |
| `network.dns` | host resolvers | nameserver overrides (non-loopback IPv4); empty mirrors the host `/etc/resolv.conf`, like Docker |
| `network.search` | host search domains | search-domain overrides; empty mirrors the host |

Guest `/etc/hostname`, `/etc/hosts`, and `/etc/resolv.conf` are written by the
agent on every boot. Base disks built with an older agent must be rebuilt to
pick up the new identity-file writer.

Data directory layout (created automatically):

```text
$DATA_DIR/
  state.db                        bbolt: sandbox records + event log
  images/<hex>.img + .json        content-addressed base disks
  images/tags/...                 tag -> digest index
  volumes/<sandbox-id>.img        writable CoW runtime volumes
  sandboxes/<sandbox-id>/         serial.log, VMM logs, vmm-api.sock, vsock.sock,
                                  memory/{config.json,memory-ranges,state.json} when dormant
  run/                            VMM working dirs
```

VMM boot failures include the Cloud Hypervisor stderr tail in the error/event;
the guest console is captured in `sandboxes/<id>/serial.log` by CH's
file-backed serial backend. Dormant snapshots live in the same sandbox
directory (`memory/`); they restore only with the same Cloud Hypervisor version,
so pin the VMM per host.

---

## 10. Testing

```sh
make test                                    # unit tests
make test-race                               # race detector
make e2e                                     # real-VM e2e (needs KVM + assets)
FASTVM_NET_TESTS=1 go test ./internal/network/...   # TAP/bridge/nft tests
```

The privileged tests skip cleanly when their prerequisites are missing
(`/dev/kvm`, KVM, Cloud Hypervisor, virtiofsd, assets,
`CAP_NET_ADMIN`/`CAP_BPF`). Cloud Hypervisor / virtiofsd paths for the tests can
be overridden with `FASTVM_CLOUD_HYPERVISOR` / `FASTVM_VIRTIOFSD`.

How the e2e suites were verified in development:

| Suite | Command | Result |
|-------|---------|--------|
| Unit | `go test ./...` | green |
| Race | `go test -race ./...` | green |
| Real VM lifecycle | `FASTVM_E2E=1 go test ./test/e2e/ -run TestDaemonLifecycleRealVM` | PASS ~1.7 s: boot→agent→exec→pause→dormant snapshot/restore→wake→exec→delete |
| CLI/API over unix socket | `... -run TestCLIOverUnixSocket` | PASS ~2.1 s: run→ls→exec→deep sleep→wake→events→rm |
| Wake on network (real TAP) | `... -run TestDaemonNetworkWakeRealVM` | PASS ~1.8 s: boot with guest NIC, pause, TCP connect is queued and wakes the VM |
| Daemon binary | `... -run TestDaemonBinarySmoke` | PASS ~3 s: fastvmd starts with the demo config, CLI version/image ls/ls, clean SIGTERM |
| Driver-level checks | `go test ./internal/vmm/cloudhypervisor/` | unit: launch args (virtio-fs/net/vsock), CH API client, volume validation; real VM paths are covered by the lifecycle/CLI e2e rows above |
| Bridge/TAP/nft | `FASTVM_NET_TESTS=1 go test ./internal/network/...` | PASS ~0.9 s (isolated network namespace) |

eBPF load/attach tests run only with `CAP_BPF`; without it the daemon logs the
fallback and uses the Linux bridge.

---

## 11. Troubleshooting

| Symptom | Fix |
|---|---|
| `cannot reach fastvmd at ...` | Wrong socket. Export `FASTVM_SOCKET` or pass `--socket` before the subcommand; check the daemon is running. The demo config uses `/run/fastvm/fastvm.sock`. |
| `permission denied` on the socket | The daemon created it as root with mode 0600; run the CLI as root too (same root shell) or run the daemon as the user that will use it. |
| `operation not permitted` creating TAP/bridge/nft | The daemon needs `CAP_NET_ADMIN`: run `fastvmd` as root. |
| `cloud-hypervisor: executable file not found in $PATH` | Install Cloud Hypervisor v54+ (or `cloud-hypervisor-static`) and make sure it is on the daemon's `PATH`, or set `vmm.binary` / `FASTVM_CLOUD_HYPERVISOR` to its absolute path. |
| `virtiofsd: executable file not found` | Install virtiofsd ≥ 1.10 (the distro package usually installs `/usr/lib/virtiofsd`; the daemon falls back to it) or set `vmm.virtiofsd` / `FASTVM_VIRTIOFSD`. Only needed for host-directory volumes. |
| `volume N: host_path "..." is outside the allowed host prefixes` | Add the absolute host path prefix to `host_volumes.allowed_prefixes` in the daemon config (empty denies all host mounts), or drop the `--volume` flag. |
| A dormant sandbox fails to restore after upgrading | The Cloud Hypervisor snapshot format is version-specific: restore with the same CH version (and CPU/disk/vsock paths). Delete the sandbox or re-create it after a VMM upgrade. |
| Boot fails / sandbox goes `Failed` | Check `/dev/kvm` permissions, `kernel_path`, and the VMM stderr tail in the event plus `sandboxes/<id>/serial.log`. |
| `image not found` on `run` | `fastvm image ls` to see tags; pull or ingest first. |
| `fastvm image pull` fails to reach a registry | Check network/DNS and registry credentials (`~/.docker/config.json`). Private registries use the same keychain; local HTTP registries are not enabled by default. |
| `exec: "<tool>": executable file not found in $PATH` | Base disks built before the PATH fix contain an old agent/init. Rebuild the base image or call through a shell: `fastvm exec <vm> -- /bin/sh -c '<tool> --help'`. |
| `fastvm shell`: `unknown op "shell"` | The image was built with an older guest agent. Rebuild the base disk and recreate the sandbox. |
| A guest command (e.g. `claude --help`) hangs with no output | The image predates guest-entropy seeding: `getrandom()` blocks until the microVM CRNG initializes. Rebuild the base disk and recreate the sandbox. |
| Build on an old guest kernel fails to mount ext4 | Host `mke2fs` defaults can enable newer features; pass a matching kernel or pin features in the builder. |
| Daemon log says `eBPF switch unavailable; using Linux bridge` | Expected without `CAP_BPF`/BTF/verifier support; the bridge + AF_PACKET path is the fallback. Set `network.datapath.mode: bridge` to silence the attempt. |
| `--publish` port refuses connections on localhost | The guest server must listen on IPv4 (`0.0.0.0`). Busybox `httpd -p 80` binds IPv6-only; use `-p 0.0.0.0:80`. |
| Daemon won't start, `state.db` timeout | Another `fastvmd` is using the same `data_dir` (bbolt exclusive lock). Stop it first. |
| Sandbox never becomes Idle | Check `fastvm events` for the Idle reason (`quiet for ...`) and whether it was created with `--no-auto-sleep`. |

---

## 12. What's next

Read [DESIGN.md](DESIGN.md) for the full design, roadmap, and intentional
non-goals. Practical v0.1 gaps and behavior worth knowing are collected in
[LIMITATIONS.md](LIMITATIONS.md).
