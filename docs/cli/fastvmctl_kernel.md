# FASTVMCTL-KERNEL(1)

## NAME

fastvmctl-kernel - manage Cloud Hypervisor guest kernels

## SYNOPSIS

```text
fastvmctl kernel ls
fastvmctl kernel remote-ls [--limit N]
fastvmctl kernel download [release] [--asset NAME] [--force]
fastvmctl kernel use <release>
```

## DESCRIPTION

Manages guest kernels from `cloud-hypervisor/linux`.

The daemon owns the kernel directory and the `current` symlink:

```text
<daemon kernel dir>/
├── <release>-<asset>
├── <release>-<asset>.sha256
└── current -> <release>-<asset>
```

Daemon config should point at the symlink:

```yaml
kernel_path: /var/lib/fastvm/kernels/current
```

The CLI talks to the daemon, so `download` and `use` do not require sudo. The
daemon writes its own kernel directory and verifies downloads against the
SHA-256 digest published by GitHub before accepting them.

## COMMANDS

### `ls`

List kernels installed in the daemon's directory. The active kernel is marked
with `*`.

### `remote-ls`

List releases and assets available from
[cloud-hypervisor/linux](https://github.com/cloud-hypervisor/linux), including
their SHA-256 digests and sizes. Only direct-boot kernel images for the host
architecture are shown by default (for example `vmlinux-x86_64`, not
`bzImage-x86_64`); use `--arch` to inspect another architecture.

### `download`

Ask the daemon to download and verify one kernel. With no release argument the
latest release is used. The default asset follows the host architecture.

Existing files are reused only when their SHA-256 matches. Use `--force` to
replace a mismatched file.

### `use`

Point the daemon's `current` symlink at a downloaded release. To pin only one
sandbox instead, use `fastvmctl run --kernel <release>`. Use the release
name shown by `kernel ls` and `kernel remote-ls`
(`ch-release-v7.2.8-20261009`). New sandbox boots use the new kernel; existing
sandboxes are untouched.

## OPTIONS

| Option | Description |
|---|---|
| `--limit N` | Number of releases shown by `remote-ls` (default 10) |
| `--arch ARCH` | Architecture filter for `remote-ls`: `amd64`, `arm64`, or `riscv64` (default: host) |
| `--release TAG` | Release tag for `download` (alternative to the positional argument) |
| `--asset NAME` | Asset to download (default: host architecture) |
| `--force` | Re-download even when a file exists |

## EXAMPLES

```sh
fastvmctl kernel remote-ls --limit 5
fastvmctl kernel download
fastvmctl kernel download ch-release-v7.2.8-20261009
fastvmctl kernel use ch-release-v7.2.8-20261009
fastvmctl kernel ls
```

Example output:

```text
   RELEASE                     ASSET           SIZE      SHA256     PATH
*  ch-release-v7.2.8-20261009  vmlinux-x86_64  46.0 MiB  edfeb8a8…  /var/lib/fastvm/kernels/...
```

## SEE ALSO

[fastvmctl](fastvmctl.md), [fastvmctl run](fastvmctl_run.md),
[Getting started](../GETTING_STARTED.md)
