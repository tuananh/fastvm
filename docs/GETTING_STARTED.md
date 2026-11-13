# fastvm — Getting Started

A complete walkthrough: build the binaries, start the daemon with a ready-made
config, build a base disk from a Dockerfile, boot a real microVM, exec into it,
pause it, spill guest RAM to disk, and wake it from a network request.

Design and roadmap: [DESIGN.md](DESIGN.md). CLI/API/limits summary: [README.md](../README.md).

---

## 1. Requirements

| Requirement | Notes |
|---|---|
| Linux x86_64 + KVM | `/dev/kvm` must be accessible. |
| **root / `CAP_NET_ADMIN`** | The daemon creates TAP devices, a Linux bridge, and nftables rules. Run `fastvmd` as root (or grant it `CAP_NET_ADMIN`). Required for booting sandboxes and for `--publish` ports. |
| Firecracker | Tested with v1.17: `firecracker --version`. |
| Go 1.24+ | Only to build from source. |
| Kernel image | `make assets` downloads a Firecracker-compatible `vmlinux.bin`; or point `kernel_path` at your own pinned kernel. |
| Docker (optional) | Only for `fastvm image build` and template image builds (Dockerfile → OCI → ext4). |
| e2fsprogs (`mke2fs`), `nft` | Base disk creation and DNAT rules. |

> The public sandbox this repo was developed in runs as an unprivileged user, so
> it cannot create TAP/bridge/nftables. Run the walkthrough below on a normal
> KVM host as root (or a VM with the required capabilities). Unit tests run
> unprivileged; the gated e2e tests handle their own isolation.

---

## 2. Build

```sh
git clone <this repo> fastvm && cd fastvm
make build            # bin/fastvm, bin/fastvmd, bin/fastvm-agent
make test             # unit tests (no KVM/root needed)
make assets           # kernel + alpine rootfs for demos/e2e (cached, gitignored)
sudo make install     # /usr/local/bin (or: make install PREFIX=$HOME/.local)
```

`make assets` prints the paths it downloaded under `test/e2e/assets/`.

---

## 3. Start the daemon

A ready-to-use demo config is checked in at [hack/config.yaml](../hack/config.yaml).
It uses `/tmp` for throwaway state so you can try fastvm without installing
anything system-wide:

```yaml
data_dir: /tmp/fastvm/data
socket_path: /run/fastvm/fastvm.sock
kernel_path: test/e2e/assets/vmlinux.bin
agent_path: bin/fastvm-agent
lifecycle: {idle_window: 60s, pause_after: 10m, offload_after: 30m, destroy_after: 0s}
# ... see the file for the idle/memory/wake/switch sections
```

Its relative `kernel_path`/`agent_path` are resolved from the directory you
start the daemon in, so run from the repo root. For a real installation, copy it
to `/etc/fastvm/config.yaml` and point the paths at `/var/lib/fastvm/...`.

Start the daemon as root (it runs in the foreground; `Ctrl-C` stops it):

```sh
make build                 # once, as your user
sudo make start            # runs bin/fastvmd -config hack/config.yaml
```

Or, in a root shell, the equivalent:

```sh
sudo -s
cd /path/to/fastvm
bin/fastvmd -config hack/config.yaml
```

What happens:

- The daemon creates `$DATA_DIR` (`/tmp/fastvm-demo-data`), writes
  `/run/fastvm/fastvm.sock` (mode 0600) and serves the HTTP/JSON API.
- With `switch.mode: ebpf` (the default) the daemon loads the eBPF L2 switch.
  TAP ingress sees guest→host frames and redirects them to the uplink veth;
  uplink ingress sees host→guest frames, redirects them into the sleeping
  sandbox's TAP and emits a ringbuf wake event. Without `CAP_BPF`/BTF it logs
  `eBPF switch unavailable; using Linux bridge` and switches to the bridge +
  AF_PACKET wake path.
- On the first sandbox it creates TAP devices, the uplink veth pair, `/30`
  gateway addresses/static neighbors and nftables DNAT rules (bridge mode also
  creates `bridge fastvm0`).
- `hack/config.yaml` uses generous windows (60 s idle, 10 min pause, 30 min
  dormant) so a coding-agent VM is not paused a few seconds after you stop
  typing. `hack/demo-wake.sh` derives a shortened copy (3 s / 2 s / 8 s) for the
  wake demo; tune `lifecycle.*` for your workload.

Because the daemon owns the socket as root, run the CLI from the same root
shell (or with `sudo`). In another root shell:

```sh
export FASTVM_SOCKET=/run/fastvm/fastvm.sock
bin/fastvm version
# fastvm 0.1.0
```

Every command also accepts `-socket /path/fastvm.sock` **before** the
subcommand.

---

## 4. Build a base disk from a Dockerfile

The repo ships an example Dockerfile at
[hack/Dockerfile.example](../hack/Dockerfile.example). With the daemon running
(see §3), build it into a bootable base disk with one command:

```sh
make image
# demo:latest  sha256:<64-hex>  8589934592 bytes

bin/fastvm image ls
# TAG          DIGEST        SIZE      SOURCE
# (a disk whose tag moved to a newer build is shown as "-" and remains
#  addressable by digest; tag -> digest comes from images/tags)
# demo:latest  sha256:...    8192 MiB  dockerfile
```

`make image` runs `bin/fastvm -socket /run/fastvm/fastvm.sock image build -f
hack/Dockerfile.example -t demo:latest hack` (override with
`make image FASTVM_SOCKET=/path/to/fastvm.sock`). Use any Dockerfile/context
directly the same way:

```sh
bin/fastvm image build -f /path/to/Dockerfile -t myapp:latest /path/to/context
```

How it works: `fastvm image build` runs `DOCKER_BUILDKIT=1 docker build` (unique temp
tag), exports the container rootfs, injects the guest agent + init, and creates
a **content-addressed ext4 base disk** in `$DATA_DIR/images/`. Identical inputs
hit the cache; the base disk is read-only and shared by sandboxes.

Optional flag: `-size-gb N` (base disk size, default 8 GiB). Remove an image
once no sandbox references it; `--force` overrides the reference check and
deletes the disk plus every tag pointing at its digest:

```sh
bin/fastvm image rm demo:latest
bin/fastvm image rm --force sha256:<hex>
```

Without Docker you can import an unpacked rootfs through the API:

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
bin/fastvm run \
  --image demo:latest \
  --name demo \
  --no-auto-sleep \
  -- /bin/sh -c 'while true; do sleep 5; done'
# 82090a0f3d5e7805896a0a8d	Running	demo

bin/fastvm ls
# ID            NAME  STATE    IMAGE         IP         VCPUS  MEM
# 82090a0f3d5e  demo  Running  sha256:ba919  10.77.0.2  2      512M
```

`run` boots a real Firecracker microVM, waits for the guest agent over
virtio-vsock, then starts the command after `--` as a detached workload.
`--vcpus`, `--memory`, `--publish host:guest[/tcp|udp]`, `--env K=V`, and
`--no-auto-sleep` are available.

### Exec into it

```sh
bin/fastvm exec demo -- /bin/sh -c 'echo before-sleep > /tmp/demo.txt; cat /etc/motd'
# hello from fastvm
```

### Open an interactive shell

```sh
bin/fastvm exec -it demo -- /bin/sh    # Docker-style interactive PTY; Ctrl-D exits
bin/fastvm shell demo                  # shorthand for exec -it demo -- /bin/sh
bin/fastvm exec -it demo -- /bin/bash  # or any other command
```

`exec -it` allocates a PTY in the guest, puts your terminal into raw mode,
forwards window-size changes, and returns the guest exit code. While the shell
is open the sandbox is kept exec-active, so auto-sleep will not pause it
underneath you. `fastvm shell` is just shorthand for `exec -it <vm> -- /bin/sh`.
This is the easiest way to debug a sandbox (and to run the coding-agent TUIs
from the templates interactively).

### Pause, spill RAM to disk, wake

```sh
bin/fastvm pause demo --deep      # Running -> Paused -> Dormant
sleep 0.5
bin/fastvm get demo | grep -E '"state"|"wake_count"|"snapshot_dir"'
# "state": "Dormant"
# "snapshot_dir": "/tmp/fastvm-demo-data/sandboxes/<id>/memory"
# "wake_count": 0

bin/fastvm resume demo
bin/fastvm get demo | grep -E '"state"|"wake_count"'
# "state": "Running"
# "wake_count": 1
```

At `Dormant`, guest RAM was handed to a Firecracker memory file and the VMM was
restored back into a **paused** state; only touched pages are resident. The VMM
and its virtio-net device queue stay alive, so an inbound packet still lands in
the guest's queue and wakes it (§6).

Prove state survived the whole cycle:

```sh
bin/fastvm exec demo -- cat /tmp/demo.txt
# before-sleep
```

`fastvm pause demo` (without `--deep`) only pauses: RAM stays resident and wake
is faster.

### Observe the lifecycle and clean up

```sh
bin/fastvm events -n 12
# TIME          NAME  ID            TYPE   MESSAGE
# 13:33:52.600  demo  <id>          state  sandbox running
# 13:33:52.615  demo  <id>          state  Idle: idle
# 13:33:52.616  demo  <id>          state  Paused: paused:
# 13:33:52.897  demo  <id>          state  Dormant: memory offloaded to disk: .../memory
# 13:33:53.407  demo  <id>          state  Running: woken

bin/fastvm rm demo     # stops VMM, deletes TAP/network, volume, memory image, record
bin/fastvm ls          # empty
```

Stop the daemon with `Ctrl-C`; a graceful stop first snapshots active
sandboxes to `Dormant` (best-effort; this can take a moment), then stops their
VMMs and removes host-side network state. The next daemon start adopts those
dormant sandboxes, so sandboxes survive a normal restart. Dormant memory
images/volumes remain on disk either way.

### Coding-agent templates

Three templates are baked into the CLI:

| Template | Agent | Host env forwarded |
|---|---|---|
| `claude` | Anthropic Claude Code (`@anthropic-ai/claude-code`) | `ANTHROPIC_API_KEY` |
| `codex` | OpenAI Codex CLI (`@openai/codex`) | `OPENAI_API_KEY` |
| `copilot` | GitHub Copilot CLI (`@github/copilot`) | `GH_TOKEN` or `GITHUB_TOKEN` |

Templates only reference prebuilt image tags (`fastvm/<name>:v6`). The
Dockerfiles live in [`hack/templates/`](../hack/templates/) and are built once
against the running daemon (Docker + network required):

```sh
sudo make template:copilot     # one image (or template:claude / template:codex)
sudo make template-images      # or all three

bin/fastvm template list
# -d: stdout is exactly the sandbox name, for scripts
vm=$(bin/fastvm template start copilot -d)
bin/fastvm exec -it "$vm" -- copilot

# without -d you also get the ID, template and suggested commands
bin/fastvm template start claude
```

`make template-images` (or an individual `template:<name>`) can be reused as a
CI/pipeline step (it accepts `FASTVM_SOCKET=...`); `template start` does not build Dockerfiles implicitly and
fails with a clear message if the referenced image is missing.

API keys are forwarded from your environment when set (or use
`--env K=V` explicitly). `--name`, `--vcpus`, `--memory` and
`--no-auto-sleep` override template defaults. For interactive agent TUIs use
`fastvm shell <vm>` and run the agent inside it; `fastvm exec` remains
non-interactive.

---

## 6. Auto-sleep and wake-on-network

The flagship behavior: an idle sandbox pauses itself, pages its memory to disk,
and a network request wakes it and is answered. The ready-made demo does all of
it in one command (requires root, Docker, and the model above):

```sh
sudo make demo
# or: sudo ./hack/demo-wake.sh
```

Under the hood it uses [hack/config.yaml](../hack/config.yaml) with a shortened
copy of its auto-sleep windows (3 s / 2 s / 8 s) and `/tmp` state, plus
[hack/demo-wake.sh](../hack/demo-wake.sh), then prints:

```text
== 1. build a base disk from a Dockerfile ==
== 2. boot a sandbox and publish port 8080 -> guest 80 ==
== 3. curl while the sandbox is running ==
hello-from-sleeping-vm
== 4. wait for the daemon to auto-sleep it (idle -> paused -> dormant) ==
   state: Dormant  wake_count: 0
== 5. curl the sleeping sandbox: the packet wakes it, then it answers ==
hello-from-sleeping-vm
   curl wall: 0.01 s
   state: Running  wake_count: 1
```

### Manual version

With the daemon from §3 running, run a tiny HTTP server in the guest and publish
it on host port 8080:

```sh
bin/fastvm run --image demo:latest --name web --publish 8080:80 -- \
  /bin/sh -c 'echo hello from a sleeping VM > /tmp/index.html;
              exec /bin/busybox httpd -f -p 0.0.0.0:80 -h /tmp'
```

> Guest servers must listen on IPv4. Busybox `httpd -p 80` binds IPv6-only, so
> use `-p 0.0.0.0:80` as above.

Wait for auto-sleep, or force it with `fastvm pause web --deep`. With
`hack/config.yaml` the automatic windows are 60 s idle → 10 min pause → 30 min
dormant; the demo script shortens them to 3 s → 2 s → 8 s. `fastvm get web`
shows `Paused` then `Dormant`:

```sh
fastvm get web | grep state      # "state": "Dormant"
curl http://127.0.0.1:8080/      # hello from a sleeping VM
fastvm get web | grep -E 'state|wake_count'
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
`/var/lib/fastvm`, then start `fastvmd -config /etc/fastvm/config.yaml` from a
systemd unit (or any service manager) as root.

---

## 7. CLI reference

```text
fastvm [-socket PATH] <command> [flags]

version                                     daemon version
image build [-f Dockerfile] [-t tag] [-size-gb N] [context]  base disk from Dockerfile
image ls                                    list cached base disks
image rm [--force] <tag|digest>...          delete images and their tags
run    --image <tag|digest> [flags] -- <cmd...>          create a sandbox
       --name, --vcpus, --memory, --publish h:g[/tcp|udp], --env K=V, --no-auto-sleep
ls                                          list sandboxes
get <sandbox>                               full sandbox JSON record
exec <sandbox> -- <cmd...>                  run a command in the guest (auto-wakes if asleep)
exec -it <sandbox> -- <cmd...>              same, but with an interactive PTY
shell <sandbox> [-- <cmd...>]               shorthand for exec -it <sandbox> -- /bin/sh
template list                               list baked-in coding-agent templates
template start <t> [-d] [--name N] [--env K=V] [--vcpus N] [--memory MiB]
                                            boot a template sandbox; -d prints the name only
pause <sandbox> [--deep]                    pause; --deep also offloads guest RAM to disk
resume <sandbox>                            resume a paused/dormant sandbox
rm <sandbox>                                delete sandbox + volume + memory image
events [-n N]                               recent lifecycle events
```

Sandboxes are addressable by full ID, full name, or any unique ID prefix
(Docker-style) — e.g. `fastvm get web`, `fastvm get 128e1f980d2c`, or
`fastvm exec 128e1f -- ...`. An ambiguous prefix returns an error listing the
matching IDs. `FASTVM_SOCKET` or `-socket` selects the daemon socket. The demo
config puts it at `/run/fastvm/fastvm.sock`.

---

## 8. Sandbox lifecycle

| State | Meaning | What's live |
|---|---|---|
| `Running` | workload executing | VMM, TAP, switch port, volume |
| `Idle` | quiet; pause countdown running | same |
| `Paused` | vCPUs halted; memory resident | VMM, TAP, device queue, wake detection |
| `Dormant` | guest RAM in a memory file (mmap restore), VMM alive **paused** | VMM, TAP, device queue, wake detection |
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

`fastvmd -config hack/config.yaml` (what `make start` uses); CLI flags override (`-socket`, `-data-dir`,
`-kernel`, `-agent`, `-log-level`). The checked-in demo config is the quickest
starting point; full schema is in `internal/config/config.go`.

| Key | Demo value | Meaning |
|---|---|---|
| `data_dir` | `/tmp/fastvm-demo-data` | state, images, volumes, sandbox dirs |
| `socket_path` | `/run/fastvm/fastvm.sock` | API unix socket |
| `kernel_path` | `test/e2e/assets/vmlinux.bin` | pinned guest kernel (relative to cwd) |
| `agent_path` | `bin/fastvm-agent` | guest agent injected into images |
| `init_path` | empty | custom `/init`; empty uses the built-in script |
| `lifecycle.idle_window` | `60s` | Running→Idle debounce |
| `lifecycle.pause_after` | `10m` | Idle→Paused |
| `lifecycle.offload_after` | `30m` | Paused→Dormant |
| `lifecycle.destroy_after` | `0s` (demo) / `24h` | auto-delete (0 = never) |
| `lifecycle.on_workload_exit` | `idle` | idle / pause / offload / stop |
| `idle.cpu_threshold_pct` | `1.0` | CPU must be below this to sleep (veto) |
| `idle.cpu_wake_pct` | `5.0` | CPU spike that returns Idle→Running |
| `idle.block_io_bytes` | `65536` | block I/O budget per window |
| `idle.ignore_ports` | `[]` | ports that never count/wake (host probes) |
| `memory.offload_mode` | `file` | `file` or `never` (uffd/swap not implemented) |
| `wake.max_wakes_per_minute` | `6` | scan protection per sandbox |
| `wake.coalesce_window` | `100ms` | burst coalescing |
| `volume.cow` | `reflink` | `reflink` on XFS/btrfs, `copy` fallback |
| `switch.mode` | `ebpf` | `ebpf` (default; automatic `bridge` fallback without CAP_BPF) or `bridge` |
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
  sandboxes/<sandbox-id>/         serial.log, VMM logs, api/vsock sockets,
                                  memory/{mem,vmstate} when dormant
  run/                            VMM working dirs
```

VMM boot failures include the VMM stderr tail in the error/event; the guest
console is in `sandboxes/<id>/serial.log` (serial is not reattached after a
snapshot restore — a Firecracker limitation; exec/vsock/disk state are
unaffected).

---

## 10. Testing

```sh
make test                                    # unit tests
make test-race                               # race detector
make e2e                                     # real-VM e2e (needs KVM + assets)
FASTVM_NET_TESTS=1 go test ./internal/network/...   # TAP/bridge/nft tests
FASTVM_DOCKER_TESTS=1 go test ./internal/image/...  # Dockerfile build test
```

The privileged tests skip cleanly when their prerequisites are missing
(`/dev/kvm`, Firecracker, docker, assets, `CAP_NET_ADMIN`/`CAP_BPF`).

How the e2e suites were verified in development:

| Suite | Command | Result |
|-------|---------|--------|
| Unit | `go test ./...` | green |
| Race | `go test -race ./...` | green |
| Real VM lifecycle | `FASTVM_E2E=1 go test ./test/e2e/ -run TestDaemonLifecycleRealVM` | PASS ~1.7 s: boot→agent→exec→pause→dormant snapshot/restore→wake→exec→delete |
| CLI/API over unix socket | `... -run TestCLIOverUnixSocket` | PASS ~2.1 s: run→ls→exec→deep sleep→wake→events→rm |
| Wake on network (real TAP) | `... -run TestDaemonNetworkWakeRealVM` | PASS ~1.8 s: boot with guest NIC, pause, TCP connect is queued and wakes the VM |
| Daemon binary | `... -run TestDaemonBinarySmoke` | PASS ~3 s: fastvmd starts with the demo config, CLI version/image ls/ls, clean SIGTERM |
| Driver-level snapshot | `FASTVM_E2E=1 go test ./internal/vmm/firecracker/ -run TestE2EFirecracker` | PASS ~6 s: pause freezes vCPUs, snapshot→restore paused, guest state intact |
| Docker build | `FASTVM_DOCKER_TESTS=1 go test ./internal/image/ -run TestBuildDocker` | PASS ~2.7 s: Dockerfile→rootfs→ext4, debugfs-verified contents |
| Bridge/TAP/nft | `FASTVM_NET_TESTS=1 go test ./internal/network/...` | PASS ~0.9 s (isolated network namespace) |

eBPF load/attach tests run only with `CAP_BPF`; without it the daemon logs the
fallback and uses the Linux bridge.

---

## 11. Troubleshooting

| Symptom | Fix |
|---|---|
| `cannot reach fastvmd at ...` | Wrong socket. Export `FASTVM_SOCKET` or pass `-socket` before the subcommand; check the daemon is running. The demo config uses `/run/fastvm/fastvm.sock`. |
| `permission denied` on the socket | The daemon created it as root with mode 0600; run the CLI as root too (same root shell) or run the daemon as the user that will use it. |
| `operation not permitted` creating TAP/bridge/nft | The daemon needs `CAP_NET_ADMIN`: run `fastvmd` as root. |
| `firecracker: executable file not found in $PATH` | Install Firecracker and make sure the daemon's PATH includes it. |
| Boot fails / sandbox goes `Failed` | Check `/dev/kvm` permissions, `kernel_path`, and the VMM stderr tail in the event plus `sandboxes/<id>/serial.log`. |
| `image not found` on `run` | `fastvm image ls` to see tags; build or ingest first. |
| `fastvm image build` fails with docker error | Docker daemon must be reachable (and have network for FROM/RUN). Use the ingest API for offline rootfs. |
| `exec: "<tool>": executable file not found in $PATH` | Base disks built before the PATH fix contain an old agent/init. Rebuild the image (templates now build `v6`) or call through a shell: `fastvm exec <vm> -- /bin/sh -c '<tool> --help'`. |
| `fastvm shell`: `unknown op "shell"` | The image was built with an older guest agent. Rebuild the base disk (templates now build `v6`) and recreate the sandbox. |
| A guest command (e.g. `claude --help`) hangs with no output | The image predates guest-entropy seeding: `getrandom()` blocks until the microVM CRNG initializes. Rebuild the base disk (templates now build `v6`) and recreate the sandbox. |
| Build on an old guest kernel fails to mount ext4 | Host `mke2fs` defaults can enable newer features; pass a matching kernel or pin features in the builder. |
| Daemon log says `eBPF switch unavailable; using Linux bridge` | Expected without `CAP_BPF`/BTF/verifier support; the bridge + AF_PACKET path is the fallback. Set `switch.mode: bridge` to silence the attempt. |
| `--publish` port refuses connections on localhost | The guest server must listen on IPv4 (`0.0.0.0`). Busybox `httpd -p 80` binds IPv6-only; use `-p 0.0.0.0:80`. |
| Daemon won't start, `state.db` timeout | Another `fastvmd` is using the same `data_dir` (bbolt exclusive lock). Stop it first. |
| Sandbox never becomes Idle | Check `fastvm events` for the Idle reason (`quiet for ...`) and whether it was created with `--no-auto-sleep`. |

---

## 12. What's next

Read [DESIGN.md](DESIGN.md) for the full design, roadmap, and intentional
non-goals. Practical v0.1 gaps and behavior worth knowing are collected in
[LIMITATIONS.md](LIMITATIONS.md).
