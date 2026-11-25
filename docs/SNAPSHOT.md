# Snapshots

`fastvmctl snapshot` saves a sandbox's RAM/CPU state and writable disk. Restore
it later as a new sandbox with `run --from-snapshot`.

## Create

```sh
fastvmctl snapshot dev
fastvmctl snapshot dev --name before-upgrade
```

The sandbox is paused briefly while the snapshot is captured, then resumed. The
snapshot is printed:

```text
dev-20261121-001234
```

Snapshots live under `<data_dir>/snapshots/<name>`.

## Restore

```sh
fastvmctl run --from-snapshot dev-20261121-001234 --name dev2
```

The restored sandbox gets a new TAP/IP and hostname. The source snapshot is not
modified, and multiple sandboxes can be restored from it.

### Restore restrictions

`run --from-snapshot` cannot be combined with:

```text
--image  --file  --cpu  --memory  --publish  --env  --volume
--net  --allow-host  --auto-publish  --no-auto-sleep
--entrypoint  --workdir  --no-start
```

The stored image, resources, workdir, network/allowlist, and command state come
from the snapshot.

## Manage

```sh
fastvmctl snapshot ls
fastvmctl snapshot rm dev-20261121-001234
```

## What is saved

```text
snapshot.json       fastvm manifest
snapshot/           Cloud Hypervisor config, memory, and device state
disk.img            writable disk
```

## Limitations

- Full snapshots only; no incremental/diff snapshots
- Host-directory volumes are not supported
- The base image must still exist in the image store
- Snapshots are Cloud Hypervisor-version and host-specific; not portable across
  hosts/CPU/VMM versions
- Port mappings, environment, auto-publish, and `no-auto-sleep` are not restored
- Guest RAM is saved, so injected secrets in guest memory are included in the
  snapshot; treat snapshot bundles as secret-bearing
- Changes made after the snapshot are not reflected
