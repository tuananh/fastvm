package volume

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"fastvm/internal/types"
)

const testBaseSize int64 = 8 << 20 // 8MiB sparse base disk

// ---- helpers ---------------------------------------------------------------

func newTestManager(t *testing.T, volDir, defaultCOW string) *FSManager {
	t.Helper()
	m, err := NewManager(Options{VolDir: volDir, DefaultCOW: defaultCOW})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

// tempDirOnWorkFS creates a temporary directory on the same filesystem as the
// package source tree. On the reference host that is ext4 (which, unlike the
// tmpfs usually backing t.TempDir, does not support reflink), so the copy
// fallback can be exercised for real.
func tempDirOnWorkFS(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(".", ".voltest-*")
	if err != nil {
		// Fall back to the regular temp directory if the source tree is not
		// writable.
		return t.TempDir()
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Logf("cleanup %q: %v", dir, err)
		}
	})
	return dir
}

// makeBaseDiskFile creates a sparse base disk file of size bytes with a known
// magic payload written at the middle of the file. It returns the BaseDisk
// metadata, the payload and the payload offset.
func makeBaseDiskFile(t *testing.T, path string, size int64) (types.BaseDisk, []byte, int64) {
	t.Helper()
	payload := make([]byte, 4096)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	offset := size / 2
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create base disk: %v", err)
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		t.Fatalf("truncate base disk: %v", err)
	}
	if _, err := f.WriteAt(payload, offset); err != nil {
		_ = f.Close()
		t.Fatalf("write base payload: %v", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		t.Fatalf("fsync base disk: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close base disk: %v", err)
	}
	return types.BaseDisk{
		Name:      "test-base",
		Tag:       "test:base",
		Digest:    "sha256:" + strings.Repeat("ab", 32),
		Path:      path,
		SizeBytes: size,
		Source:    "rootfs",
	}, payload, offset
}

func readAt(t *testing.T, path string, offset int64, length int) []byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %q: %v", path, err)
	}
	defer f.Close()
	buf := make([]byte, length)
	if _, err := f.ReadAt(buf, offset); err != nil {
		t.Fatalf("read %q at %d: %v", path, offset, err)
	}
	return buf
}

func statVolume(t *testing.T, path string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	return fi
}

// fsSupportsReflink probes dir for FICLONE support.
func fsSupportsReflink(t *testing.T, dir string) bool {
	t.Helper()
	src, err := os.CreateTemp(dir, "reflink-src-*")
	if err != nil {
		t.Fatalf("create reflink probe: %v", err)
	}
	defer func() {
		name := src.Name()
		_ = src.Close()
		_ = os.Remove(name)
	}()
	if _, err := src.Write(make([]byte, 4096)); err != nil {
		t.Fatalf("write reflink probe: %v", err)
	}
	dst, err := os.CreateTemp(dir, "reflink-dst-*")
	if err != nil {
		t.Fatalf("create reflink probe: %v", err)
	}
	defer func() {
		name := dst.Name()
		_ = dst.Close()
		_ = os.Remove(name)
	}()
	return tryReflink(dst, src) == nil
}

// checkCloneContent verifies that vol is a byte-identical clone of base and
// that writes to the clone do not affect the base (isolation).
func checkCloneContent(t *testing.T, vol Volume, base types.BaseDisk, payload []byte, offset int64) {
	t.Helper()
	if vol.SizeBytes != base.SizeBytes {
		t.Errorf("volume size = %d, want %d", vol.SizeBytes, base.SizeBytes)
	}
	fi := statVolume(t, vol.Path)
	if fi.Size() != base.SizeBytes {
		t.Errorf("volume file size = %d, want %d", fi.Size(), base.SizeBytes)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("volume mode = %v, want 0600", fi.Mode().Perm())
	}
	if got := readAt(t, vol.Path, offset, len(payload)); !bytes.Equal(got, payload) {
		t.Errorf("volume payload differs from base")
	}
	if got := readAt(t, vol.Path, 0, 4096); !bytes.Equal(got, make([]byte, 4096)) {
		t.Errorf("volume hole does not read as zero")
	}

	// Isolation: overwrite the payload in the clone; the base must not change.
	replacement := bytes.Repeat([]byte{0xff}, len(payload))
	f, err := os.OpenFile(vol.Path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open volume for write: %v", err)
	}
	if _, err := f.WriteAt(replacement, offset); err != nil {
		_ = f.Close()
		t.Fatalf("write volume: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close volume: %v", err)
	}
	if got := readAt(t, base.Path, offset, len(payload)); !bytes.Equal(got, payload) {
		t.Errorf("write to volume leaked into the base disk")
	}
}

// ---- tests -----------------------------------------------------------------

func TestCreateCopy(t *testing.T) {
	dir := t.TempDir()
	base, payload, offset := makeBaseDiskFile(t, filepath.Join(dir, "base.img"), testBaseSize)
	volDir := filepath.Join(dir, "volumes")
	m := newTestManager(t, volDir, COWCopy)
	ctx := t.Context()

	vol, err := m.Create(ctx, "sb-1", base, 0, COWCopy)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if vol.COW != COWCopy {
		t.Errorf("COW = %q, want %q", vol.COW, COWCopy)
	}
	if vol.SandboxID != "sb-1" || vol.BaseDisk != base.Digest {
		t.Errorf("volume = %+v, want sandbox sb-1 based on %s", vol, base.Digest)
	}
	if want := filepath.Join(volDir, "sb-1.img"); vol.Path != want {
		t.Errorf("Path = %q, want %q", vol.Path, want)
	}
	checkCloneContent(t, vol, base, payload, offset)

	// Metadata sidecar must exist and be readable by Get.
	if _, err := os.Stat(filepath.Join(volDir, "sb-1.json")); err != nil {
		t.Errorf("metadata sidecar missing: %v", err)
	}
	got, ok, err := m.Get(ctx, "sb-1")
	if err != nil || !ok {
		t.Fatalf("Get = %+v, %v, %v; want present", got, ok, err)
	}
	if got != vol {
		t.Errorf("Get = %+v, want %+v", got, vol)
	}

	if err := m.Remove(ctx, "sb-1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok, err := m.Get(ctx, "sb-1"); err != nil || ok {
		t.Errorf("Get after Remove = %v, %v; want absent", ok, err)
	}
	if err := m.Remove(ctx, "sb-1"); err != nil {
		t.Errorf("second Remove should be idempotent: %v", err)
	}
	if _, err := os.Stat(vol.Path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("volume file still exists after Remove: %v", err)
	}
}

func TestSparseCopy(t *testing.T) {
	dir := t.TempDir()
	base, payload, offset := makeBaseDiskFile(t, filepath.Join(dir, "base.img"), testBaseSize)
	m := newTestManager(t, filepath.Join(dir, "volumes"), COWCopy)
	vol, err := m.Create(t.Context(), "sb-sparse", base, 0, COWCopy)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := readAt(t, vol.Path, offset, len(payload)); !bytes.Equal(got, payload) {
		t.Error("payload not copied")
	}
	fi := statVolume(t, vol.Path)
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("stat_t not available; cannot check sparse allocation")
	}
	allocated := int64(st.Blocks) * 512
	if allocated >= testBaseSize/2 {
		t.Errorf("copy allocated %d of %d bytes; hole preservation failed", allocated, testBaseSize)
	}
}

func TestCreateReflinkOrCopyFallback(t *testing.T) {
	dir := tempDirOnWorkFS(t)
	base, payload, offset := makeBaseDiskFile(t, filepath.Join(dir, "base.img"), testBaseSize)
	volDir := filepath.Join(dir, "volumes")
	m := newTestManager(t, volDir, COWReflink)
	supportsReflink := fsSupportsReflink(t, dir)

	vol, err := m.Create(t.Context(), "sb-reflink", base, 0, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	checkCloneContent(t, vol, base, payload, offset)

	if supportsReflink && vol.COW != COWReflink {
		t.Errorf("filesystem supports reflink but COW = %q, want %q", vol.COW, COWReflink)
	}
	if !supportsReflink && vol.COW != COWCopy {
		t.Errorf("filesystem lacks reflink but COW = %q, want %q (copy fallback)", vol.COW, COWCopy)
	}
	t.Logf("clone mode on %s: %s (reflink supported: %v)", dir, vol.COW, supportsReflink)
}

func TestCreateReflinkWhenSupported(t *testing.T) {
	dir := tempDirOnWorkFS(t)
	if !fsSupportsReflink(t, dir) {
		t.Skip("filesystem does not support reflink")
	}
	base, payload, offset := makeBaseDiskFile(t, filepath.Join(dir, "base.img"), testBaseSize)
	m := newTestManager(t, filepath.Join(dir, "volumes"), COWReflink)
	vol, err := m.Create(t.Context(), "sb-rl", base, 0, COWReflink)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if vol.COW != COWReflink {
		t.Errorf("COW = %q, want %q", vol.COW, COWReflink)
	}
	checkCloneContent(t, vol, base, payload, offset)
}

func TestCreateDefaultCOW(t *testing.T) {
	dir := t.TempDir()
	base, _, _ := makeBaseDiskFile(t, filepath.Join(dir, "base.img"), testBaseSize)
	m := newTestManager(t, filepath.Join(dir, "volumes"), COWCopy)
	ctx := t.Context()
	vol, err := m.Create(ctx, "sb-default", base, 0, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if vol.COW != COWCopy {
		t.Errorf("COW = %q, want default %q", vol.COW, COWCopy)
	}

	// A volume created through the empty-clone default must be retrievable and
	// removable exactly like an explicitly cloned one.
	got, ok, err := m.Get(ctx, "sb-default")
	if err != nil || !ok {
		t.Fatalf("Get after default Create = %+v, %v, %v; want present", got, ok, err)
	}
	if got != vol {
		t.Errorf("Get after default Create = %+v, want %+v", got, vol)
	}
	if err := m.Remove(ctx, "sb-default"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok, err := m.Get(ctx, "sb-default"); err != nil || ok {
		t.Errorf("Get after Remove = %v, %v; want absent", ok, err)
	}
	if _, err := os.Stat(vol.Path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("volume file still exists after Remove: %v", err)
	}

	// An empty DefaultCOW means reflink, which falls back to copy here.
	m2 := newTestManager(t, filepath.Join(dir, "volumes2"), "")
	if m2.defaultCOW != COWReflink {
		t.Errorf("default COW = %q, want %q", m2.defaultCOW, COWReflink)
	}
}

func TestCreateIdempotent(t *testing.T) {
	dir := t.TempDir()
	base, _, _ := makeBaseDiskFile(t, filepath.Join(dir, "base.img"), testBaseSize)
	m := newTestManager(t, filepath.Join(dir, "volumes"), COWCopy)
	ctx := t.Context()
	first, err := m.Create(ctx, "sb-idem", base, 0, COWCopy)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	second, err := m.Create(ctx, "sb-idem", base, 0, COWCopy)
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if second != first {
		t.Errorf("idempotent Create = %+v, want %+v", second, first)
	}

	other := base
	other.Digest = "sha256:" + strings.Repeat("cd", 32)
	if _, err := m.Create(ctx, "sb-idem", other, 0, COWCopy); !errors.Is(err, ErrVolumeExists) {
		t.Errorf("Create with different base error = %v, want ErrVolumeExists", err)
	}
}

func TestCreateSizeMismatch(t *testing.T) {
	dir := t.TempDir()
	basePath := filepath.Join(dir, "base.img")
	base, _, _ := makeBaseDiskFile(t, basePath, testBaseSize)
	notAFile := filepath.Join(dir, "dir-base")
	if err := os.Mkdir(notAFile, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	tests := []struct {
		name   string
		base   types.BaseDisk
		sizeGB int
		wantIs error
	}{
		{
			name:   "requested GiB differs from base",
			base:   base,
			sizeGB: 1,
			wantIs: ErrSizeMismatch,
		},
		{
			name:   "metadata size differs from file",
			base:   types.BaseDisk{Path: basePath, SizeBytes: testBaseSize + 4096, Digest: base.Digest},
			wantIs: ErrSizeMismatch,
		},
		{
			name:   "negative size_gb",
			base:   base,
			sizeGB: -1,
			wantIs: ErrSizeMismatch,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestManager(t, filepath.Join(dir, "volumes"), COWCopy)
			_, err := m.Create(t.Context(), "sb-size", tc.base, tc.sizeGB, COWCopy)
			if !errors.Is(err, tc.wantIs) {
				t.Fatalf("Create error = %v, want %v", err, tc.wantIs)
			}
		})
	}

	// Base path validation errors.
	t.Run("empty base path", func(t *testing.T) {
		m := newTestManager(t, filepath.Join(dir, "volumes"), COWCopy)
		if _, err := m.Create(t.Context(), "sb-x", types.BaseDisk{}, 0, COWCopy); err == nil {
			t.Fatal("expected error for empty base path")
		}
	})
	t.Run("missing base", func(t *testing.T) {
		m := newTestManager(t, filepath.Join(dir, "volumes"), COWCopy)
		if _, err := m.Create(t.Context(), "sb-x", types.BaseDisk{Path: filepath.Join(dir, "nope.img")}, 0, COWCopy); err == nil {
			t.Fatal("expected error for missing base disk")
		}
	})
	t.Run("base not a regular file", func(t *testing.T) {
		m := newTestManager(t, filepath.Join(dir, "volumes"), COWCopy)
		if _, err := m.Create(t.Context(), "sb-x", types.BaseDisk{Path: notAFile}, 0, COWCopy); err == nil {
			t.Fatal("expected error for directory base")
		}
	})
}

func TestCreateValidation(t *testing.T) {
	dir := t.TempDir()
	base, _, _ := makeBaseDiskFile(t, filepath.Join(dir, "base.img"), testBaseSize)
	m := newTestManager(t, filepath.Join(dir, "volumes"), COWCopy)

	tests := []struct {
		name  string
		id    string
		clone string
		want  string
	}{
		{"empty sandbox id", "", COWCopy, "sandbox id is required"},
		{"dot dot id", "..", COWCopy, "invalid sandbox id"},
		{"dot id", ".", COWCopy, "invalid sandbox id"},
		{"hidden id", ".hidden", COWCopy, "invalid sandbox id"},
		{"slash id", "a/b", COWCopy, "path separator"},
		{"backslash id", `a\b`, COWCopy, "path separator"},
		{"too long id", strings.Repeat("x", maxIDLen+1), COWCopy, "too long"},
		{"invalid clone mode", "sb-ok", "bogus", "invalid clone mode"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := m.Create(t.Context(), tc.id, base, 0, tc.clone)
			if err == nil {
				t.Fatalf("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}

	t.Run("dm-thin not supported", func(t *testing.T) {
		_, err := m.Create(t.Context(), "sb-ok", base, 0, COWDMThin)
		if !errors.Is(err, ErrDMThinUnsupported) {
			t.Fatalf("error = %v, want ErrDMThinUnsupported", err)
		}
		if !strings.Contains(err.Error(), COWReflink) || !strings.Contains(err.Error(), COWCopy) {
			t.Errorf("dm-thin error should suggest reflink/copy: %v", err)
		}
	})
}

func TestManagerOptions(t *testing.T) {
	if _, err := NewManager(Options{}); err == nil {
		t.Fatal("NewManager with empty VolDir should fail")
	}
	if _, err := NewManager(Options{VolDir: t.TempDir(), DefaultCOW: "bogus"}); err == nil {
		t.Fatal("NewManager with invalid DefaultCOW should fail")
	}
	dir := filepath.Join(t.TempDir(), "volumes")
	m, err := NewManager(Options{VolDir: dir})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if m.defaultCOW != COWReflink {
		t.Errorf("default COW = %q, want %q", m.defaultCOW, COWReflink)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Errorf("volume dir not created: %v", err)
	}
}

func TestGetEdgeCases(t *testing.T) {
	dir := t.TempDir()
	base, _, _ := makeBaseDiskFile(t, filepath.Join(dir, "base.img"), testBaseSize)
	volDir := filepath.Join(dir, "volumes")
	m := newTestManager(t, volDir, COWCopy)
	ctx := t.Context()

	if _, ok, err := m.Get(ctx, "nope"); err != nil || ok {
		t.Errorf("Get(missing) = %v, %v; want absent", ok, err)
	}
	if _, _, err := m.Get(ctx, "a/b"); err == nil {
		t.Error("Get with invalid sandbox id should fail")
	}

	vol, err := m.Create(ctx, "sb-corrupt", base, 0, COWCopy)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := os.WriteFile(filepath.Join(volDir, "sb-corrupt.json"), []byte("not json"), 0o600); err != nil {
		t.Fatalf("corrupt metadata: %v", err)
	}
	if _, _, err := m.Get(ctx, "sb-corrupt"); err == nil {
		t.Error("Get with corrupt metadata should fail")
	}
	// Remove must clean up even with corrupt metadata.
	if err := m.Remove(ctx, "sb-corrupt"); err != nil {
		t.Errorf("Remove: %v", err)
	}
	if _, err := os.Stat(vol.Path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("volume file still exists: %v", err)
	}

	// Metadata present but image gone: treated as absent.
	if _, err := m.Create(ctx, "sb-orphan", base, 0, COWCopy); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := os.Remove(filepath.Join(volDir, "sb-orphan.img")); err != nil {
		t.Fatalf("remove volume file: %v", err)
	}
	if _, ok, err := m.Get(ctx, "sb-orphan"); err != nil || ok {
		t.Errorf("Get(orphan) = %v, %v; want absent", ok, err)
	}
}

func TestRemoveCleansTempFiles(t *testing.T) {
	dir := t.TempDir()
	m := newTestManager(t, filepath.Join(dir, "volumes"), COWCopy)
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	leftover := filepath.Join(m.dir, ".tmp-sb-temp-123.img")
	if err := os.WriteFile(leftover, []byte("x"), 0o600); err != nil {
		t.Fatalf("write leftover: %v", err)
	}
	if err := m.Remove(t.Context(), "sb-temp"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(leftover); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temp leftover not removed: %v", err)
	}
}

func TestCancelledContext(t *testing.T) {
	dir := t.TempDir()
	base, _, _ := makeBaseDiskFile(t, filepath.Join(dir, "base.img"), testBaseSize)
	m := newTestManager(t, filepath.Join(dir, "volumes"), COWCopy)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Create(ctx, "sb-x", base, 0, COWCopy); !errors.Is(err, context.Canceled) {
		t.Errorf("Create error = %v, want context.Canceled", err)
	}
	if _, _, err := m.Get(ctx, "sb-x"); !errors.Is(err, context.Canceled) {
		t.Errorf("Get error = %v, want context.Canceled", err)
	}
	if err := m.Remove(ctx, "sb-x"); !errors.Is(err, context.Canceled) {
		t.Errorf("Remove error = %v, want context.Canceled", err)
	}
}
