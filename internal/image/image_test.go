package image

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"fastvm/internal/types"
)

// ---- helpers ---------------------------------------------------------------

func requireMke2fs(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("mke2fs"); err != nil {
		t.Skip("mke2fs not installed (e2fsprogs missing)")
	}
}

func newTestBuilder(t *testing.T, baseDir string) *FSBuilder {
	t.Helper()
	b, err := NewBuilder(Options{BaseDir: baseDir})
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	return b
}

func writeTestFile(t *testing.T, path, content string, perm os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatalf("chmod %q: %v", path, err)
	}
	return path
}

// makeRootfs builds a tiny but structurally interesting rootfs.
func makeRootfs(t *testing.T, root string) {
	t.Helper()
	writeTestFile(t, filepath.Join(root, "etc", "os-release"), "ID=fastvm-test\nVERSION=1\n", 0o644)
	writeTestFile(t, filepath.Join(root, "bin", "busybox"), "#!/bin/sh\necho busybox\n", 0o755)
	writeTestFile(t, filepath.Join(root, "etc", "hostname"), "fastvm\n", 0o644)
	if err := os.Symlink("/bin/busybox", filepath.Join(root, "bin", "sh")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "private"), 0o700); err != nil {
		t.Fatalf("mkdir private: %v", err)
	}
}

func diskHex(t *testing.T, disk string) string {
	t.Helper()
	hex, ok := digestHexFromRef(disk)
	if !ok {
		t.Fatalf("invalid digest %q", disk)
	}
	return hex
}

// debugfsRun runs a read-only debugfs command and returns stdout and stderr.
// debugfs reports missing files on stderr while still exiting 0.
func debugfsRun(t *testing.T, debugfs, diskPath, command string) (string, string) {
	t.Helper()
	cmd := exec.Command(debugfs, "-R", command, diskPath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("debugfs %q: %v: %s", command, err, stderr.String())
	}
	return stdout.String(), stderr.String()
}

// requireDebugfs skips the injection verification when e2fsprogs' debugfs is
// not installed.
func requireDebugfs(t *testing.T) (string, bool) {
	t.Helper()
	if os.Getenv("FASTVM_SKIP_DEBUGFS") == "1" {
		return "", false
	}
	path, err := exec.LookPath("debugfs")
	if err != nil {
		return "", false
	}
	return path, true
}

var debugfsModeRE = regexp.MustCompile(`(?m)Mode:\s+([0-7]{4})`)

func checkDebugfsFile(t *testing.T, debugfs, diskPath, guestPath, wantContent, wantMode string) {
	t.Helper()
	if got, _ := debugfsRun(t, debugfs, diskPath, "cat "+guestPath); got != wantContent {
		t.Errorf("debugfs cat %s = %q, want %q", guestPath, got, wantContent)
	}
	stat, _ := debugfsRun(t, debugfs, diskPath, "stat "+guestPath)
	m := debugfsModeRE.FindStringSubmatch(stat)
	if m == nil {
		t.Errorf("debugfs stat %s: no mode found in %q", guestPath, stat)
	} else if m[1] != wantMode {
		t.Errorf("debugfs stat %s mode = %s, want %s", guestPath, m[1], wantMode)
	}
}

func checkDebugfsMissing(t *testing.T, debugfs, diskPath, guestPath string) {
	t.Helper()
	stdout, stderr := debugfsRun(t, debugfs, diskPath, "stat "+guestPath)
	if !strings.Contains(stderr, "File not found") && !strings.Contains(stdout, "File not found") {
		t.Errorf("debugfs stat %s: expected file to be absent, got stdout=%q stderr=%q", guestPath, stdout, stderr)
	}
}

// ---- Ingest ----------------------------------------------------------------

func TestIngest(t *testing.T) {
	requireMke2fs(t)
	dir := t.TempDir()
	baseDir := filepath.Join(dir, "images")
	b := newTestBuilder(t, baseDir)
	root := filepath.Join(dir, "rootfs")
	makeRootfs(t, root)
	agent := writeTestFile(t, filepath.Join(dir, "fastvm-agent"), "#!/bin/sh\necho agent\n", 0o755)
	initScript := writeTestFile(t, filepath.Join(dir, "init.sh"), "#!/bin/sh\nexec /usr/local/bin/fastvm-agent --test\n", 0o755)

	ctx := t.Context()
	spec := IngestSpec{
		RootfsDir: root,
		Tag:       "fastvm/test:latest",
		SizeGB:    1,
		AgentPath: agent,
		InitPath:  initScript,
	}
	disk, err := b.Ingest(ctx, spec)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	hexDigest := diskHex(t, disk.Digest)
	if !strings.HasPrefix(disk.Digest, "sha256:") {
		t.Errorf("digest %q lacks sha256: prefix", disk.Digest)
	}
	if got, want := disk.Path, filepath.Join(baseDir, hexDigest+".img"); got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
	if disk.Name != spec.Tag || disk.Tag != spec.Tag {
		t.Errorf("Name/Tag = %q/%q, want %q", disk.Name, disk.Tag, spec.Tag)
	}
	if disk.Source != "rootfs" {
		t.Errorf("Source = %q, want rootfs", disk.Source)
	}
	if disk.SizeBytes != 1<<30 {
		t.Errorf("SizeBytes = %d, want %d", disk.SizeBytes, int64(1)<<30)
	}
	if disk.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}

	fi, err := os.Stat(disk.Path)
	if err != nil {
		t.Fatalf("stat disk: %v", err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Errorf("disk mode = %v, want regular 0600", fi.Mode())
	}
	if fi.Size() != disk.SizeBytes {
		t.Errorf("disk size = %d, want %d", fi.Size(), disk.SizeBytes)
	}

	metaPath := filepath.Join(baseDir, hexDigest+".json")
	metaData, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	var meta diskMeta
	if err := json.Unmarshal(metaData, &meta); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if meta.FormatVersion != formatVersion || meta.Digest != disk.Digest ||
		meta.SizeBytes != disk.SizeBytes || meta.Source != "rootfs" || meta.Tag != spec.Tag {
		t.Errorf("metadata mismatch: %+v", meta)
	}
	// The metadata must also be readable as a types.BaseDisk by other packages.
	var asBase struct {
		Name   string `json:"name"`
		Digest string `json:"digest"`
		Path   string `json:"path"`
		Source string `json:"source"`
	}
	if err := json.Unmarshal(metaData, &asBase); err != nil || asBase.Digest != disk.Digest {
		t.Errorf("metadata not BaseDisk-compatible: %+v, err=%v", asBase, err)
	}

	// Tag index must exist.
	tagData, err := os.ReadFile(filepath.Join(baseDir, tagsDir, url.PathEscape(spec.Tag)))
	if err != nil {
		t.Fatalf("read tag index: %v", err)
	}
	if strings.TrimSpace(string(tagData)) != hexDigest {
		t.Errorf("tag index = %q, want %q", strings.TrimSpace(string(tagData)), hexDigest)
	}

	// Injection verification through debugfs (read-only, no mount needed).
	if debugfs, ok := requireDebugfs(t); ok {
		checkDebugfsFile(t, debugfs, disk.Path, "/init", "#!/bin/sh\nexec /usr/local/bin/fastvm-agent --test\n", "0755")
		checkDebugfsFile(t, debugfs, disk.Path, "/usr/local/bin/fastvm-agent", "#!/bin/sh\necho agent\n", "0755")
		checkDebugfsFile(t, debugfs, disk.Path, "/etc/os-release", "ID=fastvm-test\nVERSION=1\n", "0644")
		checkDebugfsFile(t, debugfs, disk.Path, "/bin/busybox", "#!/bin/sh\necho busybox\n", "0755")
		t.Logf("injection verified via debugfs")
	} else {
		t.Log("debugfs unavailable: skipping injection verification")
	}

	// Content addressing: identical inputs must resolve to the same disk and
	// the cache hit must not rebuild it.
	before, err := os.Stat(disk.Path)
	if err != nil {
		t.Fatalf("stat disk: %v", err)
	}
	again, err := b.Ingest(ctx, spec)
	if err != nil {
		t.Fatalf("second Ingest: %v", err)
	}
	if again.Digest != disk.Digest || again.Path != disk.Path {
		t.Errorf("second Ingest = %q/%q, want %q/%q", again.Digest, again.Path, disk.Digest, disk.Path)
	}
	if !again.CreatedAt.Equal(disk.CreatedAt) {
		t.Errorf("cache hit changed CreatedAt: %v != %v", again.CreatedAt, disk.CreatedAt)
	}
	after, err := os.Stat(disk.Path)
	if err != nil {
		t.Fatalf("stat disk after cache hit: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("cache hit rebuilt the disk: mtime changed %v -> %v", before.ModTime(), after.ModTime())
	}

	// Resolution by tag, hex digest and sha256: digest.
	for _, ref := range []string{spec.Tag, hexDigest, disk.Digest} {
		got, err := b.Get(ctx, ref)
		if err != nil {
			t.Fatalf("Get(%q): %v", ref, err)
		}
		if got.Digest != disk.Digest || got.Path != disk.Path {
			t.Errorf("Get(%q) = %+v, want digest %s", ref, got, disk.Digest)
		}
	}

	disks, err := b.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(disks) != 1 || disks[0].Digest != disk.Digest {
		t.Errorf("List = %+v, want exactly the ingested disk", disks)
	}

	// Cross-instance cache: a fresh Builder over the same dir hits the cache.
	other := newTestBuilder(t, baseDir)
	fromOther, err := other.Ingest(ctx, spec)
	if err != nil {
		t.Fatalf("Ingest from second builder: %v", err)
	}
	if fromOther.Digest != disk.Digest {
		t.Errorf("digest from second builder = %s, want %s", fromOther.Digest, disk.Digest)
	}
}

func TestIngestDefaults(t *testing.T) {
	requireMke2fs(t)
	dir := t.TempDir()
	root := filepath.Join(dir, "rootfs")
	makeRootfs(t, root)
	ctx := t.Context()

	t.Run("default size and builtin init", func(t *testing.T) {
		baseDir := filepath.Join(dir, "images-default")
		b := newTestBuilder(t, baseDir)
		disk, err := b.Ingest(ctx, IngestSpec{RootfsDir: root, Tag: "default:1"})
		if err != nil {
			t.Fatalf("Ingest: %v", err)
		}
		if want := int64(DefaultSizeGB) << 30; disk.SizeBytes != want {
			t.Errorf("SizeBytes = %d, want %d", disk.SizeBytes, want)
		}
		if disk.Source != "rootfs" {
			t.Errorf("Source = %q", disk.Source)
		}
		debugfs, ok := requireDebugfs(t)
		if !ok {
			t.Skip("debugfs unavailable: skipping init/agent verification")
		}
		initContent, _ := debugfsRun(t, debugfs, disk.Path, "cat /init")
		if !strings.Contains(initContent, "/usr/local/bin/fastvm-agent") {
			t.Errorf("builtin /init does not exec the agent: %q", initContent)
		}
		if !strings.Contains(initContent, "/bin/sh") {
			t.Errorf("builtin /init lacks the shell fallback: %q", initContent)
		}
		// No agent supplied: nothing is injected at the agent path.
		checkDebugfsMissing(t, debugfs, disk.Path, "/usr/local/bin/fastvm-agent")
		// Runtime mount points must exist for the init script.
		for _, p := range []string{"/proc", "/sys", "/dev", "/tmp"} {
			stat, _ := debugfsRun(t, debugfs, disk.Path, "stat "+p)
			if !strings.Contains(stat, "Mode:") {
				t.Errorf("runtime dir %s missing: %q", p, stat)
			}
		}
	})

	t.Run("Options defaults for agent and init", func(t *testing.T) {
		agent := writeTestFile(t, filepath.Join(dir, "agent-default"), "agent-default\n", 0o755)
		initScript := writeTestFile(t, filepath.Join(dir, "init-default"), "init-default\n", 0o755)
		b, err := NewBuilder(Options{
			BaseDir:   filepath.Join(dir, "images-opts"),
			AgentPath: agent,
			InitPath:  initScript,
		})
		if err != nil {
			t.Fatalf("NewBuilder: %v", err)
		}
		disk, err := b.Ingest(ctx, IngestSpec{RootfsDir: root, Tag: "opts:1", SizeGB: 1})
		if err != nil {
			t.Fatalf("Ingest: %v", err)
		}
		debugfs, ok := requireDebugfs(t)
		if !ok {
			t.Skip("debugfs unavailable: skipping injection verification")
		}
		checkDebugfsFile(t, debugfs, disk.Path, "/init", "init-default\n", "0755")
		checkDebugfsFile(t, debugfs, disk.Path, "/usr/local/bin/fastvm-agent", "agent-default\n", "0755")
	})
}

func TestContentAddressing(t *testing.T) {
	requireMke2fs(t)
	dir := t.TempDir()
	baseDir := filepath.Join(dir, "images")
	b := newTestBuilder(t, baseDir)
	root := filepath.Join(dir, "rootfs")
	makeRootfs(t, root)
	agent := writeTestFile(t, filepath.Join(dir, "agent"), "#!/bin/sh\n", 0o755)
	initA := writeTestFile(t, filepath.Join(dir, "init-a"), "# variant a\n", 0o755)
	initB := writeTestFile(t, filepath.Join(dir, "init-b"), "# variant b\n", 0o755)

	spec := func(sizeGB int, initPath string) IngestSpec {
		return IngestSpec{RootfsDir: root, SizeGB: sizeGB, AgentPath: agent, InitPath: initPath}
	}
	ctx := t.Context()
	first, err := b.Ingest(ctx, spec(1, initA))
	if err != nil {
		t.Fatalf("Ingest baseline: %v", err)
	}

	// A second rootfs with one different file must hash differently.
	otherRoot := filepath.Join(dir, "rootfs-other")
	makeRootfs(t, otherRoot)
	writeTestFile(t, filepath.Join(otherRoot, "etc", "extra"), "extra\n", 0o644)

	tests := []struct {
		name     string
		spec     IngestSpec
		wantSame bool
	}{
		{"identical inputs", spec(1, initA), true},
		{"different init content", spec(1, initB), false},
		{"different disk size", spec(2, initA), false},
		{"different rootfs file", IngestSpec{RootfsDir: otherRoot, SizeGB: 1, AgentPath: agent, InitPath: initA}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			disk, err := b.Ingest(ctx, tc.spec)
			if err != nil {
				t.Fatalf("Ingest: %v", err)
			}
			same := disk.Digest == first.Digest
			if same != tc.wantSame {
				t.Errorf("digest same = %v, want %v (%s vs %s)", same, tc.wantSame, disk.Digest, first.Digest)
			}
			if tc.wantSame && disk.Path != first.Path {
				t.Errorf("same inputs resolved to different paths: %q vs %q", disk.Path, first.Path)
			}
		})
	}
}

func TestGetAndList(t *testing.T) {
	requireMke2fs(t)
	dir := t.TempDir()
	b := newTestBuilder(t, filepath.Join(dir, "images"))
	root := filepath.Join(dir, "rootfs")
	makeRootfs(t, root)
	ctx := t.Context()

	tags := []string{"one:a", "ns/two:b", "three:c"}
	want := make(map[string]string, len(tags))
	for i, tag := range tags {
		initScript := writeTestFile(t, filepath.Join(dir, "init", tag+".sh"), "# init "+tag+"\n", 0o755)
		disk, err := b.Ingest(ctx, IngestSpec{
			RootfsDir: root,
			Tag:       tag,
			SizeGB:    1,
			InitPath:  initScript,
		})
		if err != nil {
			t.Fatalf("Ingest(%d): %v", i, err)
		}
		want[tag] = disk.Digest
	}

	disks, err := b.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(disks) != len(tags) {
		t.Fatalf("List returned %d disks, want %d", len(disks), len(tags))
	}
	for i := 1; i < len(disks); i++ {
		if disks[i].CreatedAt.After(disks[i-1].CreatedAt) {
			t.Errorf("List is not newest-first at index %d", i)
		}
	}
	seen := make(map[string]bool, len(disks))
	for _, disk := range disks {
		seen[disk.Digest] = true
	}
	for tag, digest := range want {
		if !seen[digest] {
			t.Errorf("List missing digest %s for tag %s", digest, tag)
		}
		got, err := b.Get(ctx, tag)
		if err != nil {
			t.Fatalf("Get(%q): %v", tag, err)
		}
		if got.Digest != digest || got.Tag != tag || got.Name != tag {
			t.Errorf("Get(%q) = %+v, want digest %s name/tag %q", tag, got, digest, tag)
		}
		if byDigest, err := b.Get(ctx, diskHex(t, digest)); err != nil || byDigest.Digest != digest {
			t.Errorf("Get(hex) = %+v, err=%v", byDigest, err)
		}
	}

	for _, ref := range []string{"", "   ", "does-not-exist", "sha256:" + strings.Repeat("0", 64)} {
		if _, err := b.Get(ctx, ref); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%q) error = %v, want ErrNotFound", ref, err)
		}
	}
}

// TestTagAliasSameDigest checks that ingesting identical content under a new
// tag records the new tag -> existing digest mapping and that both tags resolve
// to the same cached disk.
func TestTagAliasSameDigest(t *testing.T) {
	requireMke2fs(t)
	dir := t.TempDir()
	b := newTestBuilder(t, filepath.Join(dir, "images"))
	root := filepath.Join(dir, "rootfs")
	makeRootfs(t, root)
	agent := writeTestFile(t, filepath.Join(dir, "agent"), "#!/bin/sh\n", 0o755)
	ctx := t.Context()

	first, err := b.Ingest(ctx, IngestSpec{RootfsDir: root, Tag: "base:one", SizeGB: 1, AgentPath: agent})
	if err != nil {
		t.Fatalf("Ingest(base:one): %v", err)
	}
	// Same inputs, new tag: must hit the existing digest and record the alias.
	alias, err := b.Ingest(ctx, IngestSpec{RootfsDir: root, Tag: "alias:two", SizeGB: 1, AgentPath: agent})
	if err != nil {
		t.Fatalf("Ingest(alias:two): %v", err)
	}
	if alias.Digest != first.Digest || alias.Path != first.Path {
		t.Errorf("alias digest/path = %s/%s, want %s/%s", alias.Digest, alias.Path, first.Digest, first.Path)
	}
	if alias.Tag != "alias:two" || alias.Name != "alias:two" {
		t.Errorf("alias Tag/Name = %q/%q, want alias:two", alias.Tag, alias.Name)
	}

	for _, ref := range []string{"base:one", "alias:two"} {
		got, err := b.Get(ctx, ref)
		if err != nil {
			t.Fatalf("Get(%q): %v", ref, err)
		}
		if got.Digest != first.Digest {
			t.Errorf("Get(%q).Digest = %s, want %s", ref, got.Digest, first.Digest)
		}
	}
	aliased, err := b.Get(ctx, "alias:two")
	if err != nil {
		t.Fatalf("Get(alias:two): %v", err)
	}
	if aliased.Tag != "alias:two" || aliased.Name != "alias:two" {
		t.Errorf("Get(alias:two) Tag/Name = %q/%q, want alias:two", aliased.Tag, aliased.Name)
	}

	// The alias must not create a second cache entry.
	disks, err := b.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(disks) != 1 || disks[0].Digest != first.Digest {
		t.Errorf("List = %+v, want exactly the shared digest", disks)
	}
}

// ---- validation ------------------------------------------------------------

func TestValidation(t *testing.T) {
	dir := t.TempDir()
	b := newTestBuilder(t, filepath.Join(dir, "images"))
	root := filepath.Join(dir, "rootfs")
	makeRootfs(t, root)
	notADir := writeTestFile(t, filepath.Join(dir, "file-rootfs"), "x", 0o644)
	agent := writeTestFile(t, filepath.Join(dir, "agent"), "#!/bin/sh\n", 0o755)
	missing := filepath.Join(dir, "missing")
	ctx := t.Context()

	tests := []struct {
		name    string
		run     func() error
		wantErr string
	}{
		{
			name: "ingest empty rootfs",
			run: func() error {
				_, err := b.Ingest(ctx, IngestSpec{Tag: "x"})
				return err
			},
			wantErr: "rootfs directory is required",
		},
		{
			name: "ingest missing rootfs",
			run: func() error {
				_, err := b.Ingest(ctx, IngestSpec{RootfsDir: missing, Tag: "x", SizeGB: 1})
				return err
			},
			wantErr: "rootfs directory",
		},
		{
			name: "ingest rootfs not a directory",
			run: func() error {
				_, err := b.Ingest(ctx, IngestSpec{RootfsDir: notADir, Tag: "x", SizeGB: 1})
				return err
			},
			wantErr: "not a directory",
		},
		{
			name: "ingest negative size",
			run: func() error {
				_, err := b.Ingest(ctx, IngestSpec{RootfsDir: root, Tag: "x", SizeGB: -1})
				return err
			},
			wantErr: "size_gb",
		},
		{
			name: "ingest missing agent",
			run: func() error {
				_, err := b.Ingest(ctx, IngestSpec{RootfsDir: root, Tag: "x", SizeGB: 1, AgentPath: missing})
				return err
			},
			wantErr: "agent",
		},
		{
			name: "ingest missing init",
			run: func() error {
				_, err := b.Ingest(ctx, IngestSpec{RootfsDir: root, Tag: "x", SizeGB: 1, AgentPath: agent, InitPath: missing})
				return err
			},
			wantErr: "init script",
		},
		{
			name: "ingest control character tag",
			run: func() error {
				_, err := b.Ingest(ctx, IngestSpec{RootfsDir: root, Tag: "bad\x00tag", SizeGB: 1})
				return err
			},
			wantErr: "control characters",
		},
		{
			name: "build empty tag",
			run: func() error {
				_, err := b.Build(ctx, BuildSpec{ContextDir: root})
				return err
			},
			wantErr: "tag is required",
		},
		{
			name: "build empty context",
			run: func() error {
				_, err := b.Build(ctx, BuildSpec{Tag: "t:1"})
				return err
			},
			wantErr: "build context dir is required",
		},
		{
			name: "build missing context",
			run: func() error {
				_, err := b.Build(ctx, BuildSpec{Tag: "t:1", ContextDir: missing})
				return err
			},
			wantErr: "build context",
		},
		{
			name: "build missing Dockerfile",
			run: func() error {
				empty := t.TempDir()
				_, err := b.Build(ctx, BuildSpec{Tag: "t:1", ContextDir: empty})
				return err
			},
			wantErr: "Dockerfile",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if err == nil {
				t.Fatalf("expected an error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestBuildDockerUnavailable(t *testing.T) {
	dir := t.TempDir()
	ctxDir := filepath.Join(dir, "ctx")
	writeTestFile(t, filepath.Join(ctxDir, "Dockerfile"), "FROM scratch\n", 0o644)
	ctx := t.Context()

	tests := []struct {
		name      string
		dockerBin string
	}{
		{"missing docker binary", "/nonexistent/docker"},
		{"failing docker binary", "false"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := NewBuilder(Options{BaseDir: filepath.Join(dir, "images"), DockerBin: tc.dockerBin})
			if err != nil {
				t.Fatalf("NewBuilder: %v", err)
			}
			_, err = b.Build(ctx, BuildSpec{Tag: "t:1", ContextDir: ctxDir, SizeGB: 1})
			if !errors.Is(err, ErrDockerUnavailable) {
				t.Fatalf("Build error = %v, want ErrDockerUnavailable", err)
			}
			if err == nil || !strings.Contains(err.Error(), "docker") {
				t.Errorf("error should mention docker: %v", err)
			}
		})
	}
}

func TestNewBuilderValidation(t *testing.T) {
	if _, err := NewBuilder(Options{}); err == nil {
		t.Fatal("NewBuilder with empty BaseDir should fail")
	}
	dir := t.TempDir()
	b, err := NewBuilder(Options{BaseDir: filepath.Join(dir, "images"), WorkDir: filepath.Join(dir, "work")})
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	if b.baseDir != filepath.Join(dir, "images") || b.workDir != filepath.Join(dir, "work") {
		t.Errorf("builder dirs = %q/%q", b.baseDir, b.workDir)
	}
	if _, err := os.Stat(filepath.Join(dir, "images")); err != nil {
		t.Errorf("base dir not created: %v", err)
	}
}

func TestCancelledContext(t *testing.T) {
	requireMke2fs(t)
	dir := t.TempDir()
	b := newTestBuilder(t, filepath.Join(dir, "images"))
	root := filepath.Join(dir, "rootfs")
	makeRootfs(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.Ingest(ctx, IngestSpec{RootfsDir: root, Tag: "x", SizeGB: 1}); !errors.Is(err, context.Canceled) {
		t.Errorf("Ingest with cancelled context error = %v, want context.Canceled", err)
	}
	if _, err := b.List(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("List with cancelled context error = %v, want context.Canceled", err)
	}
	if _, err := b.Get(ctx, "x"); !errors.Is(err, context.Canceled) {
		t.Errorf("Get with cancelled context error = %v, want context.Canceled", err)
	}
}

// A mutable tag can point at only one digest. Rebuilding under the same tag must
// make the previous disk untagged in List, not look like a second image with the
// same tag.
func TestListMovedTagLeavesOldDiskUntagged(t *testing.T) {
	requireMke2fs(t)
	dir := t.TempDir()
	b := newTestBuilder(t, filepath.Join(dir, "images"))
	root := filepath.Join(dir, "rootfs")
	makeRootfs(t, root)
	agent := writeTestFile(t, filepath.Join(dir, "agent"), "#!/bin/sh\n", 0o755)
	initA := writeTestFile(t, filepath.Join(dir, "init-a.sh"), "#!/bin/sh\necho a\n", 0o755)
	initB := writeTestFile(t, filepath.Join(dir, "init-b.sh"), "#!/bin/sh\necho b\n", 0o755)
	tag := "fastvm/claude:v6"

	first, err := b.Ingest(t.Context(), IngestSpec{RootfsDir: root, Tag: tag, SizeGB: 1, AgentPath: agent, InitPath: initA})
	if err != nil {
		t.Fatalf("Ingest(first): %v", err)
	}
	second, err := b.Ingest(t.Context(), IngestSpec{RootfsDir: root, Tag: tag, SizeGB: 1, AgentPath: agent, InitPath: initB})
	if err != nil {
		t.Fatalf("Ingest(second): %v", err)
	}
	if first.Digest == second.Digest {
		t.Fatal("test expects different digests for different init scripts")
	}

	disks, err := b.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byDigest := make(map[string]types.BaseDisk, len(disks))
	for _, disk := range disks {
		byDigest[disk.Digest] = disk
	}
	oldDisk, ok := byDigest[first.Digest]
	if !ok {
		t.Fatalf("old disk missing from List: %+v", disks)
	}
	if oldDisk.Tag != "" || oldDisk.Name != "" || len(oldDisk.Tags) != 0 {
		t.Errorf("old disk still advertises moved tag: %+v", oldDisk)
	}
	newDisk, ok := byDigest[second.Digest]
	if !ok {
		t.Fatalf("new disk missing from List: %+v", disks)
	}
	if newDisk.Tag != tag || newDisk.Name != tag || len(newDisk.Tags) != 1 || newDisk.Tags[0] != tag {
		t.Errorf("new disk tag = %+v, want %q", newDisk, tag)
	}
	got, err := b.Get(t.Context(), tag)
	if err != nil {
		t.Fatalf("Get(%q): %v", tag, err)
	}
	if got.Digest != second.Digest {
		t.Errorf("Get(%q) digest = %s, want %s", tag, got.Digest, second.Digest)
	}
}

func TestRemoveImageDeletesDiskAndAllTags(t *testing.T) {
	requireMke2fs(t)
	dir := t.TempDir()
	b := newTestBuilder(t, filepath.Join(dir, "images"))
	root := filepath.Join(dir, "rootfs")
	makeRootfs(t, root)
	ctx := t.Context()

	disk, err := b.Ingest(ctx, IngestSpec{RootfsDir: root, Tag: "remove:one", SizeGB: 1})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if _, err := b.Ingest(ctx, IngestSpec{RootfsDir: root, Tag: "remove:alias", SizeGB: 1}); err != nil {
		t.Fatalf("Ingest alias: %v", err)
	}
	removed, err := b.Remove(ctx, "remove:one")
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if removed.Digest != disk.Digest {
		t.Errorf("removed digest = %s, want %s", removed.Digest, disk.Digest)
	}
	if _, err := os.Stat(disk.Path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("disk file still exists: %v", err)
	}
	for _, tag := range []string{"remove:one", "remove:alias"} {
		if _, err := b.Get(ctx, tag); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%q) error = %v, want ErrNotFound", tag, err)
		}
	}
	disks, err := b.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(disks) != 0 {
		t.Fatalf("List after remove = %+v, want empty", disks)
	}
	if _, err := b.Remove(ctx, "remove:one"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Remove error = %v, want ErrNotFound", err)
	}
}
