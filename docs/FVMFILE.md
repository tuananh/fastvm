# `.fvmfile` declarative sandbox config

`.fvmfile` is a strict YAML description of a sandbox. Unknown fields are
rejected so typos are caught early. Load one with:

```sh
fastvmctl run -f examples/coding-agents/codex.fvmfile
fastvmctl run -f examples/coding-agents/codex.fvmfile -it
fastvmctl run -f examples/coding-agents/codex.fvmfile -d
# Claude example:
fastvmctl run -f examples/coding-agents/claude.fvmfile -it
# GitHub Copilot example:
fastvmctl run -f examples/coding-agents/copilot.fvmfile -it
# Node.js REPL example:
fastvmctl run -f examples/nodejs.fvmfile -it
# Docker-in-VM example:
fastvmctl run -d -f examples/docker.fvmfile
```

## Example

```yaml
version: 1
name: codex
image: node:26-slim
entrypoint: ["codex"]

user_data: |
  #!/bin/sh
  set -eu
  npm install -g --no-fund --no-audit @openai/codex

env:
  OPENAI_API_KEY: "${OPENAI_API_KEY}"

network:
  enabled: true
  allow_hosts:
    - registry.npmjs.org
    - api.openai.com
    - auth.openai.com

resources:
  vcpus: 2
  memory_mb: 4096

no_auto_sleep: true
```

## Fields

| Field | Description |
|---|---|
| `version` | Optional; currently `1`. |
| `name` | Sandbox name. Defaults to the filename without `.fvmfile`. |
| `image` | Required OCI image or base disk reference; pulled on demand. |
| `entrypoint` | Fixed guest argv, like Docker's `ENTRYPOINT`. |
| `command` | Default arguments appended to `entrypoint`, like Docker's `CMD`. Without `entrypoint`, it is the full guest argv. |
| `workdir` | Guest working directory for the command, `exec`, and shell sessions. Must be an absolute clean path. |
| `env` | Guest environment for the workload, user_data, exec, and shell sessions. `${NAME}` is expanded from the host environment; unset references are an error. |
| `volumes` | Host share (`host`, `guest`, `read_only`) or data volume (`size`, `mount`, optional `name`). |
| `ports` | `host`, `guest`, optional `protocol` (`tcp`/`udp`). |
| `network.enabled` | Allocate a guest NIC. |
| `network.allow_hosts` | Egress allowlist; implies `enabled: true`. |
| `network.auto_publish` | Discover and publish guest TCP listeners; implies `enabled: true`. See [auto-publishing ports](AUTO_PUBLISH.md). |
| `resources.vcpus` / `resources.memory_mb` | Sandbox sizing; 0 uses daemon defaults. |
| `no_auto_sleep` | Disable automatic idle sleep. |
| `user_data` | Shell script run once on first boot, before `entrypoint`/`command`. |
| `user_data_file` | Script file, resolved relative to the `.fvmfile`. Mutually exclusive with `user_data`. |

CLI flags override the corresponding `.fvmfile` fields. `--name` overrides the
file name. `--env` merges per key (CLI wins), and `--volume` wins for a matching
guest path. Other creation flags (`--image`, `--vcpus`, `--memory`, `--publish`,
`--net`/`--no-net`, `--allow-host`, `--no-auto-sleep`) replace the file value
only when explicitly passed.

## Entrypoint and command

These follow Docker's `ENTRYPOINT` / `CMD` split:

```yaml
name: codex
image: node:26-slim
entrypoint: ["codex"]
command: ["--help"]
```

- `entrypoint` is the fixed executable and arguments.
- `command` is the default argument list, used when no CLI args are given.
- CLI args after `--` replace `command` and are appended to `entrypoint`.
- `-w/--workdir` sets the guest working directory for the command and shell.
`--auto-publish` discovers guest TCP listeners on ports >= 1024 and maps them 1:1
on `127.0.0.1` at the same host port. Ports below 1024 are never auto-published;
use an explicit `--publish/-p` mapping for those. If a host port is occupied,
the mapping is skipped, warned once, and retried later; it is never reassigned
to another port. Mapping, unmapping, and skip decisions emit sandbox events
(`fastvmctl events`).
`-v/--volume` supports host shares (`host[:guest][:ro]`; a single host path
mounts at the same path in the guest) and writable block data volumes
(`size=8GiB,mount=/data`; mount defaults to `/data`). Data volumes are cloned
from a blank ext4 template, are destroyed with the sandbox, and disable
automatic sleep and snapshots for now.

```yaml
volumes:
  - size: 8GiB
    mount: /data
  - size: 2GiB
    mount: /scratch
  - host: /host/project
    guest: /workspace
    read_only: true
```
- `--entrypoint <cmd>` overrides `entrypoint` for one run, like Docker:

```sh
fastvmctl run -f examples/coding-agents/copilot.fvmfile -it --entrypoint bash
```

| .fvmfile | CLI | Final argv |
|---|---|---|
| `entrypoint: [codex]`, `command: [--help]` | — | `codex --help` |
| `entrypoint: [codex]`, `command: [--help]` | `-- exec` | `codex exec` |
| `command: [sleep, 10]` | — | `sleep 10` |
| `command: [sleep, 10]` | `-- sh` | `sh` |

The last two rows preserve the previous behavior for files that only use
`command`. If an entrypoint is present, CLI args no longer replace it; use
`fastvmctl exec` or a different `.fvmfile` to run another program.

## First-boot user data

`user_data` is a cloud-init-style first-boot script without a cloud-init
dependency. It runs once per sandbox, after boot and before `entrypoint`/`command`:

```yaml
user_data: |
  #!/bin/sh
  set -eu
  npm install -g --no-fund --no-audit @openai/codex
```

Or load it from a file next to the `.fvmfile`:

```yaml
user_data_file: ./setup.sh
```

Details:

- Runs with `/bin/sh -s` as root and inherits the top-level `env`.
- Maximum size: 1 MiB.
- A marker at `/var/lib/fastvm/user-data.done` prevents reruns on resume,
  wake, or snapshot restore.
- Failure aborts sandbox creation; the script's stderr is reported.
- Not supported with `--no-start`.
