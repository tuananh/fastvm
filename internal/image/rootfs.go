package image

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"syscall"
)

// Relative (rootfs-local) locations of the injected runtime pieces.
const (
	agentGuestRel = "usr/local/bin/fastvm-agent"
	initGuestRel  = "init"
)

// builtinInit is injected as /init when the caller does not supply an init
// script. It is intentionally tiny and dependency-free: it mounts the pseudo
// filesystems a guest agent needs and then hands over to the agent, falling
// back to a shell when no agent was injected (useful for ingest tests and
// debugging).
const builtinInit = `#!/bin/sh
# Minimal fastvm guest init, injected by fastvm when no init script is given.
# The agent (PID 1) inherits this PATH; without it exec.LookPath cannot resolve
# tools installed in /usr/local/bin (npm/npx/coding-agent CLIs).
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

mount -t proc proc /proc 2>/dev/null || true
mount -t sysfs sysfs /sys 2>/dev/null || true
mount -t devtmpfs devtmpfs /dev 2>/dev/null || true
mkdir -p /dev/pts /dev/shm /run /tmp
mount -t devpts devpts /dev/pts 2>/dev/null || true
mount -t tmpfs tmpfs /dev/shm 2>/dev/null || true
if [ -x /usr/local/bin/fastvm-agent ]; then
	exec /usr/local/bin/fastvm-agent
fi
exec /bin/sh
`

// modeBits returns the permission and special (setuid/setgid/sticky) bits of
// mode. Only these bits are meaningful in a filesystem inode.
func modeBits(mode fs.FileMode) fs.FileMode {
	return mode & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
}

// copyTree copies the directory tree src into the existing directory dst.
//
// It preserves regular files, directories, symlinks, permissions and hard
// links. Device nodes, FIFOs and sockets are skipped: the guest creates its own
// /dev and an unprivileged builder cannot create device nodes anyway.
func copyTree(ctx context.Context, src, dst string) error {
	var dirs []dirMode
	hardlinks := make(map[hardLinkKey]string)
	buf := make([]byte, 1<<20)

	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return fmt.Errorf("image: relative path of %q: %w", path, err)
		}
		if rel == "." {
			return nil
		}
		dstPath := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("image: stat %q: %w", path, err)
		}
		mode := info.Mode()
		switch {
		case mode.IsDir():
			// Keep the copy writable while populating it; the exact mode is
			// applied after the walk.
			if err := os.MkdirAll(dstPath, mode.Perm()|0o700); err != nil {
				return fmt.Errorf("image: mkdir %q: %w", dstPath, err)
			}
			dirs = append(dirs, dirMode{path: dstPath, mode: modeBits(mode)})
			return nil
		case mode&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("image: readlink %q: %w", path, err)
			}
			if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
				return fmt.Errorf("image: mkdir %q: %w", filepath.Dir(dstPath), err)
			}
			if err := os.Symlink(target, dstPath); err != nil {
				return fmt.Errorf("image: symlink %q: %w", dstPath, err)
			}
			return nil
		case mode.IsRegular():
			if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
				return fmt.Errorf("image: mkdir %q: %w", filepath.Dir(dstPath), err)
			}
			if key, ok := hardLinkKeyOf(info); ok {
				if first, seen := hardlinks[key]; seen {
					if err := os.Link(first, dstPath); err == nil {
						return nil
					}
				} else {
					hardlinks[key] = dstPath
				}
			}
			return copyFileContents(path, dstPath, modeBits(mode), buf)
		default:
			// Device nodes, FIFOs and sockets: skipped intentionally.
			return nil
		}
	})
	if err != nil {
		return err
	}
	// Apply directory modes deepest-first so parent permissions cannot block
	// the final chmod.
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i].path) > len(dirs[j].path) })
	for _, d := range dirs {
		if err := os.Chmod(d.path, d.mode); err != nil {
			return fmt.Errorf("image: chmod %q: %w", d.path, err)
		}
	}
	return nil
}

type dirMode struct {
	path string
	mode fs.FileMode
}

type hardLinkKey struct {
	dev uint64
	ino uint64
}

func hardLinkKeyOf(info fs.FileInfo) (hardLinkKey, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Nlink < 2 {
		return hardLinkKey{}, false
	}
	return hardLinkKey{dev: uint64(st.Dev), ino: st.Ino}, true
}

func copyFileContents(src, dst string, perm fs.FileMode, buf []byte) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("image: open %q: %w", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm|0o600)
	if err != nil {
		return fmt.Errorf("image: create %q: %w", dst, err)
	}
	if _, err := io.CopyBuffer(out, in, buf); err != nil {
		_ = out.Close()
		return fmt.Errorf("image: copy %q: %w", src, err)
	}
	if err := out.Chmod(perm); err != nil {
		_ = out.Close()
		return fmt.Errorf("image: chmod %q: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("image: close %q: %w", dst, err)
	}
	return nil
}

// ensureRuntimeDirs makes sure the mount points the guest init needs exist in
// the rootfs. Ingested rootfs trees do not always contain them.
func ensureRuntimeDirs(rootfs string) error {
	dirs := []struct {
		rel  string
		mode fs.FileMode
	}{
		{"dev", 0o755},
		{"proc", 0o755},
		{"sys", 0o755},
		{"run", 0o755},
		{"tmp", fs.ModeSticky | 0o777},
	}
	for _, d := range dirs {
		path := filepath.Join(rootfs, d.rel)
		if err := os.MkdirAll(path, d.mode); err != nil {
			return fmt.Errorf("image: create %q: %w", path, err)
		}
		if err := os.Chmod(path, d.mode); err != nil {
			return fmt.Errorf("image: chmod %q: %w", path, err)
		}
	}
	return nil
}

// injectFile copies src into the rootfs at dst with the given mode.
func injectFile(src, dst string, perm fs.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("image: read injection source %q: %w", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("image: create %q: %w", filepath.Dir(dst), err)
	}
	return overwriteFile(dst, data, perm)
}

// overwriteFile replaces path with a regular file holding data. Any existing
// file (including a symlink from a foreign rootfs) is removed first so writes
// can never escape the rootfs through a symlink.
func overwriteFile(path string, data []byte, perm fs.FileMode) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("image: replace %q: %w", path, err)
	}
	if err := os.WriteFile(path, data, perm); err != nil {
		return fmt.Errorf("image: write %q: %w", path, err)
	}
	if err := os.Chmod(path, perm); err != nil {
		return fmt.Errorf("image: chmod %q: %w", path, err)
	}
	return nil
}

// manifestDigest computes the content identity of a prepared rootfs plus the
// disk geometry. The walk order is deterministic (lexical), and only stable
// attributes are hashed: paths, entry types, permission bits, symlink targets
// and regular-file bytes. Timestamps and ownership are excluded because
// mke2fs -d derives its own inode metadata.
func manifestDigest(rootfs string, sizeGB int) (string, error) {
	h := sha256.New()
	writeFieldString(h, "fastvm-base-disk")
	writeFieldUint64(h, formatVersion)
	writeFieldUint64(h, uint64(sizeGB))

	buf := make([]byte, 1<<20)
	err := filepath.WalkDir(rootfs, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == rootfs {
			return nil
		}
		rel, err := filepath.Rel(rootfs, path)
		if err != nil {
			return fmt.Errorf("image: relative path of %q: %w", path, err)
		}
		rel = filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("image: stat %q: %w", path, err)
		}
		mode := info.Mode()
		switch {
		case mode.IsDir():
			writeFieldString(h, "dir")
			writeFieldString(h, rel)
			writeFieldUint64(h, uint64(modeBits(mode)))
		case mode&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("image: readlink %q: %w", path, err)
			}
			writeFieldString(h, "symlink")
			writeFieldString(h, rel)
			writeFieldString(h, target)
		case mode.IsRegular():
			writeFieldString(h, "file")
			writeFieldString(h, rel)
			writeFieldUint64(h, uint64(modeBits(mode)))
			writeFieldUint64(h, uint64(info.Size()))
			f, err := os.Open(path)
			if err != nil {
				return fmt.Errorf("image: open %q: %w", path, err)
			}
			if _, err := io.CopyBuffer(h, f, buf); err != nil {
				_ = f.Close()
				return fmt.Errorf("image: hash %q: %w", path, err)
			}
			if err := f.Close(); err != nil {
				return fmt.Errorf("image: close %q: %w", path, err)
			}
		default:
			writeFieldString(h, "special")
			writeFieldString(h, rel)
			writeFieldUint64(h, uint64(mode.Type()))
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeFieldString(h hash.Hash, s string) { writeFieldBytes(h, []byte(s)) }

func writeFieldBytes(h hash.Hash, b []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(b)))
	_, _ = h.Write(length[:])
	_, _ = h.Write(b)
}

func writeFieldUint64(h hash.Hash, v uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	_, _ = h.Write(b[:])
}
