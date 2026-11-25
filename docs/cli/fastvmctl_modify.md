# FASTVMCTL-MODIFY(1)

## NAME

fastvmctl-modify - resize a running sandbox

## SYNOPSIS

```text
fastvmctl modify <sandbox> --cpu N
fastvmctl modify <sandbox> --memory MB
fastvmctl modify <sandbox> --cpu N --memory MB
```

## DESCRIPTION

Changes the live vCPU count or memory size of a running sandbox. Changes are
applied immediately through Cloud Hypervisor.

The sandbox's create-time ceilings limit the values:

- `--cpu` must be between 1 and the sandbox's `--max-cpu`
- `--memory` must be between the Cloud Hypervisor boot memory and `--max-memory`

CPU hotplug is followed by online-ing the new CPUs through the guest agent.
Memory uses virtio-mem hotplug.

Memory cannot be shrunk below the boot allocation by resize; use the virtio
balloon to reduce guest-available memory instead.

The sandbox is woken if it is paused or dormant.

## OPTIONS

| Option | Description |
|---|---|
| `--cpu N` | Desired live vCPU count |
| `--memory MB` | Desired live memory in MiB |

At least one option is required.

## EXAMPLES

```sh
fastvmctl modify web --cpu 4
fastvmctl modify web --memory 1024
fastvmctl modify web --cpu 4 --memory 1024
```

## SEE ALSO

[fastvmctl](fastvmctl.md), [fastvmctl run](fastvmctl_run.md),
[fastvmctl get](fastvmctl_get.md), [fastvmctl ps](fastvmctl_ps.md)
