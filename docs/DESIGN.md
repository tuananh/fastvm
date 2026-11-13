# fastvm — Design and Roadmap

| Field | Value |
|---|---|
| **Status** | Draft v0.5 |
| **Date** | 2026-11-07 |
| **Owner** | lead |
| **Stack** | Go (control plane) + Firecracker (VMM) |
| **Repo** | `/home/anh/Code/fastvm` |

> **Rev 5** — image pipeline (Dockerfile → base disk via BuildKit), CoW runtime
> volumes, and an eBPF L2 switch that forwards frames, isolates sandboxes,
> counts traffic, and emits the wake event. Wake path and offload model from
> Rev 4 are unchanged: packets flow into the guest's own queue; no parking,
> replay, or userspace fault handler.

---

## TL;DR

`fastvm` is a single-host, Go-based micro-VM sandbox runtime. It boots Linux
sandboxes in ~100 ms and **manages their lifecycle without an operator**:
sandboxes detect idleness, pause themselves, hand their guest memory to a
file-backed image when they stay asleep, and **wake themselves on inbound
network traffic**.

The wake path stays inside the VM's own network stack — nothing is proxied and
nothing is replayed by fastvm:

```
inbound frame → eBPF L2 switch → external side of the sandbox veth → TAP
              → virtio-net device queue (host → guest)
              → first frame wakes the sandbox → guest consumes the queue → eth0
```

Two sleep levels, both with a **live device queue**:

| Level | What happens | Host memory | Wake latency |
|-------|--------------|-------------|--------------|
| **Paused** | vCPUs halted; TAP + virtio-net device queue stay attached; frames queue in the host→guest path until the guest resumes | full guest RAM | ~50–150 ms |
| **Dormant** | same paused VM, but guest RAM is handed to a Firecracker memory file and restored file-backed (mmap, demand paging); the VMM process and device queue stay alive, so an incoming frame is still enqueued (only touched pages fault in) | reclaimable page cache + VMM overhead | ~50–300 ms |

Restore is not the expensive part: Firecracker loads a paused memory file in
tens of milliseconds, so a sandbox can go dormant quickly and waking it is a
vCPU resume plus demand paging. That is what makes the pause/wake cycle feel
instant.

The difference from a snapshot-and-exit design: **a sleeping sandbox is never
process-dead**. The element that receives packets — the VMM's virtio-net device
— is still there, so the packet lands in the queue the guest will consume. Deep
sleep with the VMM exited is explicitly post-v0.1 (§4) because it has no live
queue and would need a different wake mechanism.

Supporting machinery: base disks are **built from Dockerfiles** (BuildKit → OCI
→ ext4, content-addressed and cached) and every sandbox runs on a **CoW runtime
volume** cloned from that base in milliseconds. Networking is an **eBPF L2
switch**: frames are forwarded to the right sandbox port, per-port isolation and
anti-spoofing are enforced in the kernel, counters feed activity detection, and
a frame for a sleeping port emits the wake event while still flowing into that
sandbox's virtio-net queue.

---

## 1. Problem

Running untrusted or resource-heavy workloads inside containers shares a kernel
and a blast radius. Full VMs are the right isolation boundary, but the existing
tooling exposes primitives, not a product: you get a VMM binary (Firecracker,
Cloud Hypervisor), not sandbox lifecycle. Operators end up:

- babysitting idle sandboxes that burn RAM and CPU, or killing them and losing
  in-memory state;
- writing bespoke pause/resume/snapshot scripts and network plumbing;
- choosing between keeping everything warm (expensive) and cold-booting on
  every request (slow, stateful workloads impossible);
- turning Dockerfiles into bootable VM disks and cloning them per sandbox
  without slow full copies.

The desired behavior is a **self-managing sandbox**: it knows when nobody is
using it, it parks itself, it gives back its memory, and it comes back the
moment a packet arrives — with the packet already waiting in its own network
queue.

---

## 2. Users & workload assumptions

**Assumed (confirm before M1/M2):**

- **Primary user:** a single operator/developer on one Linux/KVM host, using a
  CLI and a local HTTP API (scriptable by other tools, e.g. AI agents).
- **Primary workloads:** (a) long-running dev/preview servers reached over TCP,
  (b) batch jobs that finish and should then sleep, (c) interactive exec
  sessions (shell/agent commands).
- **Guest:** Linux x86_64, from an OCI image rootfs, kernel supplied by fastvm
  (pinned), minimal init + fastvm guest agent.
- **Images:** base disks are built from Dockerfiles (`fastvm image build`), cached by
  digest, and shared read-only; each sandbox gets a CoW runtime volume cloned
  from one.
- **Networking:** each sandbox gets a virtual L2 port on a per-host eBPF switch;
  by default it can reach the host/uplink but not other sandboxes.
- **Trust model v0.1:** single-operator host, but *guest code is untrusted with
  respect to the host*: KVM isolation, minimal device model, no host mounts.
  Hardened multi-tenant SaaS is post-v0.1.

If any of these is wrong (e.g. multi-tenant SaaS, Windows guests, GPU
workloads), say so now — it changes the networking and threat-model milestones.

---

## 3. Goals

- **G1 — Real isolation.** One microVM per sandbox, KVM, minimal device model,
  no shared kernel with the host or other sandboxes.
- **G2 — Fast start.** Cold boot to workload-ready: **p50 ≤ 150 ms,
  p95 ≤ 300 ms** on the reference host (Firecracker-class).
- **G3 — Self-managing lifecycle.** No manual pause/stop required. A state
  machine driven by a debounced idle predicate (network, exec/API, block I/O,
  CPU), workload events, TTLs, and memory pressure takes every sandbox through
  Running → Idle → Paused → Dormant → woken, and eventually → Stopped.
- **G4 — Memory offload without losing the queue.** A dormant sandbox releases
  **≥ 95%** of its guest RAM pages to a file-backed memory image (Firecracker
  paused-memory-file restore; reclaimable page cache) while its VMM process,
  TAP, and virtio-net device queue remain alive. Only touched pages and VMM
  overhead stay resident.
- **G5 — Wake on network, in the guest's own stack.** An inbound packet for a
  paused or dormant sandbox is enqueued in the live host→guest virtio-net path
  and wakes it; after resume the guest consumes the queue and the data arrives
  on `eth0`. No proxy, no user-space replay, no application changes.
  First-byte overhead: **≤ 250 ms p95** from Paused, **≤ 500 ms p95** from
  Dormant (512 MB guest, warm page cache; the restore itself is tens of ms, so
  the tail is page-in and guest handling); 2 GB cold-image wake ≤ 2 s p95.
- **G6 — State continuity.** Pause/resume and dormant/wake preserve guest
  process state, filesystem view, and network state. Workloads must not need to
  be restart-safe for the pause case.
- **G7 — Operable.** One Go daemon + CLI, HTTP/JSON API over a Unix socket,
  structured events, Prometheus metrics, and crash-safe state. A daemon restart
  must never orphan VMMs or lose dormant sandboxes.
- **G8 — Efficient.** Density is bounded by real memory, not by bookkeeping:
  100+ dormant sandboxes on one 64 GB host within a 12 GB daemon+RSS budget
  (per-sandbox VMM overhead is accounted for explicitly).
- **G9 — Image pipeline.** Build bootable base disks from Dockerfiles fast:
  Dockerfile → OCI image (BuildKit) → ext4 base disk, content-addressed,
  cached, and shared across sandboxes.
- **G10 — CoW runtime volumes.** Creating a sandbox is an instant copy-on-write
  clone of its base disk (reflink or dm-thin, no full copy); writes are
  isolated, the volume persists across pause/dormant, and it is discarded on
  destroy.
- **G11 — eBPF L2 switch.** One in-kernel L2 switch per host forwards frames
  between sandbox ports and the uplink, enforces MAC/IP anti-spoofing and
  default sandbox isolation, exports per-port counters, and delivers wake events
  to userspace without taking frames out of the guest's path.

---

## 4. Non-goals (through v0.1)

Explicitly out of scope; re-evaluate only after the definition of done (§9).

- **Deep sleep with the VMM exited.** Killing the VMM frees its process
  overhead, but it also kills the device queue: restore from the memory file is
  only tens of ms, yet the packet that triggered the wake has nowhere to land
  during those milliseconds and is dropped, so the client pays a TCP RTO (or
  fastvm would have to hold frames in user space — the design we deliberately
  reject). Post-v0.1 at most; not the v0.1 path.
- **Multi-host scheduling / clustering.** Single host first. The API is shaped
  so a scheduler can be added later, but no distributed state.
- **Live migration** of a running VM.
- **Cross-host snapshot portability.** Memory images are local wake aids: no
  OCI packaging, no restore on a different host/CPU/VMM version.
- **Incremental/diff snapshots** (Firecracker marks them developer-preview).
- **GPU / PCI passthrough**, nested virtualization, confidential computing.
- **Windows or macOS guests**, arm64 guests (x86_64 first).
- **IPv6 in the network data path** — IPv4 veth/DNAT/neighbor handling in v0.1.
- **Kubernetes operator / containerd shim.** This is a standalone runtime.
- **A general-purpose VMM wrapper.** fastvm owns the boot contract.
- **Browser GUI.**
- **Cloning/fan-out from one memory image** (guest identity/RNG duplication).
- **Encryption at rest for memory images** (v0.2+; they contain guest secrets).
- **Docker/containerd as the sandbox runtime.** BuildKit is used only to build
  base disks; sandboxes are microVMs, not containers.
- **A from-scratch Dockerfile build engine.** fastvm image builds via BuildKit and
  converts the resulting OCI image; Dockerfile semantics beyond that are out of
  scope.
- **Sandbox-to-sandbox L2 connectivity by default.** The eBPF switch isolates
  ports; opt-in connectivity/ACLs are post-v0.1.
- **eBPF-based NAT/L4 or hardware XDP offload.** The switch does L2 forwarding;
  NAT/DNAT stay in nftables, and only generic tc/XDP is in scope.
- **Disk snapshots/rollback, volume resize, persistent user volumes.** Runtime
  volumes are ephemeral CoW clones; memory offload covers sleep state.
- **Multi-arch image builds** (x86_64 guests first).
- **Hostile multi-tenant operation.** Per-sandbox netns is core (M2) and jailer
  hardening lands in M7, but mutually hostile tenants are a v1.0 concern.

Practical v0.1 gaps that follow from these non-goals, plus behavior worth
knowing when operating fastvm, are collected in [LIMITATIONS.md](LIMITATIONS.md).

---

## 5. Core behavior — the lifecycle state machine

This section is the product. Everything else exists to make it true.

### 5.1 States

| State | Meaning | vCPUs | Guest RAM | Host resources |
|-------|---------|-------|-----------|----------------|
| `Creating` | Provisioning image, CoW volume, network port, VMM | – | – | base disk, CoW volume, netns, switch port |
| `Starting` | Cold boot; waiting for agent Ready | running | allocating | VMM, TAP, switch port, CoW volume |
| `Running` | Workload executing; activity tracked | running | resident | VMM, TAP, switch port, CoW volume |
| `Idle` | No activity; pause countdown running | running | resident | VMM, TAP, switch port, CoW volume |
| `Paused` | vCPUs halted; device queue live; frames buffered in the host→guest path | halted | resident | VMM, TAP, switch port, CoW volume, wake dispatcher |
| `Dormant` | Paused + guest RAM in a file-backed memory image (Firecracker paused-memory-file restore); VMM and queue alive | halted | file-backed page cache (demand-paged) | VMM, TAP, switch port, CoW volume, wake dispatcher |
| `Resuming` | Waking a paused/dormant sandbox; vCPUs restart and drain the queue | starting | paging in | VMM, TAP, switch port, CoW volume, wake dispatcher |
| `Stopping` | Graceful teardown | – | – | – |
| `Stopped` | Destroyed; disk and memory image removed | – | – | – |
| `Failed` | Terminal error with reason; manual/automatic GC | – | – | – |

> Term note: the host→guest queue a guest consumes on `eth0` is the virtio-net
> **receive** queue; the guest's TX queue is the guest→host direction. This
> document says "device queue" for the host→guest queue to avoid ambiguity.

### 5.2 Transition rules

| From → To | Trigger | Notes |
|-----------|---------|-------|
| `Creating → Starting` | base disk resolved, CoW volume cloned, switch port up, VMM spawned | failure → `Failed` |
| `Starting → Running` | guest agent Ready over vsock | boot timeout → `Failed` |
| `Running → Idle` | the idle predicate holds for `lifecycle.idle_window` (default 30 s) | workload exit also → `Idle` immediately |
| `Idle → Running` | qualifying activity, or CPU ≥ `idle.cpu_wake_pct` for `idle.exit_hold` | resets the debounce |
| `Idle → Paused` | idle for `pause_after` (default 2 m) **and** sleep barrier clear | manual `fastvm pause` skips the wait; switch port marked asleep (wake notifications armed) before pausing |
| `Paused → Running` | wake trigger (inbound frame, API) | resume vCPUs; the queued frames are consumed by the guest |
| `Paused → Dormant` | `offload_after` idle (default 10 m) **or** host memory pressure **or** manual `--offload` | snapshot a paused memory file + reload the VM from it (File backend, mmap), still paused; restore is tens of ms and not user-facing |
| `Dormant → Running` | wake trigger (inbound frame, API) | resume vCPUs; pages fault in from the memory file as the guest drains the queue |
| `* → Stopping → Stopped` | TTL (`destroy_after`, default 24 h idle), API delete, host shutdown | paused/dormant sandboxes are destroyed too |
| `Failed → Stopping` | GC/API | — |

> Post-v0.1 only: `Dormant → Hibernated` (VMM exits) and `Hibernated → Running`
> through a different wake mechanism — see §4 and §M8.

### 5.3 Activity model and the idle predicate

"Idle" is not a single metric — it is a **debounced predicate over four
independent signals**. CPU alone is never sufficient (a CPU-idle job can be
mid-disk-I/O), and network alone is never sufficient (a chatty logger is not
useful work). All four must be quiet for the whole window.

**Signals that count as activity:**

| Signal | Source | Counts as activity? |
|--------|--------|---------------------|
| Frames with L4 payload, or SYN/FIN/RST, in either direction on a published port | eBPF switch per-port counters | **Yes** — any data frame counts, including a response the server sends |
| Pure ACKs and TCP keepalives | eBPF switch (TCP flags/len) | No — an idle-but-open connection must not pin a sandbox awake |
| exec session attached, API operation, explicit `fastvm touch` | lifecycle API / guest agent | **Yes** |
| Workload start/exit events | guest agent | **Yes** (exit sets `workload_finished` and may move straight to `Idle`) |
| Block-device I/O | Firecracker block stats / host I/O counters | **Yes** when ≥ `idle.block_io_bytes` within the window |
| Guest CPU utilization | Firecracker per-vCPU accounting | **Vetoes** idleness while above `idle.cpu_threshold_pct`; does not reset the window by itself |

**Not activity:** ARP/NDP, broadcast/multicast noise, pure ACKs and keepalives,
switch control events, guest stdout/stderr writes (a server that logs must still
be able to sleep), and frames on `idle.ignore_ports` (intended for host-side
probes; by default health-check traffic **does** count and keeps a sandbox
warm).

**The predicate.** A sandbox becomes `Idle` only when all of the following hold
continuously for `lifecycle.idle_window` (default 30 s):

1. **No qualifying activity:** `now - last_activity_at ≥ idle_window` for
   network, exec/API, and block I/O;
2. **CPU quiet:** utilization averaged over the window (1 s samples) stays below
   `idle.cpu_threshold_pct` (default **1%** of the sandbox's total vCPU
   capacity);
3. **No interactive work:** no exec session attached, no in-flight frame
   delivery, and the sleep barrier is clear;
4. **No guest veto:** the agent has not reported `workload_busy` (an opt-in
   advisory hint for workloads that are I/O-bound or about to become busy).

The window is a **debounce**: any qualifying activity resets it, so one request
or a burst of frames restarts the countdown. `Idle → Running` happens on any
qualifying activity, or when windowed CPU stays above `idle.cpu_wake_pct`
(default 5%) for `idle.exit_hold` (default 3 s). CPU only prevents sleep — it
never wakes a sleeping sandbox.

**Measurement details:**

- CPU is sampled every `idle.sample_interval` (default 1 s) from the VMM's
  per-vCPU accounting, normalized by total vCPU capacity, so 1% means the same
  thing for a 1-vCPU and an 8-vCPU sandbox.
- `last_activity_at` and the current predicate inputs are persisted with the
  sandbox record; a daemon restart does not reset sleep timers.
- A freshly started sandbox is exempt for `idle.warmup` (default 10 s) after
  agent Ready, so boot/startup traffic is not mistaken for background noise.
- Every `Running → Idle` transition records its **reason** ("no network 31 s,
  cpu 0.4%, block io 0"), exposed via `fastvm events`, `fastvm ls`, and
  `fastvm activity <id>` — that is what makes threshold tuning possible.
- Thresholds are per-sandbox overridable; a workload that must never sleep can
  be created with `--no-auto-sleep`.

### 5.4 Policies (defaults, per-sandbox overridable)

```yaml
lifecycle:
  idle_window: 30s        # Running -> Idle
  pause_after: 2m         # Idle -> Paused
  offload_after: 10m      # Paused -> Dormant (0 disables)
  destroy_after: 24h      # sleeping/idle lifetime -> Stopped (0 = never)
  on_workload_exit: idle  # idle | pause | offload | stop
idle:
  cpu_threshold_pct: 1.0  # windowed CPU below this permits Idle (required, not sufficient)
  cpu_wake_pct: 5.0       # windowed CPU above this for exit_hold leaves Idle
  sample_interval: 1s
  exit_hold: 3s
  warmup: 10s
  block_io_bytes: 65536   # qualifying block I/O within idle_window
  keepalive_aware: true   # pure ACKs/TCP keepalives do not count
  ignore_ports: []        # frames here never count and never wake (host-side probes)
memory:
  offload_mode: file      # file (paused memory file, mmap) | uffd (optional) | swap | never
  evict_above: 0.90       # host RAM used % -> offload the oldest Paused sandboxes
  reserve_mb: 1024        # never schedule below this
wake:
  tcp: true
  udp: true               # packet-level trigger; delivery best-effort
  icmp: true
  allowed_ports: []       # empty = any unicast destination port
  max_wakes_per_minute: 6 # per sandbox; scan/wake-storm protection
  coalesce_window: 100ms  # burst of frames -> one transition
volume:
  cow: reflink            # reflink (default) | dm-thin | copy
  size: 8GiB
  discard_on_destroy: true
switch:
  mode: ebpf              # ebpf (default) | bridge (dev/CI fallback)
  isolate_sandboxes: true
  wake_notify: true
```

### 5.5 Wake-on-network semantics (v0.1 contract)

**The path.** Each sandbox's TAP is itself a port on the per-host eBPF L2
switch; a shared `fvup` ↔ `fvuplink` veth pair is the switch uplink to the host
stack:

```
host network → fvup → fvuplink (switch uplink ingress)
             → eBPF switch (static MAC→port redirect)
             → tap-<id> (sandbox switch port/egress) → Firecracker virtio-net
             → device queue (host→guest) → guest eth0
```

Guest→host frames take the reverse path: TAP ingress → switch → uplink →
`fvup` → host stack. There is no Linux bridge and no per-sandbox network
namespace in v0.1. The wake boundary is the switch's forwarding decision for
the sandbox's TAP port. Nothing in this path is a proxy, and fastvm never
writes guest packets into the VM.

- **While `Running`:** the switch forwards frames normally and counts per-port
  traffic for activity detection.
- **While asleep (`Paused` or `Dormant`):** the TAP and the virtio-net device
  stay attached. Frames arriving from the host continue into the live
  host→guest path, where the device fills the guest's posted RX buffers and the
  frames wait in the queue. The guest cannot consume them until its vCPUs run —
  that is exactly the buffer we want.
- **Queue depth is the buffer.** While the guest is paused it cannot post new
  RX buffers, so the amount of buffered traffic is bounded by the device queue
  depth (guest-posted buffers; typical virtio ring sizes provide hundreds of
  KB). Beyond that, the TAP drops frames, the switch still reports the wake
  event, and TCP retransmits recover. Drops are counted (`tap_rx_dropped`),
  never silently ignored.
- **Wake trigger.** The switch's forwarding program sees the first qualifying
  unicast frame destined for a sleeping port, emits a ringbuf wake event for
  that port, and *still redirects the frame into the port* toward the TAP. The
  dispatcher coalesces the burst into one transition (`coalesce_window`), so
  the frame is already in the queue by the time vCPUs resume, or is delivered
  as the paused device backend drains the TAP. In `switch.mode=bridge`, a
  passive AF_PACKET tap supplies the same event.
- **Resume.** `Resuming` resumes vCPUs (`PATCH /vm {"state":"Resumed"}`); the
  guest's network stack consumes the queued frames in order and replies
  normally. The first delivered byte is the wake-latency measurement. For
  `Dormant`, the VM was already reloaded from the paused memory file at offload
  time (File backend, mmap), so the restore itself is tens of milliseconds and
  does not sit on the wake path: resume is the vCPU restart, and page faults
  are served by the kernel from the memory file as the guest drains the queue.
- **No replay, by design.** fastvm holds no copy of the frame, never injects
  one, and never reorders. The frame the guest sees is the frame the host sent.
- **Ordering and duplicates.** The device queue is FIFO; frames are not
  duplicated by fastvm. Duplicates can still occur from client retransmissions
  (HTTP/TCP handles them), and the guest's TCP handles retransmitted SYNs/data.
- **TCP handshake across sleep.** A SYN arriving while asleep is enqueued like
  any frame; the guest's network stack completes the handshake after resume.
  If the wake takes longer than the client's retransmission budget, the client
  retransmits; if the queue has room, the retransmit is enqueued too. Wake
  targets sit well inside common client retry windows.
- **Established connections.** Guest TCP state lives in guest memory (and in the
  memory image when dormant), so connections are not forcibly reset by fastvm.
  Delivery continues after wake when the client's retransmissions cover the
  sleep. Guest timers resume with a clock jump (agent sync/NTP), and clients
  must still be retry-safe per §5.2.6.
- **ARP/NDP.** A static neighbor entry (or proxy-ARP for the guest address) is
  installed when the sandbox sleeps, so the host can send frames to the paused
  guest's MAC without waiting for a reply. Neighbor traffic itself never wakes a
  sandbox.
- **UDP/ICMP.** Unicast UDP/ICMP frames may wake the sandbox; delivery is
  best-effort (no retransmission semantics). Broadcast/multicast noise is
  ignored by default.
- **Scan / wake-storm protection.** Wake triggers are rate-limited per sandbox
  and optionally restricted to allowed ports; frames are still forwarded into
  the path only if the sandbox is running, and while asleep the queue depth
  bounds the damage. A port scan can cost at most the configured wake rate.
- **Nothing listening / workload exited.** The guest's kernel answers (usually
  RST), so clients see real guest behavior. The dispatcher records a
  `wake_no_response` event if a wake produces no outbound frame within a
  timeout, making dead services visible.
- **Failure of the device path.** If Firecracker's device thread turns out not
  to drain the TAP while vCPUs are paused (M4 spike), frames simply wait in the
  TAP/kernel queue and are drained after resume; if that queue overflows, the
  same bounded-drop/retransmit behavior applies. The switch's wake notification
  is independent of device-thread behavior.
- **If a dormant VMM dies.** The memory file is intact and reloadable in tens
  of ms, but frames that arrived while no device existed were dropped, and
  queued frames in the dead VMM are gone. The reconciler reloads the dormant VM
  from the file (so the next wake still works) and counts the loss; clients
  retransmit. If the image is unusable the sandbox is marked `Failed`.
- **Deep sleep (VMM exit).** Out of scope for v0.1: with the VMM gone there is
  no live device queue for the frame to land in. Post-v0.1 can either keep a
  small packet-parking shim (the design rejected here) or accept
  retransmission-only wake; this spec does not commit to either.

### 5.6 What "no manual management" means

The happy path is: `fastvm run` → use it → stop calling → sandbox pauses, then
becomes dormant → traffic wakes it → after `destroy_after` it is gone. CLI commands
(`sleep`, `offload`, `wake`, `rm`) are overrides and debugging tools, not the
normal workflow. `fastvm ls` is the observability surface: state, last activity,
guest RAM vs. resident memory, queue/drop counters, wake count, and the last few
state transitions.

---

## 6. Architecture

### 6.1 Shape

```
                      ┌────────────────────────────────────────────────┐
 fastvm CLI / API ───▶│ fastvmd (single Go binary, root)               │
                      │  API ─▶ lifecycle actor ─▶ store (bbolt)       │
                      │    │           │                 ▲             │
                      │    ▼           ▼                 │ wake event  │
                      │ builder     VMM driver       eBPF L2 switch    │
                      │ (BuildKit) (Firecracker)    (tc eBPF + maps)   │
                      │    │           │                               │
                      │    │     snapshot + File-backend restore       │
                      └────┼───────────┼─────────────────▲────────┬────┘
                           │           │ API socket       │ frames │ files
                           ▼           ▼                  │        │
                    base disks   firecracker         veth fv-<id>   │
                    (ext4, RO)   + tap-<id>          (switch port)  │
                           │           │                  │        │
                           │           ▼                  │        │
                           │   sandbox netns: veth-in ─ bridge ─ tap ─ virtio-net
                           │                  guest eth0 ─▶ agent + workload
                           │                                      │
                           ▼                                      ▼
                    volumes/ (CoW clones)              images/ memory/ state/
```

Single-host, single daemon, one actor per sandbox. No database server, no
message broker, no external control plane. State is bbolt + the on-disk sandbox
directory; the source of truth for a running sandbox is reconciled against the
actual VMM process on startup.

### 6.2 Components & responsibilities

| Component | Responsibility |
|-----------|----------------|
| **API (`internal/api`)** | REST-ish JSON over Unix socket; `sandboxes`, `events` (SSE/stream), `version`, `host`. Optional bearer token for TCP listeners. |
| **Lifecycle actors (`internal/lifecycle`)** | One goroutine per sandbox; owns state machine, timers (idle/pause/offload/TTL), sleep barrier, wake serialization/coalescing, scan rate limiting, event emission. |
| **Reconciler** | On boot and every 5 s: adopt live VMMs, reload dormant VMs whose VMM died, complete interrupted offload/wake transitions, detect orphan processes/TAPs, enforce memory-pressure policy. |
| **VMM driver (`internal/vmm`)** | Interface: `Create, Start, Pause, Resume, Snapshot, Restore, Stop, Stats`. Firecracker driver talks HTTP/JSON over a Unix socket (no cgo); QEMU driver used only as a dev/CI fallback (TCG) for functional tests. |
| **Wake dispatcher (`internal/wake`)** | Consumes wake notifications from the eBPF switch (AF_PACKET tap fallback in bridge mode); coalesces bursts, rate-limits scans, marks `wake_no_response`. It never holds, injects, or reorders frames. |
| **eBPF L2 switch (`internal/switch`)** | Per-host tc `clsact` eBPF programs + BPF maps: static MAC→port table, `bpf_redirect` between sandbox TAPs and the uplink, per-port MAC anti-spoofing, default sandbox isolation, per-port byte/packet counters, and a ringbuf wake-event channel. Loaded via `cilium/ebpf`; bridge-mode fallback for dev/CI. |
| **Builder (`internal/build`)** | Dockerfile → OCI image via BuildKit → unpacked rootfs → ext4 base disk (guest agent + init injected); content-addressed cache; `fastvm image build`/`images`. |
| **Volumes (`internal/volume`)** | Base-disk store (read-only, content-addressed) and per-sandbox CoW runtime volumes (reflink default, dm-thin fallback); clone, attach, discard, GC, refcounts. |
| **Network (`internal/network`)** | Per-sandbox TAP as switch port + shared `fvup`/`fvuplink` uplink veth, nftables NAT/DNAT for published ports, static neighbor entries, MAC/IP allocation. |
| **Snapshot & restore (`internal/snapshot`)** | Paused memory-file snapshots, File-backend paused reload (mmap, kernel demand paging), restore-time measurement, checksums/fsync, GC, disk budget. A userfaultfd page-in mode is an optional later optimisation. |
| **Guest agent (`guest/`)** | Static Go binary in the rootfs over vsock (Firecracker): Ready handshake, exec, signals, workload lifecycle events, stdout/stderr pump (logs, not activity), per-boot `/etc/hostname`, `/etc/hosts`, `/etc/resolv.conf` writer, `touch`, `workload_busy` advisory, clock/NTP note after resume, reconnect after vsock reset. |
| **Image manager (`internal/image`)** | Resolve/pull OCI images, unpack rootfs for the base-disk builder, and resolve base-disk digests for volumes. Kernel is pinned per host config, not taken from the image. |
| **Store (`internal/store`)** | bbolt: sandbox records, desired state, activity timestamps, memory-image metadata, events index. |
| **CLI (`cmd/fastvm`)** | Thin client: `run, ls, exec, shell, logs, pause, offload, resume, rm, events, host`. |

### 6.3 Key data flows

**Build** → Dockerfile + context → BuildKit → OCI image → unpack rootfs →
inject guest agent + init config → `mke2fs -d` → content-addressed base disk
(sha256) → cache; `fastvm image build`/`images` expose it.

**Cold start** → base-disk digest resolve → CoW runtime volume clone
(reflink/dm-thin, milliseconds) → switch port (static MAC + /30 address) →
netns/veth/TAP + NAT/DNAT + static neighbor → spawn Firecracker with the
volume, TAP fd, and vsock → configure + boot → agent Ready → `Running`.

**Idle → pause** → activity timer expires → `Idle` → `pause_after` → sleep
barrier: mark the switch port asleep (wake notifications armed), ensure no
in-flight frame delivery or exec session, then `PATCH /vm
{"state":"Paused"}` → state `Paused`. The TAP and device queue stay attached;
frames now accumulate in the host→guest path and the guest cannot consume them
until resume.

**Pause → dormant** → (only from idle/paused) `PUT /snapshot/create` writing
`vmstate` and the sparse `mem` file → fsync memory image and volume → stop the
original VMM → start a fresh Firecracker from that snapshot with the **File**
memory backend (`mmap`) and `resume_vm=false` → reattach TAP
(`network_overrides`), vsock, and static neighbor → state `Dormant`. Restore is
tens of ms (no guest-RAM copy); guest pages are now reclaimable file-backed page
cache. The device queue state comes from the snapshot, so it stays live for
inbound frames.

**Wake** → the switch sees the first frame for a sleeping port, emits a wake
event, and forwards the frame into the port's path → the dispatcher coalesces
the burst → `Resuming`: for `Paused`, `PATCH /vm {"state":"Resumed"}`; for
`Dormant`, the VM is already restored, so `PATCH /vm {"state":"Resumed"}` and
pages fault in from the memory file as the guest and device touch them → the
guest drains the device queue → first frame reaches `eth0`; record first-byte
latency; state `Running`.

**Destroy** → stop VMM, remove switch port, TAP, netns/veth, and rules; discard
the CoW volume and memory image; drop the base-disk reference; delete record.

### 6.4 Memory offload and restore

- **Default mode (`file`).** The sandbox is paused, a full memory + device-state
  snapshot is written to `sandboxes/<id>/memory/` (sparse `mem` file,
  `vmstate`, metadata with checksums), and the original VMM is stopped. A fresh
  Firecracker process restores that paused memory file with the **File** memory
  backend (`mmap`, kernel demand paging) and **does not resume vCPUs**. Restore
  is a handful of syscalls plus device-state load — tens of milliseconds, with
  no copy of guest RAM. Guest RAM is now reclaimable file-backed page cache
  rather than anonymous memory, while the VMM process, TAP, and device queue
  stay alive, so an inbound frame still lands in the guest's RX queue.
- **Why reload, and why it is cheap.** Firecracker cannot switch a running VM's
  memory backend, so offload is snapshot + paused reload. That cost is paid
  only when the sandbox is already asleep (`offload_after`), never on the wake
  path; because the reload is mmap-based, "going dormant" is a state change
  measured in tens of milliseconds, not a heavy hibernation.
- **Resident-page accounting is honest.** A dormant sandbox must not pin guest
  RAM as anonymous memory: guest pages are file-backed and reclaimable, only
  pages touched after the reload become COW anonymous, and the VMM process +
  TAP are fixed overhead. Report anon and file-backed memory separately
  (`smaps_rollup`), not a single `ps` number.
- **Memory image format.** Sparse, uncompressed, one file ≈ guest RAM size;
  zero pages are holes. Compression is a disk/CPU tradeoff to benchmark later,
  never on the wake path. Disk budget + GC enforce a host cap.
- **Wake cost.** Restore is already done; first byte comes after the vCPU
  resume and the page faults the guest's network stack and workload touch, plus
  whatever was already faulted for the queued frame. Cold images (page cache
  evicted) lengthen the first-byte tail: benchmark cold vs warm, target p95 in
  §8.
- **Optional uffd mode.** A userfaultfd memory backend with a Go page-in
  handler is a later optimisation for finer control (prefetch, page-in
  observability, compressed images). It is not the default: kernel demand
  paging is simpler, has no userspace fault handler on the wake path, and
  restore is already fast. UFFD also adds a failure mode (a dead handler blocks
  the VM) that the default path avoids.
- **Swap alternative.** `madvise(MADV_PAGEOUT)` on a live paused VMM avoids the
  snapshot+reload entirely, but depends on host swap being configured and gives
  less control over what is resident; available per-sandbox as
  `offload_mode: swap`.
- **Snapshot compatibility.** Same VMM binary/version, same CPU features, same
  disk/vsock paths; vsock resets on restore (agent reconnects). Metadata records
  the compatibility descriptor; an incompatible image is not loaded (sandbox
  fails with a clear event; no silent cold boot with lost state).
- **Failure modes.** Interrupted offload (crash between snapshot and reload) is
  reconciled on startup: finish the reload if the file is complete, otherwise
  restore the previous paused VMM from the still-running process. A VMM that
  dies while dormant is reloaded from the memory file (tens of ms); frames lost
  while no device existed are counted and clients retransmit.
- **Guest clock/RNG.** After resume, prefer Firecracker's `clock_realtime` on
  x86_64; otherwise the agent synchronizes the clock and logs the jump. Guest
  kernels with VMGenID support reseed the RNG on restore.
- **Secrets.** Memory images contain guest secrets: `0600`, daemon-owned
  directories, disk budget; at-rest encryption is an explicit v0.2 item.

### 6.5 Storage: base disks and CoW runtime volumes

- **Base disks** are built from Dockerfiles (or ingested OCI images) as raw
  ext4 images: BuildKit produces the OCI image, fastvm unpacks the rootfs,
  injects the guest agent + init configuration, and creates the disk with
  `mke2fs -d`. The result is content-addressed (sha256), read-only, and shared
  by every sandbox using that image; the pinned kernel stays separate.
- **Build pipeline:** `fastvm image build -f Dockerfile -t name:tag` → BuildKit → OCI
  → base disk; `fastvm image ls` lists disks and digests. BuildKit's layer cache
  and the base-disk cache are content-keyed, so unchanged rebuilds are cheap,
  and a sandbox keeps the base digest it was created with even after a newer
  image is built.
- **CoW runtime volumes.** Each sandbox gets a writable raw volume cloned from
  its base disk in milliseconds: `reflink` (`FICLONE` on XFS/Btrfs) by default,
  `dm-thin` snapshots as the fallback for hosts without reflink, full `copy` as
  the last resort. Clones share blocks until written; per-sandbox writes are
  isolated.
- **Firecracker compatibility.** Firecracker takes raw block devices, so base
  disks and volumes are raw files/devices at stable per-sandbox paths (required
  by snapshot restore). Reflink clones are files; dm-thin snapshots expose a dm
  device.
- **Sleep and destroy.** The volume persists across pause and dormant unchanged
  (memory-file restore assumes the same disk path and content), so the guest's
  filesystem needs no disk snapshot. At offload the volume is fsynced together
  with the memory image, so a host crash cannot leave the image referencing
  writes that were lost. Destroy discards the volume; base disks are
  refcounted/GC'd and never removed while a derived volume exists.
- **Consistency.** The pause barrier drains virtio-blk I/O before halting; the
  memory image carries the guest page cache, so a woken guest sees a consistent
  filesystem without host-side disk snapshots.
- **Disk budget.** Base disks and volumes are accounted separately; GC evicts
  unused bases (LRU) but refuses to evict one with live derived volumes.
- **Security.** Dockerfile builds go through BuildKit (rootless or a configured
  daemon socket); base disks and volumes are `0600`, daemon-owned.

### 6.6 Networking model: eBPF L2 switch

- **Topology:** `tap-<id>` (Firecracker's virtio-net) is itself a **port on the
  per-host eBPF L2 switch**. The switch's uplink is a shared `fvup` ↔
  `fvuplink` veth pair; the host stack, gateway IPs, static neighbors and NAT
  live on `fvup`. There is no Linux bridge and no per-sandbox network namespace
  in v0.1. Addresses come from `10.77.0.0/16` (point-to-point /30); MACs are
  assigned at create.
- **Data plane:** tc `clsact` eBPF ingress programs on each switch port use a
  static MAC→port map (no learning needed — guest MACs are known) and
  `bpf_redirect`: TAP ingress carries guest→host frames and redirects them to
  the uplink; uplink ingress carries host→guest frames and redirects them into
  the destination TAP. Per-port state (running/asleep) and counters live in BPF
  maps. Default policy: sandbox↔uplink allowed, sandbox↔sandbox dropped and
  counted, source-MAC anti-spoofing per port, unknown destination from a
  sandbox → uplink or counted drop.
- **Wake notification:** when an uplink-ingress frame's destination TAP port is
  asleep, the program emits a ringbuf event for that port and *still redirects
  the frame into the TAP*, toward the guest's device queue. The userspace
  dispatcher coalesces the burst into one transition; no frame waits on
  userspace.
- **NAT and ingress:** nftables DNAT/SNAT on `fvup` for published ports; the
  switch sees post-DNAT frames. Published endpoints are the VM's own network
  stack — no user-space listener. Static neighbor entries on `fvup` keep the
  host→TAP path resolving while a guest sleeps; ARP/NDP never wakes.
- **Activity counters:** per-port rx/tx bytes/packets/drops exported from BPF
  maps and fed into the lifecycle activity model.
- **Fallback/rollback:** `switch.mode=bridge` keeps a Linux-bridge + AF_PACKET
  path for dev/CI hosts without the required kernel/BTF, and as a rollback
  mode; the AF_PACKET dispatcher then supplies wake notifications.
- **Kernel requirements:** tc `clsact` eBPF, ringbuf, and BTF (documented
  minimum kernel, e.g. 5.15+); `cilium/ebpf` verifier/load errors surface at
  startup with a clear fallback hint.
- **Guest identity files:** like Docker, each sandbox gets a per-boot
  `/etc/resolv.conf`, `/etc/hosts`, and `/etc/hostname`; because Firecracker has
  no bind-mount path, the daemon mirrors the host resolver (filtering loopback
  nameservers, with a public fallback) and passes the generated files and
  hostname over the kernel command line. The guest agent writes them and sets
  the kernel hostname before starting the workload. `network.dns` /
  `network.search` override the mirrored values.
- **Isolation:** switch anti-spoofing and default sandbox↔sandbox drop prevent
  cross-sandbox L2 spoofing; per-sandbox netns and hostile multi-tenant
  hardening remain later milestones.

### 6.7 Key decisions (ADR seeds)

| # | Decision | Chosen for v0.1 | Alternatives | Rationale | Revisit |
|---|----------|-----------------|--------------|-----------|---------|
| 1 | VMM | **Firecracker** | Cloud Hypervisor, QEMU | Fastest boot, small device surface, pause/snapshot support, already installed | If restore fidelity or virtiofs is needed |
| 2 | Go integration | Thin REST/JSON client over VMM Unix socket | `firecracker-go-sdk` | Zero cgo; full control of snapshot/restore APIs; SDK coverage to be verified in M2 | M2 — use SDK if it covers snapshot+restore cleanly |
| 3 | Sleeping model | **Pause keeps the VMM, TAP, and device queue live; offload hands guest RAM to a file-backed image via Firecracker restore** | VMM exit + frame parking; pause only | The inbound frame lands in the guest's own queue, so wake needs no replay and TCP state survives | Deep sleep post-v0.1 if density demands it |
| 4 | Memory offload | **Paused memory-file snapshot + File-backed reload (mmap, kernel demand paging)** | uffd page-in; `swap`/`MADV_PAGEOUT` | Restore is tens of ms with no userspace fault handler on the wake path; keeps the queue live while releasing RAM | UFFD if prefetch/compression is needed; swap if host swap is guaranteed |
| 5 | Wake trigger | **eBPF switch ringbuf event when a frame targets a sleeping port** | AF_PACKET tap (bridge fallback); poll VMM rx counters; nftables queue | Event fires at the L2 forwarding decision; the frame is still redirected into the guest path; no userspace frame handling | Program complexity/verifier; bridge fallback retained |
| 6 | Activity | Agent events + switch frames + API + CPU secondary | CPU-only | Correct for idle servers and finished jobs | — |
| 7 | Idle policy | Fixed defaults, per-sandbox overrides | Adaptive | Predictable; tune after soak data | M6 |
| 8 | State store | bbolt + filesystem | SQLite, Postgres, etcd | Embedded, transactional, single host | Multi-host milestone |
| 9 | Guest agent | Mandatory static Go binary over vsock | No agent | Readiness, exec, workload exit, activity hints | — |
| 10 | Kernel | Pinned host-local kernel image | Kernel from image | OCI images do not contain kernels; reproducibility | — |
| 11 | Network topology | Per-sandbox netns + veth pair + eBPF switch ports; nftables DNAT ingress | Linux bridge; shared host TAP/bridge; macvtap | Clean wake boundary, per-port isolation, counters and wake events in-kernel | IPv6; eBPF offload; netns hardening |
| 12 | Wake buffering | **Device queue + TAP kernel queue** | User-space parking queue; proxy | Bounded, in-path, no replay logic | RX queue sizing after benchmarks |
| 13 | Runtime volumes | **CoW clone: reflink by default, dm-thin fallback** | Guest overlayfs; full copy; qcow2 | Millisecond clones, block sharing, raw devices Firecracker accepts | dm-thin default if reflink filesystems are not guaranteed |
| 14 | Image build | **Dockerfile → BuildKit → OCI → ext4 base disk** | Custom Dockerfile builder; docker buildx; container2vm-style converters | BuildKit gives correct Dockerfile semantics and caching; fastvm owns rootfs→disk conversion | BuildKit dependency/version policy; remote builders later |
| 15 | L2 switch | **tc `clsact` eBPF programs + BPF maps** | Linux bridge; OVS; XDP-native-only | Redirect, isolation, counters, and wake events with one data-plane program; generic tc works on veths | XDP-native optimization; per-host program reload/rollback |

### 6.8 Repository layout (created in M0)

```
cmd/fastvm/           CLI
cmd/fastvmd/          daemon
internal/api/         HTTP/JSON + events
internal/lifecycle/   state machine, actors, policies, reconciler
internal/vmm/         driver interface, firecracker/, qemu/
internal/build/       Dockerfile -> OCI (BuildKit) -> base ext4 disk
internal/volume/      base-disk store, CoW clones, GC/refcounts
internal/switch/      eBPF L2 switch loader + programs (bpf2go)
internal/network/     netns, veth, tap, in-netns bridge, nat/dnat, neighbors
internal/wake/        wake dispatcher (switch events / AF_PACKET fallback)
internal/snapshot/    memory image, File-backend restore, gc
internal/image/       OCI pull/unpack, agent injection
internal/store/       bbolt
internal/agentproto/  vsock wire protocol
guest/                fastvm-agent
docs/adr/             decision records
test/integration/     KVM-gated end-to-end tests
test/perf/            benchmarks with recorded baselines
Makefile, go.mod, README.md, docs/DESIGN.md, CONTRIBUTING.md
```

---

## 7. Milestones

Each milestone is done only when its exit criteria pass on the reference host
(KVM, NVMe, x86_64; perf numbers are measured, not estimated).

### M0 — Skeleton (S)
Go module, layout, Makefile, CI (build, `go vet`, `golangci-lint`, unit tests),
daemon skeleton serving `version`/`health`, CLI skeleton, bbolt init, config
loading, ADR-0001 (VMM choice), README stub.
**Exit:** `go build ./...` and `go test ./...` green in CI; `fastvmd` starts
without KVM and reports healthy; `fastvm version`/`ls` work.

### M1 — Image build pipeline (M)
Dockerfile + context → BuildKit → OCI image → unpacked rootfs → guest agent +
init injection → ext4 base disk; content-addressed base-disk cache;
`fastvm image build`, `fastvm image ls`; test-image corpus in CI.
**Exit:** nginx and a multi-stage Dockerfile both build to bootable base disks;
an unchanged rebuild hits the cache without rebuilding; base-disk identity is
content-addressed and stable for identical inputs; base disk boots in a QEMU/TCG smoke test; CI job green.

### M2 — Cold boot + CoW runtime volumes (M/L)
Base-disk store; CoW volume clone (reflink default, dm-thin fallback, copy as
last resort); Firecracker driver with a raw volume; per-sandbox netns + veth +
TAP + Linux bridge + nftables NAT/DNAT + static neighbor (baseline networking
before the eBPF switch); guest agent Ready handshake; `fastvm run`, `exec`,
`logs`, `rm`; boot benchmark harness; KVM-gated integration job in CI (QEMU/TCG
smoke test on KVM-less machines).
**Exit:** nginx sandbox answers `curl` on a published (DNAT) port; `exec` works;
writes in one sandbox are invisible to another; 50 CoW clones complete with p95
≤ 50 ms on a reflink filesystem; `rm` discards the volume and drops the base
reference; cold boot p50 ≤ 200 ms / p95 ≤ 400 ms (goal 150/300, closed in M6).

### M3 — eBPF L2 switch (M/L)
tc `clsact` programs + BPF maps (MAC→port table, per-port asleep state,
counters, ringbuf wake events); loader via `cilium/ebpf` with bpf2go; static
MAC/IP assignment; per-port source MAC/IP anti-spoofing; default
sandbox↔sandbox isolation; uplink port; replaces the Linux bridge on supported
kernels; `switch.mode=bridge` fallback retained; privileged BPF tests in CI;
throughput benchmark vs bridge.
**Exit:** `curl` and `ping` work through the switch; sandbox↔sandbox frames are
dropped and counted; a spoofed source MAC is dropped; per-port counters match a
reference capture; wake ringbuf event is delivered within 1 ms of the frame;
bridge fallback still passes the M2 e2e suite; forwarding throughput is within
10% of the bridge baseline.

### M4 — Pause & wake (M)
State machine + actors + activity tracker + timers; sleep barrier (mark switch
port asleep, quiesce, halt vCPUs); TAP/device queue kept alive while paused;
wake dispatcher (switch events); coalescing/rate limits; manual `sleep`/`wake`;
state persistence; events; basic reconciler; **spike:** confirm that
Firecracker's device backend keeps draining the TAP into the guest RX queue
while vCPUs are paused (and measure usable queue depth).
**Exit:** a guest process counter continues across pause→wake; a frame sent
while paused is consumed after wake and answered; pause p95 ≤ 50 ms;
wake-to-first-byte p95 ≤ 250 ms; daemon restart leaves no orphans and adopts
running VMs.

### M5 — Dormant (memory offload) (M/L)
Paused memory-file snapshot; paused reload with the File memory backend (mmap);
measure restore time; volume + memory-image fsync ordering; sparse memory image;
RAM release; atomic metadata + checksums; disk GC/budget; interrupted-offload
reconciliation; dormant-VMM-death recovery; compatibility checks.
**Exit:** dormant sandbox ≤ 64 MB attributable anon RAM and ≥ 95% of guest RAM
non-anonymous/reclaimable; snapshot restore p95 ≤ 150 ms; inbound frame while
dormant is enqueued and answered after wake; filesystem state (files, logs)
intact after wake; dormant wake p95 ≤ 500 ms (512 MB, warm cache); daemon
restart restores 100% of dormant sandboxes; incompatible image fails with a
clear event.

### M6 — Autonomy & soak (M/L)
Default policies wired end to end (workload-exit → idle, idle → pause, pause →
dormant, TTL → stop); memory-pressure eviction; full reconciler; Prometheus
metrics; `events` stream; build/volume/switch integration in one lifecycle;
CLI polish; runbook; 24 h soak with churn.
**Exit:** a 24 h soak with 100 sandboxes (mixed servers + jobs + exec) and
randomized traffic: wake success ≥ 99.9%, zero stuck state machines, zero
orphan VMMs/TAPs/switch ports, no leaked volumes or base references, honest RAM
accounting, no manual intervention; cold boot p50 ≤ 150 ms; dormant-wake
targets met.

### M7 — Hardening (L)
Firecracker jailer + seccomp; eBPF program hardening (verifier limits, map
concurrency, program rollback, fuzz); build-pipeline security (rootless
BuildKit, cache poisoning, Dockerfile trust boundary); memory-image page-cache
behaviour (cold-start tails, readahead/prefetch); optional uffd mode (if
adopted); snapshot GC verification; at-rest encryption design (or
implementation, if cheap); crash-injection and state-machine property/fuzz
tests; load/contention benchmarks; security review checklist; IPv6 decision.
**Exit:** hostile-guest smoke tests pass inside jailer; netns + switch isolation
verified (no cross-sandbox ARP/MAC/IP spoofing); switch programs survive a
verified reload/rollback; build sandboxing is documented and tested; dormant
VMs recover after VMM death (reload from the memory file); fuzz targets run in
CI; published benchmark report.

### M8 — Post-v0.1 (not part of this goal)
Deep sleep with the VMM exited (plus whatever frame-handling tradeoff it needs);
multi-host scheduler + portable memory images; sandbox↔sandbox policy/ACLs;
HTTP ingress/TLS; persistent volumes and disk snapshots; UDP connection
semantics; arm64/macOS; warm pools; clone/fan-out; multi-arch builds.

---

## 8. Success metrics (v0.1 targets)

Measured on the reference host with a 2 vCPU / 512 MB nginx sandbox unless
noted; page-cache state recorded for every number.

| Metric | Target | Measurement |
|--------|--------|-------------|
| Cold boot (spawn → agent Ready) | p50 ≤ 150 ms, p95 ≤ 300 ms | 100 cold starts, lifecycle event timestamps |
| Base-disk build, unchanged Dockerfile (cache hit) | no layer rebuild; ≤ 500 ms | build timestamps |
| Dockerfile → bootable base disk (small image, cold) | ≤ 60 s; recorded baseline per image | build log |
| CoW volume clone | p50 ≤ 10 ms, p95 ≤ 50 ms (reflink); ≤ 200 ms (dm-thin) | clone timestamps |
| Volume write isolation | 0 cross-sandbox writes in the suite | e2e |
| eBPF switch wake-event latency | ≤ 1 ms frame → ringbuf event | BPF timestamps |
| eBPF switch throughput | within 10% of the Linux-bridge baseline | iperf/pktgen |
| eBPF switch isolation | 0 default sandbox↔sandbox frames; spoofed MACs dropped and counted | e2e |
| Pause (Running → Paused) | p95 ≤ 50 ms | state timestamps |
| Wake from Paused → first delivered byte | p50 ≤ 100 ms, p95 ≤ 250 ms | switch wake event → guest byte |
| Snapshot restore (paused memory file → paused VMM) | p50 ≤ 50 ms, p95 ≤ 150 ms | restore counter, lifecycle timestamps |
| Wake from Dormant → first delivered byte (512 MB, warm cache) | p50 ≤ 150 ms, p95 ≤ 500 ms | switch event + page-fault timestamps |
| Dormant wake, 2 GB guest, cold image | p95 ≤ 2 s | same |
| Queue buffering while asleep | no frame loss within the measured device-queue depth; drops beyond it counted | burst test, TAP/Firecracker counters |
| TCP handshake across dormant wake | 100% of queued SYNs complete after wake (< 5 s asleep) | e2e |
| Scan/wake-storm containment | 10k pps for 60 s: wakes ≤ `max_wakes_per_minute`, no host memory growth, 0 OOM | adversarial test |
| Dormant RAM | ≤ 64 MB attributable anon/sandbox; ≥ 95% of guest RAM not anonymous (file-backed, reclaimable) | `smaps_rollup` anon vs file, before/after |
| Idle entry (predicate holds → Idle) | ≤ 2 s | predicate inputs vs state events |
| False sleeps | 0 during active-transfer, active-exec, and block-I/O tests; CPU-busy/network-quiet jobs stay Running | stress suite |
| Idle predicate accuracy | 0 missed idles in a 1 h quiet run; 0 false idles in the activity suite | e2e |
| Idle → Paused latency | ≤ 2 s after the `pause_after` deadline | state events |
| Wake success rate (24 h soak) | ≥ 99.9% | wake-dispatch counters |
| Restart recovery | 100% dormant sandboxes recoverable; 0 orphan VMMs/TAPs | soak with daemon kills |
| Density | 100 dormant + 20 running (512 MB) on a 64 GB host within 12 GB anon RSS (reclaimable page cache excluded) | load test |
| Memory-image disk | ≤ 1.1× guest RAM; bounded by `max_disk_gb` with GC | `du` + GC test |
| Event/state observability | Every transition visible in `fastvm events` with cause | e2e assertions |

---

## 9. Definition of done (v0.1)

A reproducible demo on one KVM host, no manual lifecycle steps:

1. `fastvm image build` from a Dockerfile produces a content-addressed base disk; a
   sandbox from it boots; an unchanged rebuild is served from cache.
2. Ten sandboxes cloned from one base share underlying disk blocks; writes are
   isolated; clone time and `fastvm rm` reclamation are within targets.
3. `fastvm run` three sandboxes from built images (a web server, a batch job,
   an exec service).
4. Idle servers pause within `idle_window + pause_after`; they become dormant
   within `offload_after`; `fastvm ls` shows honest RAM/queue/disk accounting.
5. Idle predicate: a CPU-busy, network-quiet job stays Running; a
   block-I/O-heavy job stays Running; an idle server that keeps logging pauses;
   an attached exec session prevents pausing; a quiet server becomes Idle
   within `idle_window` + 2 s.
6. `curl` to a dormant server wakes it and returns its response; a burst of 50
   concurrent connections completes after one coalesced wake; a UDP datagram
   sent while asleep is delivered after wake; p95 wake within targets.
7. The batch job finishes, pauses, becomes dormant, and its output/state is
   still there when woken.
8. The eBPF switch enforces default sandbox isolation, drops spoofed MACs,
   exposes per-port counters, and delivers wake events; `switch.mode=bridge`
   passes the same e2e suite.
9. Kill `fastvmd` at random points (idle, mid-pause, mid-offload, mid-wake):
   restart reconciles; no orphan VMMs; dormant sandboxes recover or fail
   explicitly; events explain every recovery decision.
10. 24 h, 100-sandbox churn soak passes §8; benchmark report committed.
11. Docs: README quickstart, architecture overview, lifecycle/state-machine
    reference, runbook, and ADRs for VMM choice, sleeping model, memory-file
    restore, image build, CoW volumes, and the eBPF switch.

---

## 10. Risks & mitigations

| Risk | Impact | Mitigation |
|------|--------|-----------|
| **No KVM on the dev box** (currently `/dev/kvm` is absent even though AMD-V and the kvm modules are loaded) | Blocks all real testing | Confirm nested-virt/device passthrough or designate a remote KVM host; KVM-gated CI job with QEMU/TCG functional fallback; perf gates only on KVM |
| eBPF kernel/BTF portability and verifier limits | Switch unavailable on some hosts, dev friction | Documented minimum kernel; `switch.mode=bridge` fallback; CI kernel matrix; clear startup error with the fallback hint |
| eBPF program bugs (loops, stale maps, missed redirect) | Broken traffic or silent isolation gaps | Verifier-friendly program; netns-based unit tests; staged attach with rollback; bridge fallback |
| BuildKit dependency and untrusted Dockerfiles | Build failures or host exposure | Pin BuildKit version; rootless/sandboxed build service; documented trust boundary; no host mounts |
| CoW mechanism unavailable or corruption | Slow copy fallback or damaged disks | Detect reflink support; dm-thin fallback; copy last resort; integrity tests; fsync volume at offload |
| Base-disk / volume reference leaks | Disk exhaustion over time | Refcounted GC, per-host disk budget, soak leak checks |
| Build cache poisoning | Wrong or tampered base disk | Content-addressed digests, verify layer digests, builder trust boundary |
| Static MAC→port map growth | BPF map sizing limits sandbox count | Size maps at load; document per-host port limit; monitor map hash usage |
| Firecracker's device backend may not drain the TAP while vCPUs are paused | Frames buffer in the kernel TAP queue instead of the device queue; depth assumptions shift | M4 spike measures it; the wake dispatcher is independent, and frames are drained on resume; queue-depth numbers are measured, not assumed |
| Device queue depth is small (guest-posted RX buffers) | Bursts while asleep drop frames | Measure usable depth in M4; size RX queues; drops are counted and TCP retransmits recover; document the bound |
| Memory-image page cache eviction (cold image) | Wake tails | Sparse uncompressed memory image on NVMe; benchmark cold vs warm; readahead/prefetch optional; keep image files referenced/GC-protected |
| Dormant VMM death (crash) | Queued frames lost, wake gap | Reconciler reloads from the memory file (tens of ms); losses counted, clients retransmit; tested in M7 |
| Snapshot + paused reload transition fails mid-way | Stuck sandbox, disk churn | Only runs while already asleep (snapshot write is the heavy part); atomic temp-dir image + fsync; reconcile interrupted transitions on startup; per-sandbox disk budget |
| Per-sandbox VMM overhead (process, TAP, switch port) limits density vs VMM-exit designs | Fewer sandboxes per host | Account for it in §8; deep sleep with VMM exit is a documented post-v0.1 lever |
| Snapshot incompatible after VMM/kernel/CPU changes | Dormant wake fails | Pin VMM+kernel per host, record compatibility descriptor, validate before load, fail explicitly (no silent cold boot); local-only images |
| Static neighbor / MAC after restore | Host cannot deliver frames to a sleeping guest | Re-install static neighbor on restore; verify MAC preserved across snapshot; e2e neighbor test |
| Wake-event races with pause/resume | Missed or duplicate wake triggers | Mark the switch port asleep before pausing and clear it only after resume; single-writer actor + coalescing; tests at transition boundaries |
| Scan/wake-storm DoS | Sandboxes kept awake, cost spike | Per-sandbox wake rate limit, allowed-port list, coalescing; queue depth bounds damage; adversarial test |
| Established connections across long sleeps | Client-visible resets if the retry budget expires | Guest TCP state preserved; wake targets inside common retry budgets; clients retry-safe; `wake_no_response` telemetry |
| Idle detection wrong (false sleep, never sleeps) | Broken UX | Debounced predicate over network + exec/API + block I/O + CPU; block-I/O counters prevent I/O-bound false sleeps; stdout is not activity; overrides + `fastvm activity` telemetry; fault-injection tests |
| Idle thresholds wrong for the workload mix | Sandboxes sleep mid-work or never sleep | Per-sandbox overrides (`--no-auto-sleep`, custom thresholds); defaults are conservative; soak data + `fastvm activity` reasons drive tuning |
| Race between state machine, VMM process, TAP, switch maps, and store | Corruption, stuck states | Single-writer actors, barrier ordering, reconciliation loop, crash-injection tests, property-based state-machine tests |
| Memory images contain guest secrets | Confidentiality | `0600` perms, daemon-owned dirs, host trust boundary documented; encryption v0.2 |
| Guest clock/RNG after resume | Subtle workload bugs | `clock_realtime` on load or agent clock sync; VMGenID-capable guest kernel; documented |
| Optional uffd mode / userfaultfd restrictions | Only affects the optional page-in mode | Not on the default path; daemon runs as root; feature-flagged; `file`/`swap`/`never` fallbacks |
| Image pipeline complexity (ext4, kernel modules, agent injection) | Slow M1 | Pinned kernel, minimal rootfs contract, CI test images, reflink with copy fallback |
| Scope creep toward a platform (multi-host, k8s, ingress) | Never ships | Non-goals §4 are binding until §9 passes |

---

## 11. Open questions (defaults chosen so work can proceed)

1. **Workload mix** — confirm the §2 assumption (dev servers + jobs + exec). If
   it is really agent code-execution only, the warm-pool and server-wake design
   can be simplified.
2. **Ingress model** — default v0.1 = nftables DNAT host ports, no user-space
   listener; HTTP/TLS ingress as a separate M8+ component.
3. **Multi-tenant?** — default: single-operator host, untrusted guest code,
   hardened isolation in M7, hostile multi-tenant only at v1.0.
4. **Maximum guest RAM target** — default 4 GB; affects memory-image size,
   page-in latency, and GC budget.
5. **Offload mode** — default `file` (paused memory-file snapshot + File-backed
   reload, mmap). `uffd` (page-in handler, optional/prefetch), `swap`
   (`madvise(MADV_PAGEOUT)`, requires host swap), and `never` are per-sandbox
   options. Confirm Firecracker's restore time ("tens of ms") on the target
   hardware before locking the §8 wake targets.
6. **Deep sleep (VMM exit)** — explicitly deferred to post-v0.1. Confirm that
   keeping a small VMM process per sleeping sandbox is acceptable for density
   targets, or that process-exit behavior is a hard requirement.
7. **Device queue depth / RX queue sizing** — need measurements before locking
  buffering guarantees; the default assumes virtio ring depth is enough for a
  burst of SYN/first-byte traffic.
8. **UDP/ICMP wake** — default **on** as packet-level triggers (best-effort
  delivery); confirm that is preferable to "TCP only".
9. **Snapshot retention / host reboot** — default: dormant state survives
  daemon restarts and host reboots (memory images reload lazily on wake);
  images live until `destroy_after`/GC.
10. **API auth** — default: Unix socket (root-owned) + optional bearer token on
    TCP; no user model in v0.1.
11. **License** — TBD: MIT vs Apache-2.0 (repository is currently unlicensed).
12. **Reference hardware** — the current box (128 cores / 251 GB, AMD-V, no
    `/dev/kvm` exposed) vs. a dedicated KVM host; needed to lock the §8
    baselines.
13. **IPv6** — default IPv4-only for v0.1.
14. **Build backend** — default: BuildKit via a local (rootless) daemon/socket
    with a pinned version; remote builders and `buildx` are post-v0.1. Confirm
    the environment can run BuildKit (rootless or root daemon).
15. **CoW mechanism** — default reflink on XFS/Btrfs, dm-thin fallback; confirm
    the reference host's filesystem and whether a thin pool is available.
16. **eBPF switch rollout** — default: eBPF mode on kernels with tc/BTF support
    (~5.15+), Linux-bridge fallback otherwise. Confirm the acceptable minimum
    kernel and fallback behavior.
17. **Sandbox↔sandbox networking** — default **denied** (isolated ports). If a
    shared L2 segment between sandboxes is expected, that changes switch policy
    and is opt-in.
18. **Dockerfile scope** — multi-stage builds are supported through BuildKit;
    build secrets (`--secret`), `RUN --mount`, and multi-arch are out of scope
    for v0.1 unless requested.
19. **Idle thresholds** — defaults: 30 s debounce, CPU < 1% (windowed) to
    permit sleep, 5% to leave `Idle`, 64 KiB block-I/O budget, stdout not
    counted. Confirm these fit the workload mix; health-check traffic counts as
    activity unless its port is added to `idle.ignore_ports`.

---

## 12. Prior art & references

- **Firecracker** — microVM with ~125 ms boot, pause/resume, full snapshots with
  File (mmap) and userfaultfd memory backends for restore:
  <https://github.com/firecracker-microvm/firecracker> and
  <https://github.com/firecracker-microvm/firecracker/tree/main/docs/snapshotting>.
- **Fly Machines suspend/resume** — closest product behavior: snapshot memory to
  disk, resume in hundreds of ms, auto-stop on idle / auto-start on traffic;
  caveats: ≤ 2 GB RAM recommended, snapshots invalidated by deploys, TCP
  connections may break: <https://fly.io/docs/reference/suspend-resume/>.
- **Cloud Hypervisor snapshot/restore** — file, `ondemand` (uffd), and
  `copyonwrite` memory modes: <https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/snapshot_restore.md>.
- **flintlock snapshot/restore requirements** — catalogues VMM snapshot
  constraints (disk path pinning, `network_overrides`, vsock reset on restore,
  clone identity/RNG) that inform our design:
  <https://github.com/liquidmetal-dev/flintlock/blob/main/docs/proposals/26-09-25-snapshot-restore/0204-snapshot-restore-requirements.md>.
- **Hypeman** — closest prior art: Go sandbox runtime with multiple hypervisors,
  standby/restore snapshotting, OCI images, and built-in ingress:
  <https://github.com/kernel/hypeman>.
- **Firecracker network setup (TAP, host routing):**
  <https://github.com/firecracker-microvm/firecracker/blob/main/docs/network-setup.md>.
- **packet(7)** (AF_PACKET passive taps, used by the bridge-mode wake fallback):
  <https://man7.org/linux/man-pages/man7/packet.7.html>.
- **userfaultfd(2)**: <https://man7.org/linux/man-pages/man2/userfaultfd.2.html>.
- **Firecracker rootfs/kernel setup:**
  <https://github.com/firecracker-microvm/firecracker/blob/main/docs/rootfs-and-kernel-setup.md>.
- **BuildKit** (Dockerfile frontend, layer cache):
  <https://github.com/moby/buildkit>.
- **cilium/ebpf** (pure-Go eBPF loader/verifier): <https://github.com/cilium/ebpf>.
- **eBPF docs**: <https://docs.ebpf.io/>.
- **ioctl_ficlone(2)** (reflink CoW clones):
  <https://man7.org/linux/man-pages/man2/ioctl_ficlone.2.html>.
- **dm-thin provisioning:**
  <https://docs.kernel.org/admin-guide/device-mapper/thin-provisioning.html>.
- **Firecracker Go SDK**: <https://github.com/firecracker-microvm/firecracker-go-sdk>.
- **gVisor netstack** (candidate for a future optional L4 component, not the
  v0.1 wake path): <https://github.com/google/gvisor>.
- **OCI image spec**: <https://github.com/opencontainers/image-spec>.

---

## 13. How this document is used

- This is the north star: milestones, ADRs, and implementation tasks must trace
  back to a goal, invariant, or metric here; anything that does not is scope
  creep.
- Changes to goals, non-goals, or success metrics require a version bump and a
  decision note in `docs/adr/`.
- The definition of done (§9) is the acceptance test for v0.1.
- Suggested next actions after sign-off: scaffold M0, create ADR-0001, and
  register the objective as a tracked session goal for autonomous execution.
