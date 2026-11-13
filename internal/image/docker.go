package image

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// dockerRootfs builds spec's Dockerfile with BuildKit and returns a temporary
// directory containing the container's unpacked rootfs. The returned cleanup
// function removes the temporary image, container and directory.
func (b *FSBuilder) dockerRootfs(ctx context.Context, spec BuildSpec) (string, func(), error) {
	contextDir, err := filepath.Abs(spec.ContextDir)
	if err != nil {
		return "", nil, fmt.Errorf("image: resolve build context: %w", err)
	}
	info, err := os.Stat(contextDir)
	if err != nil {
		return "", nil, fmt.Errorf("image: build context: %w", err)
	}
	if !info.IsDir() {
		return "", nil, fmt.Errorf("image: build context %q is not a directory", contextDir)
	}
	dockerfile := spec.Dockerfile
	if strings.TrimSpace(dockerfile) == "" {
		dockerfile = filepath.Join(contextDir, "Dockerfile")
	} else if dockerfile, err = filepath.Abs(dockerfile); err != nil {
		return "", nil, fmt.Errorf("image: resolve Dockerfile: %w", err)
	}
	dfInfo, err := os.Stat(dockerfile)
	if err != nil {
		return "", nil, fmt.Errorf("image: Dockerfile: %w", err)
	}
	if dfInfo.IsDir() {
		return "", nil, fmt.Errorf("image: Dockerfile %q is a directory", dockerfile)
	}

	if err := b.checkDocker(ctx); err != nil {
		return "", nil, err
	}

	workDir, err := os.MkdirTemp(b.workDir, ".fastvm-docker-*")
	if err != nil {
		return "", nil, fmt.Errorf("image: create build workspace: %w", err)
	}
	cleanupDir := func() { _ = os.RemoveAll(workDir) }

	// A unique local tag keeps the build isolated: cleanup only ever removes
	// that tag, never an image the user tagged.
	imageTag := tempBuildTag()
	args := []string{"build", "-t", imageTag, "-f", dockerfile, contextDir}
	buildOut, err := b.runDocker(ctx, []string{"DOCKER_BUILDKIT=1"}, args...)
	if err != nil {
		cleanupDir()
		return "", nil, fmt.Errorf("image: docker build: %w: %s", err, truncateOutput(string(buildOut)))
	}

	stdout, stderr, err := b.runDockerStdout(ctx, "create", imageTag, "/bin/true")
	if err != nil {
		b.removeImage(imageTag)
		cleanupDir()
		return "", nil, fmt.Errorf("image: docker create: %w: %s", err, truncateOutput(string(stderr)))
	}
	containerID := strings.TrimSpace(string(stdout))
	if containerID == "" {
		b.removeImage(imageTag)
		cleanupDir()
		return "", nil, errors.New("image: docker create returned an empty container id")
	}
	rootfs := filepath.Join(workDir, "rootfs")
	if err := os.MkdirAll(rootfs, 0o700); err != nil {
		b.removeContainer(containerID)
		b.removeImage(imageTag)
		cleanupDir()
		return "", nil, fmt.Errorf("image: create rootfs dir: %w", err)
	}
	cleanup := func() {
		b.removeContainer(containerID)
		b.removeImage(imageTag)
		cleanupDir()
	}
	if err := b.exportRootfs(ctx, containerID, rootfs); err != nil {
		cleanup()
		return "", nil, err
	}
	return rootfs, cleanup, nil
}

// tempBuildTag returns a unique, throwaway local docker image tag.
func tempBuildTag() string {
	var raw [8]byte
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	if _, err := rand.Read(raw[:]); err == nil {
		suffix = hex.EncodeToString(raw[:])
	}
	return "fastvm-tmp-" + suffix + ":build"
}

// checkDocker verifies that the docker CLI exists and its daemon answers.
func (b *FSBuilder) checkDocker(ctx context.Context) error {
	bin := b.dockerBin
	if bin == "" {
		bin = "docker"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("%w: %v", ErrDockerUnavailable, err)
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := b.runDocker(cctx, nil, "info")
	if err != nil {
		return fmt.Errorf("%w: docker info failed (is the daemon running and accessible?): %v: %s",
			ErrDockerUnavailable, err, truncateOutput(string(out)))
	}
	return nil
}

// exportRootfs runs docker export for containerID and unpacks the tar stream
// into dest.
func (b *FSBuilder) exportRootfs(ctx context.Context, containerID, dest string) error {
	bin := b.dockerBin
	if bin == "" {
		bin = "docker"
	}
	cmd := exec.CommandContext(ctx, bin, "export", containerID)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("image: docker export: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("image: docker export: %w", err)
	}
	extractErr := extractTar(dest, stdout)
	if extractErr != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if extractErr != nil {
		return fmt.Errorf("image: unpack rootfs: %w", extractErr)
	}
	if waitErr != nil {
		return fmt.Errorf("image: docker export: %w: %s", waitErr, truncateOutput(stderr.String()))
	}
	return nil
}

func (b *FSBuilder) removeContainer(containerID string) {
	b.runCleanup(func(ctx context.Context) { _, _ = b.runDocker(ctx, nil, "rm", "-f", containerID) })
}

func (b *FSBuilder) removeImage(imageID string) {
	b.runCleanup(func(ctx context.Context) { _, _ = b.runDocker(ctx, nil, "rmi", imageID) })
}

// runCleanup runs a best-effort cleanup with a fresh, bounded context so a
// cancelled build context cannot leave containers/images behind.
func (b *FSBuilder) runCleanup(fn func(context.Context)) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fn(ctx)
}

// runDockerStdout executes the docker CLI and separates stdout from stderr.
// It is used when parsing command output (e.g. a container id) so daemon
// warnings on stderr cannot corrupt the result.
func (b *FSBuilder) runDockerStdout(ctx context.Context, args ...string) ([]byte, []byte, error) {
	bin := b.dockerBin
	if bin == "" {
		bin = "docker"
	}
	if b.runCmd != nil {
		out, err := b.runCmd(ctx, nil, bin, args...)
		return out, nil, err
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// runDocker executes the docker CLI and returns combined stdout/stderr.
func (b *FSBuilder) runDocker(ctx context.Context, extraEnv []string, args ...string) ([]byte, error) {
	bin := b.dockerBin
	if bin == "" {
		bin = "docker"
	}
	if b.runCmd != nil {
		return b.runCmd(ctx, extraEnv, bin, args...)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), extraEnv...)
	return cmd.CombinedOutput()
}

// pendingHardlink is a tar hard link whose target had not been extracted yet.
type pendingHardlink struct {
	target string
	name   string
}

// extractTar unpacks a rootfs tar stream into dest using os.Root, which makes
// path traversal and symlink-escape attacks impossible by construction.
func extractTar(dest string, r io.Reader) error {
	root, err := os.OpenRoot(dest)
	if err != nil {
		return fmt.Errorf("open rootfs root: %w", err)
	}
	defer root.Close()

	var (
		dirs    []dirMode
		pending []pendingHardlink
		buf     = make([]byte, 1<<20)
	)

	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		name := cleanTarPath(hdr.Name)
		if name == "" {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			mode := tarDirMode(hdr.Mode)
			// os.Root.MkdirAll only accepts permission bits; special bits are
			// applied with Chmod once the tree is populated.
			if err := root.MkdirAll(name, permOnly(mode)); err != nil {
				return fmt.Errorf("mkdir %q: %w", name, err)
			}
			dirs = append(dirs, dirMode{path: name, mode: tarFinalDirMode(hdr.Mode)})
		case tar.TypeReg:
			mode := tarFileMode(hdr.Mode)
			dir := path.Dir(name)
			if dir != "." {
				if err := root.MkdirAll(dir, 0o755); err != nil {
					return fmt.Errorf("mkdir %q: %w", dir, err)
				}
			}
			f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, permOnly(mode))
			if err != nil {
				return fmt.Errorf("create %q: %w", name, err)
			}
			if _, err := io.CopyBuffer(f, tr, buf); err != nil {
				_ = f.Close()
				return fmt.Errorf("write %q: %w", name, err)
			}
			if err := f.Close(); err != nil {
				return fmt.Errorf("close %q: %w", name, err)
			}
			if err := root.Chmod(name, mode); err != nil {
				return fmt.Errorf("chmod %q: %w", name, err)
			}
		case tar.TypeSymlink:
			dir := path.Dir(name)
			if dir != "." {
				if err := root.MkdirAll(dir, 0o755); err != nil {
					return fmt.Errorf("mkdir %q: %w", dir, err)
				}
			}
			// A later entry wins; drop whatever is already there.
			_ = root.Remove(name)
			if err := root.Symlink(hdr.Linkname, name); err != nil {
				return fmt.Errorf("symlink %q -> %q: %w", name, hdr.Linkname, err)
			}
		case tar.TypeLink:
			target := cleanTarPath(hdr.Linkname)
			if target == "" {
				continue
			}
			dir := path.Dir(name)
			if dir != "." {
				if err := root.MkdirAll(dir, 0o755); err != nil {
					return fmt.Errorf("mkdir %q: %w", dir, err)
				}
			}
			if err := root.Link(target, name); err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					pending = append(pending, pendingHardlink{target: target, name: name})
					continue
				}
				return fmt.Errorf("hardlink %q -> %q: %w", name, target, err)
			}
		default:
			// Device nodes, FIFOs, sockets and tar metadata are recreated by
			// the guest at boot (or are irrelevant), so skip them.
		}
	}
	for _, link := range pending {
		if err := root.Link(link.target, link.name); err != nil {
			return fmt.Errorf("hardlink %q -> %q: %w", link.name, link.target, err)
		}
	}
	// Apply directory modes deepest-first once the tree is populated.
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i].path) > len(dirs[j].path) })
	for _, d := range dirs {
		if err := root.Chmod(d.path, d.mode); err != nil {
			return fmt.Errorf("chmod %q: %w", d.path, err)
		}
	}
	return nil
}

// cleanTarPath normalizes a tar entry name to a fs.ValidPath-relative path,
// returning "" for entries that try to escape the rootfs.
func cleanTarPath(name string) string {
	name = strings.TrimPrefix(name, "./")
	cleaned := path.Clean("/" + name)
	cleaned = strings.TrimPrefix(cleaned, "/")
	if cleaned == "" || cleaned == "." || !fs.ValidPath(cleaned) {
		return ""
	}
	return cleaned
}

// unixModeToFileMode converts raw tar/unix mode bits to a Go fs.FileMode with
// the setuid/setgid/sticky flags represented correctly.
func unixModeToFileMode(mode int64) fs.FileMode {
	perm := fs.FileMode(mode) & fs.ModePerm
	if mode&0o4000 != 0 {
		perm |= fs.ModeSetuid
	}
	if mode&0o2000 != 0 {
		perm |= fs.ModeSetgid
	}
	if mode&0o1000 != 0 {
		perm |= fs.ModeSticky
	}
	return perm
}

// permOnly strips everything except the permission bits, which is all that
// os.Root.Mkdir/MkdirAll/OpenFile accept.
func permOnly(mode fs.FileMode) fs.FileMode { return mode & fs.ModePerm }

func tarFileMode(mode int64) fs.FileMode {
	perm := unixModeToFileMode(mode)
	if perm&0o400 == 0 {
		perm |= 0o400 // stays readable even if the archive is odd
	}
	if perm == 0 {
		perm = 0o644
	}
	return perm
}

// tarDirMode is used while extracting: the directory must remain writable for
// its children to be created.
func tarDirMode(mode int64) fs.FileMode {
	return unixModeToFileMode(mode) | 0o700
}

func tarFinalDirMode(mode int64) fs.FileMode {
	perm := unixModeToFileMode(mode)
	if perm == 0 {
		perm = 0o755
	}
	return perm
}
