# FASTVMCTL-RUN(1)

## NAME

fastvmctl-run - create, boot, or start a sandbox

## SYNOPSIS

```text
fastvmctl run [flags] -- <command> [args...]
fastvmctl run [flags]
fastvmctl run --name <sandbox>
```

## DESCRIPTION

Creates and boots a sandbox from an OCI image or an `.fvmfile`, or boots an
existing sandbox by name.

Without `-d`, `run` waits for the command, streams stdout/stderr, then removes
the one-off sandbox. With `-d`, it keeps the sandbox and returns immediately.
With no command, it leaves the sandbox running and prints ID/state/name.
`--no-start` creates it without booting.

## OPTIONS

| Option | Description |
|---|---|
| `-f, --file FILE` | Load sandbox config from an `.fvmfile` |
| `--image IMAGE` | OCI image or base disk tag/digest |
| `--name NAME` | Sandbox name/ID; boots an existing sandbox when no image/file is given |
| `--from-snapshot NAME` | Boot a new sandbox from a saved snapshot |
| `-d, --detach` | Run in background and print sandbox ID |
| `--no-start` | Create without booting |
| `-i, --interactive` | Attach an interactive PTY |
| `-t, --tty` | Allocate a TTY |
| `--net` | Enable networking |
| `--no-net` | Disable networking |
| `--allow-host HOST` | Restrict egress to host; implies `--net` |
| `--publish HOST:GUEST[/tcp\|udp]` | Publish a port |
| `--auto-publish` | Discover guest TCP listeners ≥1024 and map 1:1 to `127.0.0.1` |
| `--vcpus N` | vCPUs |
| `--memory MB` | Memory in MiB |
| `--env K=V` | Workload environment |
| `-v, --volume SPEC` | Host volume `host[:guest][:ro]` or data volume `size=8GiB,mount=/data` |
| `-w, --workdir DIR` | Guest working directory |
| `--entrypoint CMD` | Override image/fvmfile entrypoint |
| `--no-auto-sleep` | Never auto-sleep |

With `-f`, explicitly passed flags override fvmfile values. `--env` merges by
key and `--volume` overrides a matching guest path.

## EXAMPLES

```sh
fastvmctl run --net --image alpine -- uname -a
fastvmctl run --image alpine --volume size=8GiB,mount=/data
fastvmctl run --image alpine --volume /host/project:/workspace
fastvmctl run --net -it --image alpine -- /bin/sh
fastvmctl run -f examples/coding-agents/codex.fvmfile
fastvmctl run --image alpine --name agent --no-start
fastvmctl run --from-snapshot original-vm-20261119-182000 --name copy-1
```

## SEE ALSO

[fastvmctl](fastvmctl.md), [fastvmctl exec](fastvmctl_exec.md),
[fastvmctl ps](fastvmctl_ps.md), [FVMFILE](../FVMFILE.md)
