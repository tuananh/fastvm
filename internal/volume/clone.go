package volume

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// copyBufferSize is the buffer used for the copy fallback (and per-extent
// reads). 1MiB keeps syscall overhead low without inflating memory.
const copyBufferSize = 1 << 20

// tryReflink clones src into dst using the FICLONE ioctl (0x40049409 on
// Linux). Both files must be opened; dst is truncated/replaced by the source's
// contents. Supported by XFS and Btrfs (and some others) when reflink is
// enabled.
func tryReflink(dst, src *os.File) error {
	return unix.IoctlFileClone(int(dst.Fd()), int(src.Fd()))
}

// reflinkUnsupported reports whether err means "this filesystem cannot clone",
// in which case the caller falls back to a copy.
func reflinkUnsupported(err error) bool {
	return errors.Is(err, unix.EOPNOTSUPP) ||
		errors.Is(err, unix.ENOTTY) ||
		errors.Is(err, unix.EXDEV) ||
		errors.Is(err, unix.EINVAL) ||
		errors.Is(err, unix.ENOSYS)
}

// cloneInto fills dst (an empty temporary file) with the contents of srcPath,
// using mode. It returns the clone method that actually succeeded.
func cloneInto(ctx context.Context, mode, srcPath string, dst *os.File, size int64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	src, err := os.Open(srcPath)
	if err != nil {
		return "", fmt.Errorf("volume: open base disk %q: %w", srcPath, err)
	}
	defer src.Close()

	switch mode {
	case COWReflink:
		if err := tryReflink(dst, src); err == nil {
			if err := dst.Truncate(size); err != nil {
				return "", fmt.Errorf("volume: size reflink clone: %w", err)
			}
			return COWReflink, nil
		} else if !reflinkUnsupported(err) {
			return "", fmt.Errorf("volume: reflink clone of %q: %w", srcPath, err)
		}
		// Filesystem without reflink support (for example ext4): copy.
		if err := dst.Truncate(0); err != nil {
			return "", fmt.Errorf("volume: reset clone: %w", err)
		}
		fallthrough
	case COWCopy:
		if err := copySparse(ctx, src, dst, size); err != nil {
			return "", err
		}
		return COWCopy, nil
	default:
		return "", fmt.Errorf("volume: unsupported clone mode %q", mode)
	}
}

// copySparse copies src into dst while preserving holes: only data extents are
// read/written, so cloning a mostly-empty 8GiB base disk does not allocate
// 8GiB per sandbox. It falls back to a plain copy on filesystems that do not
// implement SEEK_DATA/SEEK_HOLE.
func copySparse(ctx context.Context, src, dst *os.File, size int64) error {
	if err := dst.Truncate(size); err != nil {
		return fmt.Errorf("volume: size volume: %w", err)
	}
	buf := make([]byte, copyBufferSize)
	srcFd := int(src.Fd())
	var offset int64
	for offset < size {
		if err := ctx.Err(); err != nil {
			return err
		}
		dataOff, err := unix.Seek(srcFd, offset, unix.SEEK_DATA)
		if err != nil {
			if errors.Is(err, unix.ENXIO) {
				break // only holes remain
			}
			if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) {
				return copyAll(ctx, src, dst, size, buf)
			}
			return fmt.Errorf("volume: seek data in base disk: %w", err)
		}
		holeOff, err := unix.Seek(srcFd, dataOff, unix.SEEK_HOLE)
		if err != nil {
			if errors.Is(err, unix.ENXIO) {
				holeOff = size
			} else {
				return fmt.Errorf("volume: seek hole in base disk: %w", err)
			}
		}
		if holeOff > size {
			holeOff = size
		}
		if holeOff <= dataOff {
			break
		}
		if err := copyRange(ctx, src, dst, dataOff, holeOff-dataOff, buf); err != nil {
			return err
		}
		offset = holeOff
	}
	return nil
}

// copyRange copies length bytes at off from src to dst.
func copyRange(ctx context.Context, src, dst *os.File, off, length int64, buf []byte) error {
	if _, err := src.Seek(off, io.SeekStart); err != nil {
		return fmt.Errorf("volume: seek base disk: %w", err)
	}
	if _, err := dst.Seek(off, io.SeekStart); err != nil {
		return fmt.Errorf("volume: seek volume: %w", err)
	}
	for length > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk := int64(len(buf))
		if chunk > length {
			chunk = length
		}
		n, err := io.ReadFull(src, buf[:chunk])
		if err != nil {
			return fmt.Errorf("volume: read base disk: %w", err)
		}
		if _, err := dst.Write(buf[:n]); err != nil {
			return fmt.Errorf("volume: write volume: %w", err)
		}
		length -= int64(n)
	}
	return nil
}

// copyAll is the non-sparse fallback used when the filesystem has no hole
// information.
func copyAll(ctx context.Context, src, dst *os.File, size int64, buf []byte) error {
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("volume: seek base disk: %w", err)
	}
	if _, err := dst.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("volume: seek volume: %w", err)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return fmt.Errorf("volume: write volume: %w", werr)
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("volume: read base disk: %w", err)
		}
	}
	if err := dst.Truncate(size); err != nil {
		return fmt.Errorf("volume: size volume: %w", err)
	}
	return nil
}
