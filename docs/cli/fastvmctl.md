# FASTVMCTL(1)

## NAME

fastvmctl - control fastvmd and its micro-VM sandboxes

## SYNOPSIS

```text
fastvmctl [--socket PATH] <command> [arguments]
```

## DESCRIPTION

`fastvmctl` is the CLI for the fastvm daemon. It creates, runs, pauses, resumes,
copies files into/out of, snapshots, and removes Cloud Hypervisor sandboxes.

Run commands after `--`; otherwise flags are parsed by `fastvmctl`:

```sh
fastvmctl run --image alpine -- uname -a
fastvmctl exec dev -- sh -c 'ps aux'
```

Sandbox arguments accept a full ID, full name, or any unambiguous ID/name prefix:

```sh
fastvmctl exec 128e1f -- uname -a   # unique ID prefix
fastvmctl exec api -- uname -a      # unique name prefix
```

Ambiguous prefixes fail with an error listing the matching sandboxes.

## COMMANDS

| Command | Description |
|---|---|
| [`cp`](fastvmctl_cp.md) | Copy files into or out of a sandbox |
| [`events`](fastvmctl_events.md) | Show recent daemon events |
| [`exec`](fastvmctl_exec.md) | Run a command in a sandbox |
| [`get`](fastvmctl_get.md) | Show one sandbox as JSON |
| [`image`](fastvmctl_image.md) | Manage base images |
| [`kernel`](fastvmctl_kernel.md) | List, download, and verify guest kernels |
| [`pause`](fastvmctl_pause.md) | Pause or offload a sandbox |
| [`ps`](fastvmctl_ps.md) | List sandboxes |
| [`resume`](fastvmctl_resume.md) | Boot or wake a sandbox |
| [`rm`](fastvmctl_rm.md) | Delete sandboxes |
| [`run`](fastvmctl_run.md) | Create, boot, or start a sandbox |
| [`shell`](fastvmctl_shell.md) | Open an interactive shell |
| [`snapshot`](fastvmctl_snapshot.md) | Create/list/remove snapshots |
| [`version`](fastvmctl_version.md) | Show CLI and daemon versions |
| [`completion`](fastvmctl_completion.md) | Generate shell completion |

## GLOBAL OPTIONS

```text
--socket PATH     fastvmd unix socket
                  (default: $FASTVM_SOCKET or /run/fastvm/fastvm.sock)
-h, --help        help
-v, --version     version
```

`--socket` overrides `FASTVM_HOST`.

## ENVIRONMENT

| Variable | Meaning |
|---|---|
| `FASTVM_HOST` | HTTPS API endpoint, e.g. `https://127.0.0.1:8443` |
| `FASTVM_CERT_PATH` | Directory containing `ca.pem`, `cert.pem`, `key.pem` |
| `FASTVM_SOCKET` | Unix socket path |

When `FASTVM_HOST` is unset, the unix socket is used. TCP mode requires mTLS
client files from `FASTVM_CERT_PATH`.

## SEE ALSO

[fastvmctl cp](fastvmctl_cp.md), [fastvmctl run](fastvmctl_run.md),
[fastvmctl exec](fastvmctl_exec.md), [REST API](../API.md),
[Prometheus metrics](../METRICS.md)
