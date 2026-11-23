package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"fastvm/internal/manager"
	"fastvm/internal/types"
	"fastvm/internal/volume"
)

// validateDataVolumes checks the API-facing data-volume contract. Mount paths
// end up in the kernel command line, so they must be simple absolute paths.
func validateDataVolumes(vols []types.DataVolume) error {
	if len(vols) > 25 {
		return fmt.Errorf("%w: at most 25 data volumes are supported", manager.ErrInvalid)
	}
	seenMount := make(map[string]bool, len(vols))
	for _, vol := range vols {
		if vol.SizeGB < 1 || vol.SizeGB > 1024 {
			return fmt.Errorf("%w: data volume size must be between 1 and 1024 GiB", manager.ErrInvalid)
		}
		mnt := vol.Mount
		if !filepath.IsAbs(mnt) || filepath.Clean(mnt) != mnt || mnt == "/" {
			return fmt.Errorf("%w: data volume mount %q must be an absolute clean path below /", manager.ErrInvalid, mnt)
		}
		if strings.ContainsAny(mnt, ":, \t\n\\") {
			return fmt.Errorf("%w: data volume mount %q contains unsupported characters", manager.ErrInvalid, mnt)
		}
		if seenMount[mnt] {
			return fmt.Errorf("%w: duplicate data volume mount %q", manager.ErrInvalid, mnt)
		}
		seenMount[mnt] = true
	}
	return nil
}

// createDataVolumes materializes one reflink clone per data volume from a
// content-sized blank ext4 template.
func (d *Daemon) createDataVolumes(ctx context.Context, id string, vols []types.DataVolume) ([]types.DataVolume, error) {
	out := append([]types.DataVolume(nil), vols...)
	dir := filepath.Join(d.cfg.DataDir, "volumes")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	for i := range out {
		template, err := d.dataTemplate(ctx, out[i].SizeGB)
		if err != nil {
			removeDataVolumeFiles(out[:i])
			return nil, err
		}
		path := filepath.Join(dir, fmt.Sprintf("%s-data-%d.img", id, i))
		if err := volume.CopyFile(ctx, template, path); err != nil {
			_ = os.Remove(path)
			removeDataVolumeFiles(out[:i])
			return nil, fmt.Errorf("data volume %d: clone: %w", i, err)
		}
		out[i].Path = path
	}
	return out, nil
}

// dataTemplate ensures a blank, formatted ext4 template of the requested size.
func (d *Daemon) dataTemplate(ctx context.Context, sizeGB int) (string, error) {
	d.dataMu.Lock()
	defer d.dataMu.Unlock()

	dir := filepath.Join(d.cfg.DataDir, "volumes", "templates")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("data-%dGiB.ext4", sizeGB))
	if info, err := os.Stat(path); err == nil && info.Size() == int64(sizeGB)<<30 {
		return path, nil
	}

	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	if err := f.Truncate(int64(sizeGB) << 30); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if _, err := exec.LookPath("mke2fs"); err != nil {
		os.Remove(tmp)
		return "", errors.New("mke2fs not found (install e2fsprogs)")
	}
	out, err := exec.CommandContext(ctx, "mke2fs", "-q", "-F", "-t", "ext4", tmp).CombinedOutput()
	if err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("mke2fs data template: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return path, nil
}

func removeDataVolumeFiles(vols []types.DataVolume) {
	for _, vol := range vols {
		if vol.Path != "" {
			_ = os.Remove(vol.Path)
		}
	}
}

func (d *Daemon) removeDataVolumes(vols []types.DataVolume) {
	removeDataVolumeFiles(vols)
}
