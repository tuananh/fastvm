# FASTVMCTL-IMAGE(1)

## NAME

fastvmctl-image - manage base images

## SYNOPSIS

```text
fastvmctl image pull <image> [--size-gb N]
fastvmctl image ls
fastvmctl image rm <tag|digest>... [--force]
```

## DESCRIPTION

Base disks are ext4 images created on the host with `mke2fs`; per-sandbox root
resizing additionally uses `e2fsck` and `resize2fs` from e2fsprogs.

`ls` reports both the virtual disk size (`SIZE`) and the sparse file allocation
on disk (`USED`). The virtual size is the ext4 capacity, not the bytes stored.

Pulls OCI images directly into the base-disk store, lists cached base disks, and
removes images. Images are cached by source and compressed layers by digest.

## OPTIONS

### pull

| Option | Description |
|---|---|
| `--size-gb N` | Base disk size in GiB (default: daemon `storage.default_image_size_gb`, 8) |

### rm

| Option | Description |
|---|---|
| `--force` | Delete even while sandboxes reference the image |

## EXAMPLES

```sh
fastvmctl image pull alpine
fastvmctl image pull python:3-alpine --size-gb 16
fastvmctl image ls
fastvmctl image rm alpine
```

## SEE ALSO

[fastvmctl](fastvmctl.md), [fastvmctl run](fastvmctl_run.md)
