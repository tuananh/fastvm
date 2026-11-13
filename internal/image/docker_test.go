package image

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// dockerDaemonAvailable reports whether a real docker daemon answers.
func dockerDaemonAvailable(t *testing.T) bool {
	t.Helper()
	cmd := exec.Command("docker", "info")
	if err := cmd.Run(); err != nil {
		return false
	}
	return true
}

// ensureLocalImage makes sure the given image is present locally, pulling it
// when needed. Pull failures are reported as "not available" so the test can
// skip cleanly on machines without registry access.
func ensureLocalImage(t *testing.T, image string) bool {
	t.Helper()
	if err := exec.Command("docker", "image", "inspect", image).Run(); err == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "pull", image).CombinedOutput()
	if err != nil {
		t.Logf("docker pull %s failed: %v: %s", image, err, strings.TrimSpace(string(out)))
		return false
	}
	return true
}

// TestBuildDocker exercises the full Dockerfile -> OCI -> rootfs -> ext4 path.
// It is gated because it needs a working docker daemon and (usually) network
// access to pull a base image.
func TestBuildDocker(t *testing.T) {
	if os.Getenv("FASTVM_DOCKER_TESTS") != "1" {
		t.Skip("set FASTVM_DOCKER_TESTS=1 to run the docker build integration test")
	}
	requireMke2fs(t)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not installed")
	}
	if !dockerDaemonAvailable(t) {
		t.Skip("docker daemon not available")
	}
	if !ensureLocalImage(t, "busybox:latest") {
		t.Skip("busybox:latest not available locally and could not be pulled")
	}

	dir := t.TempDir()
	ctxDir := filepath.Join(dir, "ctx")
	writeTestFile(t, filepath.Join(ctxDir, "Dockerfile"),
		"FROM busybox:latest\nRUN echo fastvm-docker-test > /hello.txt\n", 0o644)
	agent := writeTestFile(t, filepath.Join(dir, "agent"), "#!/bin/sh\necho docker-agent\n", 0o755)
	initScript := writeTestFile(t, filepath.Join(dir, "init.sh"),
		"#!/bin/sh\nexec /usr/local/bin/fastvm-agent --docker\n", 0o755)
	b := newTestBuilder(t, filepath.Join(dir, "images"))

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	spec := BuildSpec{
		ContextDir: ctxDir,
		Tag:        "fastvm-test:docker",
		SizeGB:     1,
		AgentPath:  agent,
		InitPath:   initScript,
	}
	disk, err := b.Build(ctx, spec)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if disk.Source != "dockerfile" {
		t.Errorf("Source = %q, want dockerfile", disk.Source)
	}
	if disk.Tag != spec.Tag || disk.Name != spec.Tag {
		t.Errorf("Tag/Name = %q/%q, want %q", disk.Tag, disk.Name, spec.Tag)
	}
	if _, ok := digestHexFromRef(disk.Digest); !ok {
		t.Errorf("invalid digest %q", disk.Digest)
	}
	fi, err := os.Stat(disk.Path)
	if err != nil {
		t.Fatalf("stat disk: %v", err)
	}
	if fi.Mode().Perm() != 0o600 || fi.Size() != int64(1)<<30 {
		t.Errorf("disk = mode %v size %d, want 0600 and %d", fi.Mode(), fi.Size(), int64(1)<<30)
	}

	if debugfs, ok := requireDebugfs(t); ok {
		checkDebugfsFile(t, debugfs, disk.Path, "/hello.txt", "fastvm-docker-test\n", "0644")
		checkDebugfsFile(t, debugfs, disk.Path, "/init", "#!/bin/sh\nexec /usr/local/bin/fastvm-agent --docker\n", "0755")
		checkDebugfsFile(t, debugfs, disk.Path, "/usr/local/bin/fastvm-agent", "#!/bin/sh\necho docker-agent\n", "0755")
	} else {
		t.Log("debugfs unavailable: skipping rootfs content verification")
	}

	// Get by tag and by digest.
	if got, err := b.Get(ctx, spec.Tag); err != nil || got.Digest != disk.Digest {
		t.Errorf("Get(tag) = %+v, %v; want digest %s", got, err, disk.Digest)
	}
	// A rebuild of identical inputs must hit the content-addressed cache.
	again, err := b.Build(ctx, spec)
	if err != nil {
		t.Fatalf("second Build: %v", err)
	}
	if again.Digest != disk.Digest {
		t.Errorf("rebuild digest = %s, want %s", again.Digest, disk.Digest)
	}
	if !again.CreatedAt.Equal(disk.CreatedAt) {
		t.Errorf("rebuild was not a cache hit: CreatedAt %v != %v", again.CreatedAt, disk.CreatedAt)
	}
}
