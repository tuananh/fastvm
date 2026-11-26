//go:build linux

package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// ensureGuestFilesystems mounts the pseudo-filesystems the agent needs. Some
// hardened/minimal images (for example dhi.io/codex) do not ship `mount`,
// `mdev`, or even a normal shell, so the injected init script cannot mount
// /proc, /sys, /dev/pts, or /dev/shm. The static Go agent runs as PID 1 with
// CAP_SYS_ADMIN, so it performs those mounts itself with syscalls.
func ensureGuestFilesystems() {
	if os.Getpid() != 1 {
		log.Printf("fastvm-agent: not PID 1; skipping guest filesystem mounts")
		return
	}
	mountRuntimeFilesystems()
	if initramfsMode() {
		if err := mountRootAndEnter(); err != nil {
			log.Printf("fastvm-agent: fatal initramfs root: %v", err)
			os.Exit(1)
		}
		// The initramfs mounts are hidden after chroot; recreate them inside
		// the OCI rootfs before mounting volumes and starting the workload.
		mountRuntimeFilesystems()
	}
	mountCmdlineVolumes()
	mountCmdlineDataVolumes()
}

// initramfsMode reports whether the agent is running from an initramfs and must
// mount the OCI root disk itself.
func initramfsMode() bool {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return false
	}
	for _, field := range strings.Fields(string(data)) {
		if field == "fastvm.initramfs=1" {
			return true
		}
	}
	return false
}

// mountRootAndEnter mounts the root disk read-write and chroots into it.
func mountRootAndEnter() error {
	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return err
	}
	rootDev := "/dev/vda"
	for _, field := range strings.Fields(string(cmdline)) {
		if strings.HasPrefix(field, "root=") {
			if value := strings.TrimPrefix(field, "root="); value != "" {
				rootDev = value
			}
			break
		}
	}
	if !strings.HasPrefix(rootDev, "/dev/") {
		return fmt.Errorf("unsupported root device %q", rootDev)
	}
	if err := os.MkdirAll("/newroot", 0o755); err != nil {
		return err
	}
	if err := unix.Mount(rootDev, "/newroot", "ext4", 0, ""); err != nil {
		return fmt.Errorf("mount %s: %w", rootDev, err)
	}
	if err := unix.Chroot("/newroot"); err != nil {
		return fmt.Errorf("chroot: %w", err)
	}
	if err := os.Chdir("/"); err != nil {
		return err
	}
	return nil
}

// mountRuntimeFilesystems mounts proc/sys/dev and the runtime tmpfs mounts.
func mountRuntimeFilesystems() {
	_ = os.MkdirAll("/proc", 0o755)
	_ = os.MkdirAll("/sys", 0o755)
	_ = os.MkdirAll("/dev", 0o755)

	mounts := []struct {
		source, target, fstype string
		flags                  uintptr
		data                   string
	}{
		{"proc", "/proc", "proc", 0, ""},
		{"sysfs", "/sys", "sysfs", 0, ""},
		{"devtmpfs", "/dev", "devtmpfs", 0, "mode=0755"},
	}
	for _, m := range mounts {
		if err := unix.Mount(m.source, m.target, m.fstype, m.flags, m.data); err != nil &&
			!errors.Is(err, unix.EBUSY) {
			log.Printf("fastvm-agent: mount %s on %s: %v", m.fstype, m.target, err)
		}
	}

	// devtmpfs normally creates /dev/pts and /dev/shm; make sure they exist
	// even when the image had a partially populated /dev.
	_ = os.MkdirAll("/dev/pts", 0o755)
	_ = os.MkdirAll("/dev/shm", 0o755)
	if err := unix.Mount("devpts", "/dev/pts", "devpts", 0, "mode=0620,ptmxmode=0666"); err != nil &&
		!errors.Is(err, unix.EBUSY) {
		log.Printf("fastvm-agent: mount devpts on /dev/pts: %v", err)
	}
	if err := unix.Mount("tmpfs", "/dev/shm", "tmpfs", 0, "mode=1777"); err != nil &&
		!errors.Is(err, unix.EBUSY) {
		log.Printf("fastvm-agent: mount tmpfs on /dev/shm: %v", err)
	}
	_ = os.MkdirAll("/run", 0o755)
	if err := unix.Mount("tmpfs", "/run", "tmpfs", 0, "mode=0755"); err != nil &&
		!errors.Is(err, unix.EBUSY) {
		log.Printf("fastvm-agent: mount tmpfs on /run: %v", err)
	}
	_ = os.MkdirAll("/sys/fs/cgroup", 0o755)
	if err := unix.Mount("cgroup2", "/sys/fs/cgroup", "cgroup2", 0, ""); err != nil &&
		!errors.Is(err, unix.EBUSY) {
		log.Printf("fastvm-agent: mount cgroup2 on /sys/fs/cgroup: %v", err)
	}

	// Some minimal devtmpfs setups do not expose the standard nodes. Create
	// them from the well-known Linux major/minor numbers if missing.
	ensureCharDevice("/dev/null", 1, 3)
	ensureCharDevice("/dev/zero", 1, 5)
	ensureCharDevice("/dev/full", 1, 7)
	ensureCharDevice("/dev/random", 1, 8)
	ensureCharDevice("/dev/urandom", 1, 9)
	ensureCharDevice("/dev/tty", 5, 0)
	ensureCharDevice("/dev/console", 5, 1)
	ensureCharDevice("/dev/ptmx", 5, 2)
	// virtio-vsock can probe after devtmpfs is populated; recover the node
	// from sysfs if needed.
	if data, err := os.ReadFile("/sys/class/misc/vsock/dev"); err == nil {
		var major, minor uint32
		if _, err := fmt.Sscanf(string(data), "%d:%d", &major, &minor); err == nil {
			ensureCharDevice("/dev/vsock", major, minor)
		}
	}

}

// mountCmdlineVolumes mounts virtio-fs shares described by
// fastvm.volume.<n>=<tag>:<guest_path>:<ro|rw> on the kernel cmdline. This used
// to live in the shell init; doing it here keeps hardened images without
// mount utilities working and keeps custom init scripts optional.
func mountCmdlineVolumes() {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		log.Printf("fastvm-agent: read cmdline for volumes: %v", err)
		return
	}
	for _, field := range strings.Fields(string(data)) {
		spec, ok := strings.CutPrefix(field, "fastvm.volume.")
		if !ok {
			continue
		}
		_, rest, ok := strings.Cut(spec, "=")
		if !ok {
			continue
		}
		parts := strings.SplitN(rest, ":", 3)
		if len(parts) != 3 {
			log.Printf("fastvm-agent: invalid volume spec %q", field)
			continue
		}
		tag, guest, mode := parts[0], filepath.Clean(parts[1]), parts[2]
		if tag == "" || guest == "" || !filepath.IsAbs(guest) || guest == "/" {
			log.Printf("fastvm-agent: invalid volume spec %q", field)
			continue
		}
		if err := os.MkdirAll(guest, 0o755); err != nil {
			log.Printf("fastvm-agent: create volume mountpoint %s: %v", guest, err)
			continue
		}
		flags := uintptr(0)
		if mode == "ro" {
			flags |= unix.MS_RDONLY
		}
		if err := unix.Mount(tag, guest, "virtiofs", flags, ""); err != nil &&
			!errors.Is(err, unix.EBUSY) {
			log.Printf("fastvm-agent: mount virtiofs %s at %s: %v", tag, guest, err)
		}
	}
}

// mountCmdlineDataVolumes mounts fastvm.data.<n>=<mount> block volumes. The
// root disk is vda, so data volume n is vd(b+n).
func mountCmdlineDataVolumes() {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		log.Printf("fastvm-agent: read cmdline for data volumes: %v", err)
		return
	}
	for _, field := range strings.Fields(string(data)) {
		spec, ok := strings.CutPrefix(field, "fastvm.data.")
		if !ok {
			continue
		}
		idxStr, mount, ok := strings.Cut(spec, "=")
		if !ok {
			continue
		}
		idx, err := strconv.Atoi(idxStr)
		if err != nil || idx < 0 || idx > 25 {
			log.Printf("fastvm-agent: invalid data volume spec %q", field)
			continue
		}
		mount = filepath.Clean(mount)
		if !filepath.IsAbs(mount) || mount == "/" {
			log.Printf("fastvm-agent: invalid data volume mount %q", mount)
			continue
		}
		dev := "/dev/vd" + string(rune('b'+idx))
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(dev); err == nil {
				break
			}
			if time.Now().After(deadline) {
				log.Printf("fastvm-agent: data device %s did not appear", dev)
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err := os.MkdirAll(mount, 0o755); err != nil {
			log.Printf("fastvm-agent: create data mountpoint %s: %v", mount, err)
			continue
		}
		if err := unix.Mount(dev, mount, "ext4", 0, ""); err != nil &&
			!errors.Is(err, unix.EBUSY) {
			log.Printf("fastvm-agent: mount data volume %s at %s: %v", dev, mount, err)
		}
	}
}

func ensureCharDevice(path string, major, minor uint32) {
	if _, err := os.Stat(path); err == nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Printf("fastvm-agent: create %s dir: %v", path, err)
		return
	}
	if err := unix.Mknod(path, unix.S_IFCHR|0o666, int(unix.Mkdev(major, minor))); err != nil {
		log.Printf("fastvm-agent: create %s: %v", path, err)
	}
}
