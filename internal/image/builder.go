package image

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"fastvm/internal/types"
)

// DefaultSizeGB is the base-disk size used when a spec leaves SizeGB zero
// (docs/DESIGN.md §5.4: volume.size = 8GiB).
const DefaultSizeGB = 8

// Guest paths where fastvm injects its runtime pieces (docs/DESIGN.md §6.5).
const (
	// AgentGuestPath is where the guest agent binary is injected.
	AgentGuestPath = "/usr/local/bin/fastvm-agent"
	// InitGuestPath is where the guest init script is injected.
	InitGuestPath = "/init"
)

const (
	// formatVersion is mixed into the content digest. Bump it whenever the
	// disk layout or injection changes so stale caches are not reused.
	formatVersion = 1

	diskExt = ".img"
	metaExt = ".json"
	tagsDir = "tags"
)

// Sentinel errors for errors.Is.
var (
	// ErrNotFound is returned by Get when no base disk matches the reference.
	ErrNotFound = errors.New("image not found")
	// ErrDockerUnavailable means the docker CLI or daemon is not usable.
	ErrDockerUnavailable = errors.New("docker unavailable")
)

// Options configures a Builder.
type Options struct {
	// BaseDir stores the content-addressed base disks (<hex>.img, <hex>.json).
	BaseDir string
	// AgentPath is the default guest agent binary for specs that leave
	// AgentPath empty. Empty disables agent injection.
	AgentPath string
	// InitPath is the default init script. Empty falls back to a built-in
	// minimal init that mounts /proc, /sys and /dev and execs the agent.
	InitPath string
	// DockerBin is the docker CLI to use (default "docker").
	DockerBin string
	// WorkDir is where temporary rootfs/disk files are created (default
	// BaseDir, which guarantees same-filesystem renames).
	WorkDir string
}

// FSBuilder is the concrete Builder implementation. It stores content-addressed
// ext4 base disks and records tag -> digest mappings.
type FSBuilder struct {
	baseDir   string
	workDir   string
	agentPath string
	initPath  string
	dockerBin string
	mke2fsBin string

	now    func() time.Time
	runCmd func(ctx context.Context, extraEnv []string, name string, args ...string) ([]byte, error)
}

var _ Builder = (*FSBuilder)(nil)

// NewBuilder creates a base-disk builder rooted at opts.BaseDir.
func NewBuilder(opts Options) (*FSBuilder, error) {
	if strings.TrimSpace(opts.BaseDir) == "" {
		return nil, errors.New("image: Options.BaseDir is required")
	}
	baseDir, err := filepath.Abs(opts.BaseDir)
	if err != nil {
		return nil, fmt.Errorf("image: resolve base dir: %w", err)
	}
	if err := os.MkdirAll(baseDir, 0o700); err != nil {
		return nil, fmt.Errorf("image: create base dir %q: %w", baseDir, err)
	}
	workDir := opts.WorkDir
	if strings.TrimSpace(workDir) == "" {
		workDir = baseDir
	} else if workDir, err = filepath.Abs(workDir); err != nil {
		return nil, fmt.Errorf("image: resolve work dir: %w", err)
	}
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return nil, fmt.Errorf("image: create work dir %q: %w", workDir, err)
	}
	dockerBin := opts.DockerBin
	if dockerBin == "" {
		dockerBin = "docker"
	}
	return &FSBuilder{
		baseDir:   baseDir,
		workDir:   workDir,
		agentPath: opts.AgentPath,
		initPath:  opts.InitPath,
		dockerBin: dockerBin,
		mke2fsBin: "mke2fs",
		now:       time.Now,
	}, nil
}

// Ingest converts an unpacked rootfs directory into a cached ext4 base disk.
func (b *FSBuilder) Ingest(ctx context.Context, spec IngestSpec) (types.BaseDisk, error) {
	return b.makeBaseDisk(ctx, baseDiskRequest{
		srcRootfs:  spec.RootfsDir,
		copySource: true, // never mutate the caller's rootfs
		tag:        spec.Tag,
		sizeGB:     spec.SizeGB,
		agentPath:  spec.AgentPath,
		initPath:   spec.InitPath,
		source:     "rootfs",
	})
}

// Build turns a Dockerfile into a cached ext4 base disk.
//
// The image is built with BuildKit (DOCKER_BUILDKIT=1 docker build --iidfile
// so no user tag is clobbered), exported with docker export and unpacked into a
// temporary rootfs, then run through the same inject + mke2fs pipeline as
// Ingest. Nothing is ever pushed to a registry.
func (b *FSBuilder) Build(ctx context.Context, spec BuildSpec) (types.BaseDisk, error) {
	if err := ctx.Err(); err != nil {
		return types.BaseDisk{}, err
	}
	if strings.TrimSpace(spec.Tag) == "" {
		return types.BaseDisk{}, errors.New("image: build tag is required")
	}
	if strings.TrimSpace(spec.ContextDir) == "" {
		return types.BaseDisk{}, errors.New("image: build context dir is required")
	}
	rootfsDir, cleanup, err := b.dockerRootfs(ctx, spec)
	if err != nil {
		return types.BaseDisk{}, err
	}
	defer cleanup()
	return b.makeBaseDisk(ctx, baseDiskRequest{
		srcRootfs:  rootfsDir,
		copySource: false, // dockerRootfs handed us a disposable temp rootfs
		tag:        spec.Tag,
		sizeGB:     spec.SizeGB,
		agentPath:  spec.AgentPath,
		initPath:   spec.InitPath,
		source:     "dockerfile",
		kernelPath: spec.KernelPath,
	})
}

// tagIndex returns digest -> current tags from the tag index. The tag index is
// authoritative; metadata files keep the tag that was used at build time and
// would otherwise make a moved tag look like it belongs to multiple disks.
func (b *FSBuilder) tagIndex() (map[string][]string, error) {
	dir := filepath.Join(b.baseDir, tagsDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("image: read tag index: %w", err)
	}
	index := make(map[string][]string)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		tag, err := url.PathUnescape(entry.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		digestHex, ok := digestHexFromRef(strings.TrimSpace(string(data)))
		if !ok {
			continue
		}
		index[digestHex] = append(index[digestHex], tag)
	}
	for digest := range index {
		sort.Strings(index[digest])
	}
	return index, nil
}

// List returns all cached base disks, newest first. Tag/Name are derived from
// the current tag index, so a disk whose tag moved to a newer build is listed
// as untagged instead of looking like a duplicate tag.
func (b *FSBuilder) List(ctx context.Context) ([]types.BaseDisk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(b.baseDir)
	if err != nil {
		return nil, fmt.Errorf("image: list base dir %q: %w", b.baseDir, err)
	}
	tags, err := b.tagIndex()
	if err != nil {
		return nil, err
	}
	disks := make([]types.BaseDisk, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), metaExt) {
			continue
		}
		digestHex := strings.TrimSuffix(entry.Name(), metaExt)
		if _, ok := digestHexFromRef(digestHex); !ok {
			continue
		}
		disk, ok := b.loadDisk(digestHex)
		if !ok {
			continue // incomplete or corrupt cache entry: ignore
		}
		disk.Tags = tags[digestHex]
		if len(disk.Tags) > 0 {
			// Prefer the tag recorded in metadata when it is still current,
			// otherwise the lexicographically first alias.
			preferred := disk.Tags[0]
			for _, tag := range disk.Tags {
				if tag == disk.Tag {
					preferred = tag
					break
				}
			}
			disk.Tag, disk.Name = preferred, preferred
		} else {
			disk.Tag, disk.Name = "", ""
		}
		disks = append(disks, disk)
	}
	sort.Slice(disks, func(i, j int) bool {
		if !disks[i].CreatedAt.Equal(disks[j].CreatedAt) {
			return disks[i].CreatedAt.After(disks[j].CreatedAt)
		}
		return disks[i].Digest < disks[j].Digest
	})
	return disks, nil
}

// Get resolves a tag (e.g. "nginx:alpine") or digest (with or without the
// "sha256:" prefix) to a cached base disk.
func (b *FSBuilder) Get(ctx context.Context, ref string) (types.BaseDisk, error) {
	if err := ctx.Err(); err != nil {
		return types.BaseDisk{}, err
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return types.BaseDisk{}, fmt.Errorf("%w: empty reference", ErrNotFound)
	}
	if digestHex, ok, err := b.resolveTag(ref); err != nil {
		return types.BaseDisk{}, err
	} else if ok {
		disk, found := b.loadDisk(digestHex)
		if !found {
			return types.BaseDisk{}, fmt.Errorf("%w: tag %q points at missing base disk %s", ErrNotFound, ref, digestHex)
		}
		// Report the tag that was actually asked for, even when it is an alias
		// of a disk that was first created under another tag.
		disk.Name = ref
		disk.Tag = ref
		return disk, nil
	}
	if digestHex, ok := digestHexFromRef(ref); ok {
		if disk, found := b.loadDisk(digestHex); found {
			return disk, nil
		}
	}
	return types.BaseDisk{}, fmt.Errorf("%w: %q", ErrNotFound, ref)
}

// baseDiskRequest is the shared input of Ingest and Build.
type baseDiskRequest struct {
	srcRootfs  string
	copySource bool
	tag        string
	sizeGB     int
	agentPath  string
	initPath   string
	source     string
	kernelPath string
}

// makeBaseDisk is the content-addressed pipeline shared by Ingest and Build:
// prepare rootfs -> inject agent/init -> digest -> cache lookup -> mke2fs ->
// publish disk + metadata + tag mapping.
func (b *FSBuilder) makeBaseDisk(ctx context.Context, req baseDiskRequest) (types.BaseDisk, error) {
	if err := ctx.Err(); err != nil {
		return types.BaseDisk{}, err
	}
	if strings.TrimSpace(req.srcRootfs) == "" {
		return types.BaseDisk{}, errors.New("image: rootfs directory is required")
	}
	rootfs, err := filepath.Abs(req.srcRootfs)
	if err != nil {
		return types.BaseDisk{}, fmt.Errorf("image: resolve rootfs: %w", err)
	}
	info, err := os.Stat(rootfs)
	if err != nil {
		return types.BaseDisk{}, fmt.Errorf("image: rootfs directory: %w", err)
	}
	if !info.IsDir() {
		return types.BaseDisk{}, fmt.Errorf("image: rootfs %q is not a directory", rootfs)
	}

	sizeGB := req.sizeGB
	if sizeGB == 0 {
		sizeGB = DefaultSizeGB
	}
	if sizeGB < 1 {
		return types.BaseDisk{}, fmt.Errorf("image: size_gb must be at least 1 (got %d)", sizeGB)
	}
	if int64(sizeGB) > math.MaxInt64>>30 {
		return types.BaseDisk{}, fmt.Errorf("image: size_gb %d is too large", sizeGB)
	}
	sizeBytes := int64(sizeGB) << 30

	if req.tag != "" {
		if err := validateTag(req.tag); err != nil {
			return types.BaseDisk{}, err
		}
	}
	agentPath := firstNonEmpty(req.agentPath, b.agentPath)
	initPath := firstNonEmpty(req.initPath, b.initPath)
	if agentPath != "" {
		if err := checkReadableFile(agentPath, "agent"); err != nil {
			return types.BaseDisk{}, err
		}
	}
	if initPath != "" {
		if err := checkReadableFile(initPath, "init script"); err != nil {
			return types.BaseDisk{}, err
		}
	}

	if req.copySource {
		tmpRoot, err := os.MkdirTemp(b.workDir, ".fastvm-rootfs-*")
		if err != nil {
			return types.BaseDisk{}, fmt.Errorf("image: create temp rootfs: %w", err)
		}
		defer os.RemoveAll(tmpRoot)
		if err := copyTree(ctx, rootfs, tmpRoot); err != nil {
			return types.BaseDisk{}, err
		}
		rootfs = tmpRoot
	}

	if err := ensureRuntimeDirs(rootfs); err != nil {
		return types.BaseDisk{}, err
	}
	if agentPath != "" {
		if err := injectFile(agentPath, filepath.Join(rootfs, agentGuestRel), 0o755); err != nil {
			return types.BaseDisk{}, err
		}
	}
	initData := []byte(builtinInit)
	if initPath != "" {
		initData, err = os.ReadFile(initPath)
		if err != nil {
			return types.BaseDisk{}, fmt.Errorf("image: read init script %q: %w", initPath, err)
		}
	}
	if err := overwriteFile(filepath.Join(rootfs, initGuestRel), initData, 0o755); err != nil {
		return types.BaseDisk{}, err
	}

	digestHex, err := manifestDigest(rootfs, sizeGB)
	if err != nil {
		return types.BaseDisk{}, err
	}
	digest := "sha256:" + digestHex
	imgPath := filepath.Join(b.baseDir, digestHex+diskExt)
	metaPath := filepath.Join(b.baseDir, digestHex+metaExt)

	// Content-addressed cache hit: the disk identity is derived from the inputs
	// before mke2fs runs, so an unchanged rebuild never pays for a rebuild.
	if disk, ok := b.loadDisk(digestHex); ok {
		if req.tag != "" {
			if err := b.recordTag(req.tag, digestHex); err != nil {
				return types.BaseDisk{}, err
			}
			disk.Name = req.tag
			disk.Tag = req.tag
		}
		return disk, nil
	}

	tmpDisk, err := os.CreateTemp(b.baseDir, ".fastvm-build-*"+diskExt)
	if err != nil {
		return types.BaseDisk{}, fmt.Errorf("image: create temp disk: %w", err)
	}
	tmpPath := tmpDisk.Name()
	committed := false
	defer func() {
		if !committed {
			_ = tmpDisk.Close()
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmpDisk.Truncate(sizeBytes); err != nil {
		return types.BaseDisk{}, fmt.Errorf("image: size disk: %w", err)
	}
	if err := tmpDisk.Close(); err != nil {
		return types.BaseDisk{}, fmt.Errorf("image: close temp disk: %w", err)
	}
	if err := b.mkfs(ctx, rootfs, tmpPath, digestHex); err != nil {
		return types.BaseDisk{}, err
	}
	diskFile, err := os.OpenFile(tmpPath, os.O_RDWR, 0)
	if err != nil {
		return types.BaseDisk{}, fmt.Errorf("image: reopen disk: %w", err)
	}
	if err := diskFile.Sync(); err != nil {
		diskFile.Close()
		return types.BaseDisk{}, fmt.Errorf("image: fsync disk: %w", err)
	}
	if err := diskFile.Chmod(0o600); err != nil {
		diskFile.Close()
		return types.BaseDisk{}, fmt.Errorf("image: chmod disk: %w", err)
	}
	if err := diskFile.Close(); err != nil {
		return types.BaseDisk{}, fmt.Errorf("image: close disk: %w", err)
	}
	if err := os.Rename(tmpPath, imgPath); err != nil {
		return types.BaseDisk{}, fmt.Errorf("image: publish disk: %w", err)
	}
	committed = true

	meta := diskMeta{
		Name:          req.tag,
		Tag:           req.tag,
		Digest:        digest,
		Path:          imgPath,
		SizeBytes:     sizeBytes,
		Source:        req.source,
		CreatedAt:     b.now().UTC(),
		FormatVersion: formatVersion,
		SizeGB:        sizeGB,
		KernelPath:    req.kernelPath,
	}
	if err := b.writeMeta(metaPath, meta); err != nil {
		_ = os.Remove(imgPath)
		return types.BaseDisk{}, fmt.Errorf("image: write metadata: %w", err)
	}
	if req.tag != "" {
		if err := b.recordTag(req.tag, digestHex); err != nil {
			return types.BaseDisk{}, err
		}
	}
	return meta.toBaseDisk(), nil
}

// Remove deletes base disk ref and every tag that points at its digest.
// Callers must ensure no sandbox still references the digest.
func (b *FSBuilder) Remove(ctx context.Context, ref string) (types.BaseDisk, error) {
	if err := ctx.Err(); err != nil {
		return types.BaseDisk{}, err
	}
	disk, err := b.Get(ctx, ref)
	if err != nil {
		return types.BaseDisk{}, err
	}
	digestHex, ok := digestHexFromRef(disk.Digest)
	if !ok {
		return types.BaseDisk{}, fmt.Errorf("image: invalid digest %q", disk.Digest)
	}
	tags, err := b.tagIndex()
	if err != nil {
		return types.BaseDisk{}, err
	}
	for _, tag := range tags[digestHex] {
		path := filepath.Join(b.baseDir, tagsDir, url.PathEscape(tag))
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return types.BaseDisk{}, fmt.Errorf("image: remove tag %q: %w", tag, err)
		}
	}
	imgPath := filepath.Join(b.baseDir, digestHex+diskExt)
	if err := os.Remove(imgPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return types.BaseDisk{}, fmt.Errorf("image: remove disk %q: %w", ref, err)
	}
	metaPath := filepath.Join(b.baseDir, digestHex+metaExt)
	if err := os.Remove(metaPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return types.BaseDisk{}, fmt.Errorf("image: remove metadata %q: %w", ref, err)
	}
	return disk, nil
}

// diskMeta is the on-disk (<hex>.json) representation of a base disk.
type diskMeta struct {
	Name          string    `json:"name"`
	Tag           string    `json:"tag,omitempty"`
	Digest        string    `json:"digest"`
	Path          string    `json:"path"`
	SizeBytes     int64     `json:"size_bytes"`
	Source        string    `json:"source"`
	CreatedAt     time.Time `json:"created_at"`
	FormatVersion int       `json:"format_version"`
	SizeGB        int       `json:"size_gb,omitempty"`
	KernelPath    string    `json:"kernel_path,omitempty"`
}

func (m diskMeta) toBaseDisk() types.BaseDisk {
	return types.BaseDisk{
		Name:      m.Name,
		Tag:       m.Tag,
		Digest:    m.Digest,
		Path:      m.Path,
		SizeBytes: m.SizeBytes,
		Source:    m.Source,
		CreatedAt: m.CreatedAt,
	}
}

// loadDisk returns a cached disk by its lowercase hex digest. It returns false
// for absent, incomplete, outdated or corrupt cache entries; such entries are
// simply rebuilt/ignored by the caller.
func (b *FSBuilder) loadDisk(digestHex string) (types.BaseDisk, bool) {
	imgPath := filepath.Join(b.baseDir, digestHex+diskExt)
	fi, err := os.Stat(imgPath)
	if err != nil || !fi.Mode().IsRegular() {
		return types.BaseDisk{}, false
	}
	data, err := os.ReadFile(filepath.Join(b.baseDir, digestHex+metaExt))
	if err != nil {
		return types.BaseDisk{}, false
	}
	var meta diskMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return types.BaseDisk{}, false
	}
	if meta.FormatVersion != formatVersion {
		return types.BaseDisk{}, false
	}
	if meta.Digest != "sha256:"+digestHex || meta.SizeBytes != fi.Size() {
		return types.BaseDisk{}, false
	}
	meta.Path = imgPath
	return meta.toBaseDisk(), true
}

func (b *FSBuilder) writeMeta(path string, meta diskMeta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	data = append(data, '\n')
	return writeFileAtomic(path, data, 0o600)
}

// recordTag atomically records tag -> digest in <baseDir>/tags/<escaped-tag>.
func (b *FSBuilder) recordTag(tag, digestHex string) error {
	if err := validateTag(tag); err != nil {
		return err
	}
	dir := filepath.Join(b.baseDir, tagsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("image: create tag index: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(dir, url.PathEscape(tag)), []byte(digestHex+"\n"), 0o600); err != nil {
		return fmt.Errorf("image: record tag %q: %w", tag, err)
	}
	return nil
}

// resolveTag looks a tag up in the tag index.
func (b *FSBuilder) resolveTag(tag string) (string, bool, error) {
	if err := validateTag(tag); err != nil {
		return "", false, nil // not tag-shaped; the caller may still pass a digest
	}
	data, err := os.ReadFile(filepath.Join(b.baseDir, tagsDir, url.PathEscape(tag)))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("image: read tag index: %w", err)
	}
	digestHex, ok := digestHexFromRef(strings.TrimSpace(string(data)))
	if !ok {
		return "", false, fmt.Errorf("image: tag %q has invalid digest %q", tag, strings.TrimSpace(string(data)))
	}
	return digestHex, true, nil
}

// validateTag checks a docker-style image reference. Tags may contain '/' and
// ':' (they are escaped in the tag index) but not control characters.
func validateTag(tag string) error {
	if tag == "" {
		return errors.New("image: tag is required")
	}
	if tag == "." || tag == ".." {
		return fmt.Errorf("image: invalid tag %q", tag)
	}
	if len(tag) > 255 {
		return fmt.Errorf("image: tag %q is too long (max 255 bytes)", tag)
	}
	for _, r := range tag {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("image: tag %q contains control characters", tag)
		}
	}
	return nil
}

// digestHexFromRef normalizes a digest reference to lowercase hex.
func digestHexFromRef(ref string) (string, bool) {
	ref = strings.TrimPrefix(strings.TrimSpace(ref), "sha256:")
	if len(ref) != sha256.Size*2 {
		return "", false
	}
	ref = strings.ToLower(ref)
	if _, err := hex.DecodeString(ref); err != nil {
		return "", false
	}
	return ref, true
}

// mkfs creates the ext4 filesystem on diskPath from rootfs.
func (b *FSBuilder) mkfs(ctx context.Context, rootfs, diskPath, digestHex string) error {
	bin := b.mke2fsBin
	if bin == "" {
		bin = "mke2fs"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("image: mke2fs not found (install e2fsprogs): %w", err)
	}
	args := []string{
		"-q", "-F", "-t", "ext4",
		"-U", digestUUID(digestHex, "volume"),
		"-E", "hash_seed=" + digestUUID(digestHex, "seed"),
		"-d", rootfs, diskPath,
	}
	out, err := b.runMke2fs(ctx, bin, args)
	if err == nil {
		return nil
	}
	// Very old e2fsprogs may not understand -U/-E hash_seed; retry once with
	// the minimal invocation so the disk is still built.
	if isMkfsOptionError(out) {
		simple := []string{"-q", "-F", "-t", "ext4", "-d", rootfs, diskPath}
		out2, err2 := b.runMke2fs(ctx, bin, simple)
		if err2 == nil {
			return nil
		}
		return fmt.Errorf("image: mke2fs %s: %w: %s (retry without determinism flags: %w: %s)",
			diskPath, err, truncateOutput(string(out)), err2, truncateOutput(string(out2)))
	}
	return fmt.Errorf("image: mke2fs %s: %w: %s", diskPath, err, truncateOutput(string(out)))
}

func (b *FSBuilder) runMke2fs(ctx context.Context, bin string, args []string) ([]byte, error) {
	// E2FSPROGS_FAKE_TIME makes e2fsprogs timestamps reproducible; older
	// versions simply ignore the variable.
	env := []string{"E2FSPROGS_FAKE_TIME=1704067200"}
	if b.runCmd != nil {
		return b.runCmd(ctx, env, bin, args...)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), env...)
	return cmd.CombinedOutput()
}

func isMkfsOptionError(out []byte) bool {
	return bytes.Contains(out, []byte("hash_seed")) ||
		bytes.Contains(out, []byte("Bad option")) ||
		bytes.Contains(out, []byte("Invalid"))
}

// digestUUID derives a stable UUID for mke2fs flags from the content digest.
func digestUUID(digestHex, purpose string) string {
	sum := sha256.Sum256([]byte("fastvm-uuid:" + purpose + ":" + digestHex))
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func checkReadableFile(path, what string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("image: %s %q: %w", what, path, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("image: %s %q is not a regular file", what, path)
	}
	return nil
}

// truncateOutput keeps error messages readable.
func truncateOutput(s string) string {
	const max = 4096
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return "..." + s[len(s)-max:]
}

// writeFileAtomic writes data to path via a temporary file and rename.
func writeFileAtomic(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".fastvm-tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if tmp != "" {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	tmp = "" // renamed: nothing to clean up
	return nil
}
