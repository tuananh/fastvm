package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"fastvm/internal/manager"
	"fastvm/internal/types"
)

const copyChunkSize = 1 << 20

func guestFileError(err error) error {
	if errors.Is(err, os.ErrNotExist) ||
		strings.Contains(strings.ToLower(err.Error()), "no such file or directory") {
		return fmt.Errorf("%w: %v", manager.ErrNotFound, err)
	}
	return err
}

// GuestPath resolves a guest path relative to the sandbox workdir.
func guestPath(sb *types.Sandbox, p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("guest path is required")
	}
	if !filepath.IsAbs(p) {
		base := sb.Workdir
		if base == "" {
			base = "/"
		}
		p = filepath.Join(base, p)
	}
	return filepath.Clean(p), nil
}

func (d *Daemon) CopyToGuest(ctx context.Context, ref, path string, mode os.FileMode, r io.Reader) error {
	rt, err := d.runtimeFor(ctx, ref)
	if err != nil {
		return err
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	sb := &rt.sb
	if sb.State.Sleeping() {
		if err := d.wakeLocked(ctx, rt, sb); err != nil {
			return err
		}
	}
	if sb.State != types.StateRunning && sb.State != types.StateIdle {
		return fmt.Errorf("%w: sandbox is %s", manager.ErrConflict, sb.State)
	}
	ac, err := d.ensureAgentLocked(ctx, rt, sb)
	if err != nil {
		return err
	}
	guest, err := guestPath(sb, path)
	if err != nil {
		return err
	}
	if mode == 0 {
		mode = 0o644
	}
	buf := make([]byte, copyChunkSize)
	var offset int64
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			if err := ac.FileWrite(ctx, guest, offset, uint32(mode.Perm()), buf[:n]); err != nil {
				return fmt.Errorf("copy to guest: %w", err)
			}
			offset += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("read local file: %w", rerr)
		}
	}
	if offset == 0 {
		if err := ac.FileWrite(ctx, guest, 0, uint32(mode.Perm()), nil); err != nil {
			return fmt.Errorf("copy empty file: %w", err)
		}
	}
	d.touchLocked(rt, sb)
	return nil
}

func (d *Daemon) CopyFromGuest(ctx context.Context, ref, path string, w io.Writer) (os.FileMode, int64, error) {
	return d.CopyFromGuestFrom(ctx, ref, path, 0, w)
}

// CopyFromGuestFrom copies a guest file starting at offset. It returns the file
// mode and full file size.
func (d *Daemon) CopyFromGuestFrom(ctx context.Context, ref, path string, offset int64, w io.Writer) (os.FileMode, int64, error) {
	rt, err := d.runtimeFor(ctx, ref)
	if err != nil {
		return 0, 0, err
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	sb := &rt.sb
	if sb.State.Sleeping() {
		if err := d.wakeLocked(ctx, rt, sb); err != nil {
			return 0, 0, err
		}
	}
	if sb.State != types.StateRunning && sb.State != types.StateIdle {
		return 0, 0, fmt.Errorf("%w: sandbox is %s", manager.ErrConflict, sb.State)
	}
	ac, err := d.ensureAgentLocked(ctx, rt, sb)
	if err != nil {
		return 0, 0, err
	}
	guest, err := guestPath(sb, path)
	if err != nil {
		return 0, 0, err
	}
	size, mode, err := ac.FileStat(ctx, guest)
	if err != nil {
		return 0, 0, fmt.Errorf("copy from guest: %w", guestFileError(err))
	}
	if offset < 0 {
		return 0, 0, fmt.Errorf("offset must not be negative")
	}
	if offset > size {
		offset = size
	}
	for offset < size {
		data, curSize, _, err := ac.FileRead(ctx, guest, offset, copyChunkSize)
		if err != nil {
			return 0, 0, fmt.Errorf("copy from guest: %w", err)
		}
		if len(data) == 0 {
			break
		}
		if _, err := w.Write(data); err != nil {
			return 0, 0, err
		}
		offset += int64(len(data))
		if curSize != size {
			size = curSize
			if offset > size {
				break
			}
		}
	}
	d.touchLocked(rt, sb)
	return os.FileMode(mode), size, nil
}

// StatGuest returns a guest file's mode and size.
func (d *Daemon) StatGuest(ctx context.Context, ref, path string) (os.FileMode, int64, error) {
	rt, err := d.runtimeFor(ctx, ref)
	if err != nil {
		return 0, 0, err
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	sb := &rt.sb
	if sb.State.Sleeping() {
		if err := d.wakeLocked(ctx, rt, sb); err != nil {
			return 0, 0, err
		}
	}
	if sb.State != types.StateRunning && sb.State != types.StateIdle {
		return 0, 0, fmt.Errorf("%w: sandbox is %s", manager.ErrConflict, sb.State)
	}
	ac, err := d.ensureAgentLocked(ctx, rt, sb)
	if err != nil {
		return 0, 0, err
	}
	guest, err := guestPath(sb, path)
	if err != nil {
		return 0, 0, err
	}
	size, mode, err := ac.FileStat(ctx, guest)
	if err != nil {
		return 0, 0, fmt.Errorf("stat guest file: %w", guestFileError(err))
	}
	return os.FileMode(mode), size, nil
}
