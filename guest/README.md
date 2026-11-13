# Guest integration (`guest/`)

Files the image pipeline must inject into every fastvm base disk:

| Guest path                    | Source                        | Notes                                   |
|-------------------------------|-------------------------------|-----------------------------------------|
| `/usr/local/bin/fastvm-agent` | `go build ./cmd/fastvm-agent` | Static binary: `CGO_ENABLED=0`          |
| `/init`                       | `guest/init.sh`               | Mode `0755`, POSIX `sh` (busybox)       |

The kernel is pinned per host and booted with
`console=ttyS0 root=/dev/vda rw init=/init`; `/init` mounts `/proc`, `/sys`,
`/dev`, `/dev/pts` and `/dev/shm`, then `exec`s the agent as PID 1.

## Agent

- Listens on vsock port **1024** (`internal/agent.VsockPort`) using the
  Firecracker host-side Unix socket bridge. No network interface is required.
- Wire protocol: newline-delimited JSON, one `Request` per line host→guest and
  one `Response` or `Event` per line guest→host (`internal/agent/proto.go`).
- On every new connection it sends `{"type":"ready"}`, then replays the current
  workload state (`workload_started` / `workload_exit`) and the busy advisory.
- After a Firecracker snapshot restore the vsock connection resets; the host
  client reconnects lazily on the next request (best-effort, one retry).

## Operations

| Op          | Fields                                   | Result |
|-------------|------------------------------------------|--------|
| `ready`     | –                                        | ack (also sent as an event on connect) |
| `exec`      | `argv`, `env`, `stdin`, `timeout_ms`     | sync `Response{exit_code,stdout,stderr,error}` |
| `exec` detach | `argv`, `env`, `detach:true`           | starts the sandbox workload in the background, emits `workload_started` then `workload_exit` |
| `signal`    | `signal` (e.g. `SIGTERM`)                | signals the running detached workload's process group |
| `touch`     | –                                        | emits a `touch` event |
| `busy`      | `busy` bool                              | sets the `workload_busy` advisory and emits an event |
| `heartbeat` | –                                        | ack |

`exec` output is capped at 1 MiB per stream (with a truncation marker).
Detached workload stdout/stderr is appended to `/var/log/fastvm/workload.log`
inside the guest (falling back to the serial console when the path is not
writable); it is deliberately not part of the activity signal.

## Guest networking

If (and only if) the kernel command line carries

```
fastvm.ip=10.77.0.2/30 fastvm.gw=10.77.0.1
```

the agent configures `eth0` before it starts the workload and before it reports
`ready`: assigns the IPv4 address/prefix, brings the link up, and installs the
default route via pure Go ioctls + one NETLINK_ROUTE message (no `ip`/`route`
tools required). Both keys are absent for TAP-less sandboxes and the agent then
leaves the NIC untouched; a malformed key is logged and ignored. The daemon only
emits these keys when the sandbox has a TAP (docs/DESIGN.md §6.6).

## Optional workload

`/etc/fastvm/workload.json` is read once at agent startup and the workload is
started detached. The file is written by the image builder or by the daemon
before boot (for `fastvm run -- cmd...` it is usually injected when creating the
CoW volume).

```json
{
  "argv": ["nginx", "-g", "daemon off;"],
  "env": {"NGINX_ENTRYPOINT_QUIET_LOGS": "1"},
  "dir": "/srv"
}
```

- `argv` is required and must not be empty.
- `env` entries override the inherited environment.
- `dir` is the working directory (optional).
- The workload runs in its own process group and is the target of `signal`.
- A missing or malformed config is logged and ignored; the VM still boots.

## Limitations (v0.1)

- The agent only reaps its own children (workload + exec sessions). Other
  processes reparented to PID 1 are not reaped; images must not spawn
  unsupervised daemons.
- Detached workload output is not streamed over vsock; read it with
  `exec cat /var/log/fastvm/workload.log` or via the image's own logging.
- The agent waits briefly for the virtio-vsock misc device (`/dev/vsock`) to
  appear (the device may probe asynchronously); `/init` also runs `mdev -s` and
  falls back to `mknod` from `/sys/class/misc/vsock/dev`.
- The serial console is not re-attached after a Firecracker snapshot restore
  (Firecracker only accepts `/serial` configuration before `InstanceStart`).
  Filesystem state and vsock work normally after restore.
