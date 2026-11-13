# Testing CoW volumes (quick)

fastvm clones a read-only base disk into a writable per-sandbox volume and
records the clone method in `$DATA_DIR/volumes/<sandbox-id>.json`:

```json
{ "base_disk": "sha256:…", "size_bytes": 8589934592, "cow": "reflink" }
```

- **ext4 / tmpfs**: no reflink → `"cow": "copy"` (sparse copy; still isolated).
- **XFS `reflink=1` / Btrfs**: real CoW → `"cow": "reflink"`.

This host's root and `/var/tmp` are ext4, so the steps below create a small
loopback **XFS** image and mount it at `/tmp/fastvm-cow`. The image itself lives
on `/var/tmp` (disk-backed) to avoid consuming RAM in `/tmp` (tmpfs).

> Verified on this host in a privileged container: `mkfs.xfs -m reflink=1`,
> mount, `cp --reflink=always`, `xfs_info` reports `reflink=1`, and `filefrag`
> shows a `shared` extent.

## Requirements (do this first)

| Requirement | Why / check |
|---|---|
| Arch Linux, root/sudo | `losetup`/`mount` and `fastvmd` (TAP/bridge/nftables) need root. |
| `xfsprogs` | `mkfs.xfs`, `xfs_info`. Arch package `xfsprogs`. |
| `util-linux` | `losetup`, `findmnt`, `mount`. |
| `e2fsprogs` | `filefrag` (and `mkfs.ext4` for image builds). |
| `jq` | Read the volume sidecar JSON. |
| `docker` | `make image` builds the demo base disk. |
| `firecracker` + `/dev/kvm` | Boot the sandbox. |
| `go` + `make` | Build fastvm. |

On this host all of these are already installed; nothing was installed by this
guide. If something is missing on another machine:

```sh
sudo pacman -S --needed xfsprogs util-linux e2fsprogs jq
# optional Btrfs alternative: sudo pacman -S --needed btrfs-progs
```

Then build and fetch the guest kernel (once):

```sh
cd /path/to/fastvm
make build
make assets
```

Stop any running `fastvmd` that uses the same data dir/socket before continuing.

## Configuration to update

`hack/config.yaml` is almost usable as-is. These are the values that matter:

| Setting | Value for this test | Why |
|---|---|---|
| `data_dir` | `/tmp/fastvm-cow/data` | **Must be on the mounted XFS**; otherwise fastvm silently uses `copy`. |
| `volume.cow` | `reflink` | Already the default in `hack/config.yaml`; keep it. |
| `socket_path` | `/tmp/fastvm-cow/fastvm.sock` | Optional; avoids clashing with another daemon on `/run`. |
| `kernel_path` / `agent_path` | repo-relative (run from repo root) or absolute | Config paths are resolved from the daemon's working directory. |

You can override `data_dir` and `socket_path` with daemon flags without editing
the file (that's what the commands below do). If you prefer a real config file:

```sh
cp hack/config.yaml /tmp/fastvm-cow/config.yaml
sed -i -e 's|^data_dir:.*|data_dir: /tmp/fastvm-cow/data|' \
       -e 's|^socket_path:.*|socket_path: /tmp/fastvm-cow/fastvm.sock|' \
       /tmp/fastvm-cow/config.yaml
grep -E '^(data_dir|socket_path):|^  cow:' /tmp/fastvm-cow/config.yaml
# data_dir: /tmp/fastvm-cow/data
# socket_path: /tmp/fastvm-cow/fastvm.sock
#   cow: reflink
```

## Step 1 — create the CoW filesystem (important)

This step decides whether you get real CoW. Reflink must be enabled when the
filesystem is created; it cannot be added to an ext4 filesystem later.

```sh
sudo truncate -s 16G /var/tmp/fastvm-cow.img   # sparse backing image
LOOP=$(sudo losetup -f --show /var/tmp/fastvm-cow.img)
sudo mkdir -p /tmp/fastvm-cow
sudo mkfs.xfs -f -m reflink=1 -L fastvm-cow "$LOOP"
sudo mount "$LOOP" /tmp/fastvm-cow
sudo chown "$(id -u):$(id -g)" /tmp/fastvm-cow
```

**Verify before continuing** — all four must pass:

```sh
findmnt -T /tmp/fastvm-cow -o TARGET,SOURCE,FSTYPE
# /tmp/fastvm-cow  /dev/loopN  xfs

xfs_info /tmp/fastvm-cow | grep -o 'reflink=1'
# reflink=1

dd if=/dev/zero of=/tmp/fastvm-cow/a bs=1M count=8 status=none
cp --reflink=always /tmp/fastvm-cow/a /tmp/fastvm-cow/b
# must succeed (no "Operation not supported")

filefrag -v /tmp/fastvm-cow/b | grep -m1 shared
# flags include "shared"

rm /tmp/fastvm-cow/a /tmp/fastvm-cow/b
```

If `xfs_info` lacks `reflink=1`, or `cp --reflink=always` fails, stop: your
kernel/xfsprogs can't do reflink (recreate the image; reflink is mkfs-time only).

## Step 2 — start the daemon with `data_dir` on that mount

Terminal 1 (root; run from the repo root):

```sh
cd /path/to/fastvm
sudo bin/fastvmd -config hack/config.yaml \
  -data-dir /tmp/fastvm-cow/data \
  -socket /tmp/fastvm-cow/fastvm.sock
```

Now verify *where the data dir actually is* (this is what makes the volume CoW):

```sh
findmnt -T /tmp/fastvm-cow/data -o TARGET,SOURCE,FSTYPE
# must be xfs on /dev/loopN — never ext4/tmpfs
```

If this shows ext4 or tmpfs, the daemon is using the wrong data dir; fix before
building the sandbox.

## Step 3 — build the image and boot a sandbox

Terminal 2:

```sh
SOCK=/tmp/fastvm-cow/fastvm.sock
cd /path/to/fastvm

sudo make image FASTVM_SOCKET="$SOCK"     # builds hack/Dockerfile.example -> demo:latest

sudo bin/fastvm -socket "$SOCK" run \
  --image demo:latest --name cowtest --no-auto-sleep -- \
  /bin/sh -c 'while true; do sleep 5; done'
```

## Step 4 — verify the volume is really on the CoW filesystem

```sh
SOCK=/tmp/fastvm-cow/fastvm.sock
ID=$(sudo bin/fastvm -socket "$SOCK" get cowtest | jq -r .id)
META=/tmp/fastvm-cow/data/volumes/$ID.json

sudo jq . "$META"                                 # "cow": "reflink"

VOL=$(sudo jq -r .path "$META")
BASE=/tmp/fastvm-cow/data/images/$(sudo jq -r .base_disk "$META" | sed 's/^sha256://').img

# Both the base and the volume must be on the same XFS loop device:
findmnt -T "$BASE" -o TARGET,SOURCE,FSTYPE
findmnt -T "$VOL"  -o TARGET,SOURCE,FSTYPE
# both: xfs /dev/loopN

sudo du -B1 --apparent-size "$BASE" "$VOL"        # ~8 GiB logical
sudo du -B1 "$BASE" "$VOL"                        # small allocated size
sudo filefrag -v "$VOL" | grep -c shared          # > 0
```

Checklist — all must be true:
- [ ] `data_dir` mounts as **xfs** on `/dev/loopN`
- [ ] sidecar says `"cow": "reflink"`
- [ ] base and volume are on the same XFS loop device
- [ ] `filefrag -v "$VOL"` reports **shared** extents

If the sidecar says `copy`, the data dir is not on the XFS mount; go back to
Step 2.

## Step 5 — prove copy-on-write

Same shell as Step 4 (`$VOL`, `$BASE`, `$SOCK` still set):

```sh
sudo sha256sum "$BASE" > /tmp/base.sha

# write 64 MiB inside the guest
sudo bin/fastvm -socket "$SOCK" exec cowtest -- /bin/sh -c \
  'dd if=/dev/zero of=/cowtest.bin bs=1M count=64 2>/dev/null; sync'

sudo du -B1 "$VOL"                 # grew by ~64 MiB
sudo sha256sum -c /tmp/base.sha    # base disk unchanged
sudo filefrag -v "$VOL" | grep -c shared   # fewer shared extents than before
```

That's CoW: only the touched extents were copied into the volume; the shared
base image is untouched.

Optional — the writes survive deep sleep/wake:

```sh
sudo bin/fastvm -socket "$SOCK" pause cowtest --deep
sudo bin/fastvm -socket "$SOCK" exec cowtest -- /bin/sh -c 'wc -c /cowtest.bin'
# 67108864 (exec auto-wakes)
```

## Step 6 — clean up

```sh
sudo bin/fastvm -socket /tmp/fastvm-cow/fastvm.sock rm cowtest
# stop fastvmd with Ctrl-C in terminal 1

sudo umount /tmp/fastvm-cow
sudo losetup -j /var/tmp/fastvm-cow.img    # prints /dev/loopN
sudo losetup -d /dev/loopN                  # use the device from above
sudo rm -rf /tmp/fastvm-cow /var/tmp/fastvm-cow.img /tmp/base.sha
```

## If you only have ext4/tmpfs

Same flow with `data_dir` on ext4 (e.g. `/var/tmp/fastvm-cow`) and no Step 1
loop mount. The sidecar will say `"cow": "copy"`, `filefrag` shows no shared
extents, and the volume is a sparse copy — correct, but not CoW sharing.

## Unit tests (optional)

```sh
cd /path/to/fastvm
go test ./internal/volume/ -v -run TestCreate
# ext4: "clone mode ... copy (reflink supported: false)"; reflink test skips
```

To run the reflink tests for real, copy the checkout onto the mounted XFS (the
test helper creates temp dirs next to the package source):

```sh
sudo cp -a /path/to/fastvm /tmp/fastvm-cow/src
sudo chown -R "$(id -u):$(id -g)" /tmp/fastvm-cow/src
cd /tmp/fastvm-cow/src && go test ./internal/volume/ -v -run TestCreate
```

## Troubleshooting

| Symptom | Fix |
|---|---|
| `cp --reflink=always`: `Operation not supported` | Not a reflink filesystem (ext4/tmpfs). Use the XFS loop from Step 1. |
| `xfs_info` has no `reflink=1` | Reflink must be set at `mkfs` time and needs a recent kernel/xfsprogs; recreate the image. |
| Sidecar `"cow": "copy"` on XFS | `data_dir` isn't on the XFS mount, or base/volume are on different filesystems (`EXDEV`). Check `findmnt -T "$DATA_DIR"`. |
| `losetup`/`mount` says busy | `sudo losetup -a`, `findmnt`, then `umount` and `losetup -d`. |
| `permission denied` on sidecar/volume | Root-owned 0600; inspect with `sudo`. |
| `dm-thin` config error | Not implemented; use `reflink` or `copy`. |
| `/var/tmp` image is large | `truncate` makes it sparse; XFS allocates only as data is written. `rm` the image after detaching the loop. |
