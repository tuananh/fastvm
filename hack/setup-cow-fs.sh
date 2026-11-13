#!/usr/bin/env bash
# Set up a local XFS filesystem with reflink=1 for fastvm CoW runtime volumes.
#
# A file-backed loopback XFS is used because ext4 cannot do FICLONE/reflink
# clones. The data directory lives at /tmp/fastvm-cow/data by default; the
# backing image is kept under /var/tmp so the filesystem data does not consume
# tmpfs RAM. Run on the host as root:
#
#   sudo ./hack/setup-cow-fs.sh
#   sudo ./hack/setup-cow-fs.sh status
#   sudo ./hack/setup-cow-fs.sh teardown
#
# Then start fastvm with this data dir (or set data_dir: /tmp/fastvm-cow/data
# in a config copy):
#
#   sudo bin/fastvmd -config hack/config.yaml \
#     -data-dir /tmp/fastvm-cow/data -socket /tmp/fastvm-demo.sock
#
# Environment overrides:
#   FASTVM_COW_IMG   backing image        (default /var/tmp/fastvm-cow.img)
#   FASTVM_COW_MNT   mount point          (default /tmp/fastvm-cow)
#   FASTVM_COW_DATA  data directory       (default $MNT/data)
#   FASTVM_COW_SIZE  image size           (default 16G)
#   FASTVM_COW_FORCE 1 to reformat an existing non-XFS image
set -euo pipefail

IMG="${FASTVM_COW_IMG:-/var/tmp/fastvm-cow.img}"
MNT="${FASTVM_COW_MNT:-/tmp/fastvm-cow}"
DATA="${FASTVM_COW_DATA:-$MNT/data}"
SIZE="${FASTVM_COW_SIZE:-16G}"
LABEL="${FASTVM_COW_LABEL:-fastvm-cow}"
FORCE="${FASTVM_COW_FORCE:-0}"
CMD="${1:-setup}"

die() { echo "error: $*" >&2; exit 1; }
usage() {
	sed -n '2,28p' "$0" | sed 's/^# \{0,1\}//'
}

need() {
	command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

require_root() {
	[[ "$(id -u)" -eq 0 ]] || die "run as root (sudo $0 $CMD)"
}

validate_paths() {
	[[ -n "$IMG" && "$IMG" != "/" ]] || die "refusing to use image path $IMG"
	[[ -n "$MNT" && "$MNT" != "/" ]] || die "refusing to use mount point $MNT"
	[[ "$DATA" == "$MNT/"* ]] || die "FASTVM_COW_DATA must be under FASTVM_COW_MNT"
}

ensure_data_dir() {
	mkdir -p "$DATA"
	chmod 0755 "$DATA"
	# Give the invoking user access when the script is run through sudo.
	if [[ -n "${SUDO_UID:-}" && -n "${SUDO_GID:-}" ]]; then
		chown "$SUDO_UID:$SUDO_GID" "$MNT" "$DATA" 2>/dev/null || true
	fi
}

is_mounted() {
	# findmnt -T resolves the containing filesystem even when MNT isn't a
	# mount point, so compare the exact target.
	[[ "$(findmnt -n -T "$MNT" -o TARGET 2>/dev/null || true)" == "$MNT" ]]
}

xfs_has_reflink() {
	xfs_info "$MNT" 2>/dev/null | grep -q 'reflink=1'
}

verify_cow() {
	xfs_has_reflink || die "XFS at $MNT does not have reflink=1 (recreate the image with mkfs)"
	local probe
	probe="$(mktemp -d "$MNT/.reflink-probe.XXXXXX")"
	dd if=/dev/zero of="$probe/a" bs=1M count=8 status=none
	cp --reflink=always "$probe/a" "$probe/b" || die "FICLONE failed on $MNT"
	rm -rf "$probe"
	echo "reflink/FICLONE verified on $MNT"
}

loop_for_image() {
	losetup -j "$IMG" 2>/dev/null | head -n1 | cut -d: -f1
}

cmd_status() {
	need findmnt
	need xfs_info
	if ! is_mounted; then
		echo "$MNT is not mounted"
		[[ -f "$IMG" ]] && echo "image exists: $IMG ($(du -h --apparent-size "$IMG" | cut -f1) apparent, $(du -h "$IMG" | cut -f1) allocated)"
		return 1
	fi
	findmnt -T "$DATA" -o TARGET,SOURCE,FSTYPE,OPTIONS
	xfs_info "$MNT" | grep -q 'reflink=1' && echo "reflink=1: yes" || echo "reflink=1: NO"
	du -sh "$DATA" 2>/dev/null || true
}

cmd_setup() {
	require_root
	for c in truncate losetup mkfs.xfs mount findmnt xfs_info blkid cp dd; do need "$c"; done
	validate_paths

	if is_mounted; then
		local fst
		fst="$(findmnt -n -T "$MNT" -o FSTYPE)"
		[[ "$fst" == "xfs" ]] || die "$MNT is already mounted with filesystem $fst (want xfs)"
		echo "$MNT is already mounted as XFS; ensuring $DATA exists"
		ensure_data_dir
		verify_cow
		echo "ready: data_dir=$DATA"
		return 0
	fi

	local image_existed=0
	if [[ -f "$IMG" ]]; then
		image_existed=1
	fi
	# Mounting over a non-empty directory hides its contents (and any running
	# fastvm state that lives there). Refuse unless explicitly forced.
	if [[ -d "$MNT" && -n "$(ls -A "$MNT" 2>/dev/null || true)" && "$FORCE" != "1" ]]; then
		die "$MNT is not empty and is not a mount point; move its contents first (or set FASTVM_COW_FORCE=1)"
	fi
	mkdir -p "$(dirname "$IMG")" "$MNT"
	if [[ "$image_existed" -eq 0 ]]; then
		echo "creating sparse XFS image $IMG ($SIZE)"
		truncate -s "$SIZE" "$IMG"
	fi

	local loop
	loop="$(loop_for_image)"
	if [[ -z "$loop" ]]; then
		echo "attaching loop device to $IMG"
		loop="$(losetup -f --show "$IMG")"
	fi
	echo "loop device: $loop"

	local fstype=""
	fstype="$(blkid -o value -s TYPE "$loop" 2>/dev/null || true)"
	if [[ "$fstype" != "xfs" ]]; then
		if [[ "$image_existed" -eq 1 && "$FORCE" != "1" ]]; then
			die "$IMG exists but is not XFS (type ${fstype:-unknown}); set FASTVM_COW_FORCE=1 to recreate it"
		fi
		echo "formatting $loop as XFS with reflink=1"
		mkfs.xfs -f -m reflink=1 -L "$LABEL" "$loop" >/dev/null
	else
		echo "existing XFS filesystem detected on $loop"
	fi

	echo "mounting $loop at $MNT"
	mount -o noatime "$loop" "$MNT"
	ensure_data_dir
	verify_cow
	echo
	echo "CoW filesystem ready"
	echo "  mount:    $MNT"
	echo "  data_dir: $DATA"
	echo "  image:    $IMG (loop $loop)"
	echo
	echo "Use it with a daemon config data_dir: $DATA"
}

cmd_teardown() {
	require_root
	validate_paths
	if is_mounted; then
		echo "unmounting $MNT"
		umount "$MNT"
	fi
	local loop
	while loop="$(loop_for_image)" && [[ -n "$loop" ]]; do
		echo "detaching $loop"
		losetup -d "$loop" || break
	done
	if [[ -f "$IMG" ]]; then
		echo "removing $IMG (all data on the volume is lost)"
		rm -f "$IMG"
	fi
	rmdir "$MNT" 2>/dev/null || true
	echo "teardown complete"
}

case "$CMD" in
	setup) cmd_setup ;;
	status) cmd_status ;;
	teardown) cmd_teardown ;;
	help|-h|--help) usage ;;
	*) usage; exit 2 ;;
esac
