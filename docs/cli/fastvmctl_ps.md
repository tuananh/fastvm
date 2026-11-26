# FASTVMCTL-PS(1)

## NAME

fastvmctl-ps - list sandboxes

## SYNOPSIS

```text
fastvmctl ps
```

## DESCRIPTION

Lists sandboxes with ID, name, state, image digest, IP, vCPUs, and memory.

This is an inspection command and does not wake sleeping sandboxes. To run a
command inside a sandbox, use `fastvmctl exec`; that wakes it first.

## OPTIONS

No command-specific options.

## EXAMPLES

```sh
fastvmctl ps
```

## SEE ALSO

[fastvmctl](fastvmctl.md), [fastvmctl get](fastvmctl_get.md)
