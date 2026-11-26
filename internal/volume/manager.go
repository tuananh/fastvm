package volume

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"fastvm/internal/types"
)

// Supported CoW clone modes (docs/DESIGN.md §5.4 storage.cow).
const (
	// COWReflink clones with the FICLONE ioctl (XFS/Btrfs and other reflink
	// filesystems); falls back to a copy on filesystems that do not support it.
	COWReflink = "reflink"
	// COWCopy always performs a sparse copy.
	COWCopy = "copy"
	// COWDMThin names the dm-thin snapshot backend. It is not implemented by
	// this manager (it requires a device-mapper thin pool and root); Create
	// returns ErrDMThinUnsupported with guidance.
	COWDMThin = "dm-thin"
)

const (
	volumeExt = ".img"
	metaExt   = ".json"
	// maxIDLen keeps "<sandboxID>.img" comfortably below NAME_MAX.
	maxIDLen = 200
)

// Sentinel errors for errors.Is.
var (
	// ErrDMThinUnsupported means the dm-thin clone backend is not available.
	ErrDMThinUnsupported = errors.New("volume: dm-thin snapshots are not supported")
	// ErrSizeMismatch means a requested/recorded size does not match the base
	// disk or the volume file.
	ErrSizeMismatch = errors.New("volume: size does not match base disk")
	// ErrVolumeExists means a volume for the sandbox already exists for a
	// different base disk.
	ErrVolumeExists = errors.New("volume: a volume for this sandbox already exists")
)

// Options configures an FSManager.
type Options struct {
	// VolDir stores the per-sandbox volumes (<sandboxID>.img and .json).
	VolDir string
	// DefaultCOW is the clone mode used when Create receives an empty clone
	// argument: reflink, copy or dm-thin (default reflink).
	DefaultCOW string
	// ResizeFS is the resize2fs executable used when growing or shrinking a
	// sandbox root volume. Empty uses resize2fs from PATH.
	ResizeFS string
	// E2fsck is the e2fsck executable used before a shrink. Empty uses e2fsck
	// from PATH.
	E2fsck string
}

// FSManager is the concrete volume.Manager implementation. Volumes are raw
// ext4 files cloned from a base disk; the volume metadata sidecar records the
// base digest, size and the clone method that was actually used.
type FSManager struct {
	dir          string
	defaultCOW   string
	resize2fsBin string
	e2fsckBin    string
	templateMu   sync.Mutex
}

var _ Manager = (*FSManager)(nil)

// NewManager creates a volume manager rooted at opts.VolDir.
func NewManager(opts Options) (*FSManager, error) {
	if strings.TrimSpace(opts.VolDir) == "" {
		return nil, errors.New("volume: Options.VolDir is required")
	}
	dir, err := filepath.Abs(opts.VolDir)
	if err != nil {
		return nil, fmt.Errorf("volume: resolve volume dir: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("volume: create volume dir %q: %w", dir, err)
	}
	defaultCOW := opts.DefaultCOW
	if defaultCOW == "" {
		defaultCOW = COWReflink
	}
	switch defaultCOW {
	case COWReflink, COWCopy, COWDMThin:
	default:
		return nil, fmt.Errorf("volume: invalid default COW mode %q (want reflink|copy|dm-thin)", defaultCOW)
	}
	resizeBin := opts.ResizeFS
	if resizeBin == "" {
		resizeBin = "resize2fs"
	}
	e2fsckBin := opts.E2fsck
	if e2fsckBin == "" {
		e2fsckBin = "e2fsck"
	}
	return &FSManager{dir: dir, defaultCOW: defaultCOW, resize2fsBin: resizeBin, e2fsckBin: e2fsckBin}, nil
}

// templatePath is the cached resized clone for one base digest and root size.
func (m *FSManager) templatePath(base types.BaseDisk, sizeGB int) string {
	digest := strings.TrimPrefix(base.Digest, "sha256:")
	if digest == "" {
		return ""
	}
	return filepath.Join(m.dir, fmt.Sprintf(".template-%s-%dGiB%s", digest, sizeGB, volumeExt))
}

// ensureTemplate builds (or reuses) a resized clone so repeated sandboxes with
// the same base and root size only pay for a clone, not e2fsck and resize2fs.
func (m *FSManager) ensureTemplate(ctx context.Context, base types.BaseDisk, sizeGB int, baseSize, expected int64, mode string) (string, error) {
	path := m.templatePath(base, sizeGB)
	if path == "" {
		return "", nil
	}
	m.templateMu.Lock()
	defer m.templateMu.Unlock()
	if info, err := os.Stat(path); err == nil && info.Size() == expected {
		return path, nil
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("volume: stat template: %w", err)
	}
	tmp, err := os.CreateTemp(m.dir, ".tmp-template-*"+volumeExt)
	if err != nil {
		return "", fmt.Errorf("volume: create template: %w", err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()
	cloneSize := expected
	if expected < baseSize {
		cloneSize = baseSize
	}
	if _, err := cloneInto(ctx, mode, base.Path, tmp, cloneSize); err != nil {
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		return "", fmt.Errorf("volume: fsync template: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("volume: close template: %w", err)
	}
	if err := m.resizeClone(ctx, tmpPath, sizeGB, expected, baseSize); err != nil {
		return "", err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return "", fmt.Errorf("volume: publish template: %w", err)
	}
	committed = true
	return path, nil
}

// resizeClone grows or shrinks an offline ext4 clone.
func (m *FSManager) resizeClone(ctx context.Context, path string, sizeGB int, expected, baseSize int64) error {
	if expected == baseSize {
		return nil
	}
	if expected < baseSize {
		cmd := exec.CommandContext(ctx, m.e2fsckBin, "-f", "-y", path)
		output, err := cmd.CombinedOutput()
		if err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() > 1 {
				return fmt.Errorf("volume: e2fsck before shrink: %w: %s", err, strings.TrimSpace(string(output)))
			}
		}
		cmd = exec.CommandContext(ctx, m.resize2fsBin, path, fmt.Sprintf("%dG", sizeGB))
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("volume: shrink %s to %d GiB: %w: %s", path, sizeGB, err, strings.TrimSpace(string(output)))
		}
		if err := os.Truncate(path, expected); err != nil {
			return fmt.Errorf("volume: truncate shrunk volume: %w", err)
		}
		return nil
	}
	cmd := exec.CommandContext(ctx, m.resize2fsBin, path)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("volume: resize %s to %d bytes: %w: %s", path, expected, err, strings.TrimSpace(string(output)))
	}
	return nil
}

// Create clones base into <VolDir>/<sandboxID>.img.
//
// With clone == "" the configured DefaultCOW is used. reflink attempts the
// FICLONE ioctl and transparently falls back to a sparse copy on filesystems
// without reflink support (e.g. ext4); the returned Volume.COW reports the
// method that was actually used. sizeGB may be smaller or larger than the base
// image; the clone is shrunk with e2fsck+resize2fs or grown with resize2fs
// offline before it is published.
// Create is idempotent: an existing compatible volume is returned unchanged.
func (m *FSManager) Create(ctx context.Context, sandboxID string, base types.BaseDisk, sizeGB int, clone string) (Volume, error) {
	if err := ctx.Err(); err != nil {
		return Volume{}, err
	}
	if err := validateSandboxID(sandboxID); err != nil {
		return Volume{}, err
	}
	if strings.TrimSpace(base.Path) == "" {
		return Volume{}, errors.New("volume: base disk path is required")
	}
	baseInfo, err := os.Stat(base.Path)
	if err != nil {
		return Volume{}, fmt.Errorf("volume: stat base disk: %w", err)
	}
	if !baseInfo.Mode().IsRegular() {
		return Volume{}, fmt.Errorf("volume: base disk %q is not a regular file", base.Path)
	}
	baseSize := baseInfo.Size()
	if baseSize <= 0 {
		return Volume{}, fmt.Errorf("volume: base disk %q is empty", base.Path)
	}
	if base.SizeBytes != 0 && base.SizeBytes != baseSize {
		return Volume{}, fmt.Errorf("%w: base disk %s metadata size is %d bytes but the file is %d bytes",
			ErrSizeMismatch, base.Identity(), base.SizeBytes, baseSize)
	}

	expected := baseSize
	if sizeGB != 0 {
		if sizeGB < 1 {
			return Volume{}, fmt.Errorf("%w: size_gb must be at least 1 (got %d)", ErrSizeMismatch, sizeGB)
		}
		if int64(sizeGB) > math.MaxInt64>>30 {
			return Volume{}, fmt.Errorf("%w: size_gb %d is too large", ErrSizeMismatch, sizeGB)
		}
		expected = int64(sizeGB) << 30
	}

	mode := clone
	if mode == "" {
		mode = m.defaultCOW
	}
	switch mode {
	case COWReflink, COWCopy:
	case COWDMThin:
		return Volume{}, fmt.Errorf("%w (fastvm does not create device-mapper snapshots); set storage.cow to %q on XFS/Btrfs or %q",
			ErrDMThinUnsupported, COWReflink, COWCopy)
	default:
		return Volume{}, fmt.Errorf("volume: invalid clone mode %q (want reflink|copy|dm-thin)", mode)
	}

	target := m.pathFor(sandboxID)
	metaPath := m.metaPathFor(sandboxID)

	if existing, ok, err := m.Get(ctx, sandboxID); err != nil {
		return Volume{}, err
	} else if ok {
		if existing.BaseDisk != "" && base.Digest != "" && existing.BaseDisk != base.Digest {
			return Volume{}, fmt.Errorf("%w: volume %q is based on %s, requested %s; remove it first",
				ErrVolumeExists, sandboxID, existing.BaseDisk, base.Digest)
		}
		if existing.SizeBytes != expected {
			return Volume{}, fmt.Errorf("%w: existing volume %q is %d bytes, want %d",
				ErrSizeMismatch, sandboxID, existing.SizeBytes, expected)
		}
		return existing, nil
	}

	// A leftover image without usable metadata is stale; replace it.
	if err := os.Remove(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Volume{}, fmt.Errorf("volume: remove stale volume %q: %w", target, err)
	}
	tmpFile, err := os.CreateTemp(m.dir, ".tmp-"+sandboxID+"-*"+volumeExt)
	if err != nil {
		return Volume{}, fmt.Errorf("volume: create temp volume: %w", err)
	}
	tmpPath := tmpFile.Name()
	committed := false
	defer func() {
		if !committed {
			_ = tmpFile.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	method := ""
	if expected != baseSize && base.Digest != "" {
		template, err := m.ensureTemplate(ctx, base, sizeGB, baseSize, expected, mode)
		if err != nil {
			return Volume{}, err
		}
		method, err = cloneInto(ctx, mode, template, tmpFile, expected)
		if err != nil {
			return Volume{}, err
		}
	} else {
		cloneSize := expected
		if expected < baseSize {
			cloneSize = baseSize
		}
		var err error
		method, err = cloneInto(ctx, mode, base.Path, tmpFile, cloneSize)
		if err != nil {
			return Volume{}, err
		}
		if err := m.resizeClone(ctx, tmpPath, sizeGB, expected, baseSize); err != nil {
			return Volume{}, err
		}
	}
	if err := tmpFile.Sync(); err != nil {
		return Volume{}, fmt.Errorf("volume: fsync volume: %w", err)
	}
	if err := tmpFile.Chmod(0o600); err != nil {
		return Volume{}, fmt.Errorf("volume: chmod volume: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return Volume{}, fmt.Errorf("volume: close volume: %w", err)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return Volume{}, fmt.Errorf("volume: publish volume: %w", err)
	}
	committed = true

	vol := Volume{
		SandboxID: sandboxID,
		Path:      target,
		BaseDisk:  base.Digest,
		SizeBytes: expected,
		COW:       method,
	}
	if err := writeVolumeMeta(metaPath, vol); err != nil {
		_ = os.Remove(target)
		return Volume{}, err
	}
	return vol, nil
}

// Remove deletes the sandbox volume and its metadata. It is idempotent: a
// missing volume is not an error.
func (m *FSManager) Remove(ctx context.Context, sandboxID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateSandboxID(sandboxID); err != nil {
		return err
	}
	var errs []error
	for _, path := range []string{m.pathFor(sandboxID), m.metaPathFor(sandboxID)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("volume: remove %q: %w", path, err))
		}
	}
	// Clean up temp files left behind by an interrupted Create.
	if leftovers, err := filepath.Glob(filepath.Join(m.dir, ".tmp-"+sandboxID+"-*"+volumeExt)); err == nil {
		for _, path := range leftovers {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, fmt.Errorf("volume: remove temp %q: %w", path, err))
			}
		}
	}
	return errors.Join(errs...)
}

// Get returns the sandbox volume if it exists and is consistent.
func (m *FSManager) Get(ctx context.Context, sandboxID string) (Volume, bool, error) {
	if err := ctx.Err(); err != nil {
		return Volume{}, false, err
	}
	if err := validateSandboxID(sandboxID); err != nil {
		return Volume{}, false, err
	}
	metaPath := m.metaPathFor(sandboxID)
	data, err := os.ReadFile(metaPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Volume{}, false, nil
		}
		return Volume{}, false, fmt.Errorf("volume: read metadata %q: %w", metaPath, err)
	}
	var vol Volume
	if err := json.Unmarshal(data, &vol); err != nil {
		return Volume{}, false, fmt.Errorf("volume: corrupt metadata %q: %w", metaPath, err)
	}
	target := m.pathFor(sandboxID)
	if vol.Path != target {
		return Volume{}, false, fmt.Errorf("volume: metadata %q points at %q, expected %q", metaPath, vol.Path, target)
	}
	info, err := os.Stat(target)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Volume{}, false, nil
		}
		return Volume{}, false, fmt.Errorf("volume: stat %q: %w", target, err)
	}
	if info.Size() != vol.SizeBytes {
		return Volume{}, false, fmt.Errorf("%w: volume %q is %d bytes, metadata says %d",
			ErrSizeMismatch, target, info.Size(), vol.SizeBytes)
	}
	return vol, true, nil
}

func (m *FSManager) pathFor(sandboxID string) string {
	return filepath.Join(m.dir, sandboxID+volumeExt)
}

func (m *FSManager) metaPathFor(sandboxID string) string {
	return filepath.Join(m.dir, sandboxID+metaExt)
}

func validateSandboxID(id string) error {
	if id == "" {
		return errors.New("volume: sandbox id is required")
	}
	if len(id) > maxIDLen {
		return fmt.Errorf("volume: sandbox id %q is too long (max %d bytes)", id, maxIDLen)
	}
	if id == "." || id == ".." || strings.HasPrefix(id, ".") {
		return fmt.Errorf("volume: invalid sandbox id %q", id)
	}
	if strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("volume: invalid sandbox id %q (contains a path separator)", id)
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("volume: invalid sandbox id %q (contains control characters)", id)
		}
	}
	return nil
}

func writeVolumeMeta(path string, vol Volume) error {
	data, err := json.MarshalIndent(vol, "", "  ")
	if err != nil {
		return fmt.Errorf("volume: marshal metadata: %w", err)
	}
	data = append(data, '\n')
	return writeFileAtomic(path, data, 0o600)
}

func writeFileAtomic(path string, data []byte, perm fs.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".fastvm-meta-*")
	if err != nil {
		return fmt.Errorf("volume: create metadata file: %w", err)
	}
	tmp := f.Name()
	defer func() {
		if tmp != "" {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("volume: write metadata: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("volume: fsync metadata: %w", err)
	}
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return fmt.Errorf("volume: chmod metadata: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("volume: close metadata: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("volume: publish metadata: %w", err)
	}
	tmp = ""
	return nil
}
