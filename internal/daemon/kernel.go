package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"fastvm/internal/kernel"
	"fastvm/internal/manager"
	"fastvm/internal/types"
)

// kernelDir is the directory managed by fastvmctl kernel. The config
// kernel_path should point at <dir>/current.
func (d *Daemon) kernelDir() string {
	if d.cfg.KernelPath == "" {
		return kernel.SystemDir
	}
	return filepath.Dir(d.cfg.KernelPath)
}

// ListKernels lists installed kernels and marks the current one active.
func (d *Daemon) ListKernels(_ context.Context) (types.KernelList, error) {
	dir := d.kernelDir()
	items, err := kernel.ListLocal(dir)
	if err != nil {
		return types.KernelList{}, err
	}
	current, _ := kernel.Current(dir)
	out := types.KernelList{Dir: dir, Current: current, Kernels: make([]types.KernelInfo, 0, len(items)+1)}
	seen := make(map[string]bool)
	for _, item := range items {
		info := kernelInfo(item, current)
		out.Kernels = append(out.Kernels, info)
		seen[item.Path] = true
	}
	// The demo may point current outside the managed directory (for example a
	// repo-relative test kernel); still expose it as the active entry.
	if current != "" && !seen[current] {
		if st, err := os.Stat(current); err == nil {
			out.Kernels = append(out.Kernels, types.KernelInfo{
				Name: filepath.Base(current), Path: current, SizeBytes: st.Size(), Active: true,
			})
		}
	}
	return out, nil
}

func kernelInfo(item kernel.LocalKernel, current string) types.KernelInfo {
	info := types.KernelInfo{
		Name: item.Name, Path: item.Path, SizeBytes: item.Size, SHA256: item.SHA256,
		Active: current != "" && item.Path == current,
	}
	if release, asset, ok := kernel.SplitLocalName(item.Name); ok {
		info.Release, info.Asset = release, asset
	}
	return info
}

func parseKernelRef(release, asset string) (string, string, error) {
	tag, namedAsset, ok := kernel.SplitLocalName(release)
	if !ok {
		return release, asset, nil
	}
	if asset != "" && asset != namedAsset {
		return "", "", fmt.Errorf("kernel: %q names asset %q, not %q", release, namedAsset, asset)
	}
	return tag, namedAsset, nil
}

// defaultSandboxKernel pins the daemon's current kernel into a new sandbox so a
// later "kernel use" does not change kernels under an existing sandbox.
func (d *Daemon) defaultSandboxKernel() (release, path string) {
	path = d.cfg.KernelPath
	if path == "" {
		return "", ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return "", ""
	}
	if rel, _, ok := kernel.SplitLocalName(filepath.Base(path)); ok {
		release = rel
	}
	return release, path
}

func (d *Daemon) resolveSandboxKernel(ref string) (kernel.Resolved, error) {
	resolved, err := kernel.Resolve(d.kernelDir(), ref)
	if errors.Is(err, kernel.ErrNotFound) {
		available := kernel.Available(d.kernelDir())
		if len(available) == 0 {
			return kernel.Resolved{}, fmt.Errorf("%w: kernel %q is not installed; no kernels are installed (run fastvmctl kernel download)", manager.ErrInvalid, ref)
		}
		return kernel.Resolved{}, fmt.Errorf("%w: kernel %q is not installed; available: %s",
			manager.ErrInvalid, ref, strings.Join(available, ", "))
	}
	return resolved, err
}

// DownloadKernel downloads and verifies a kernel into the managed directory.
func (d *Daemon) DownloadKernel(ctx context.Context, req types.KernelDownloadRequest) (types.KernelDownloadResult, error) {
	releaseTag, assetName, err := parseKernelRef(req.Release, req.Asset)
	if err != nil {
		return types.KernelDownloadResult{}, err
	}
	releases, err := kernel.Remote(ctx, &http.Client{Timeout: 30 * time.Second}, 30)
	if err != nil {
		return types.KernelDownloadResult{}, err
	}
	rel, ok := kernel.LatestRelease(releases)
	if releaseTag != "" {
		rel, ok = kernel.FindRelease(releases, releaseTag)
		if !ok {
			return types.KernelDownloadResult{}, fmt.Errorf("kernel release %q not found", releaseTag)
		}
	}
	if !ok {
		return types.KernelDownloadResult{}, fmt.Errorf("kernel: no releases found")
	}
	if assetName == "" {
		assetName, err = kernel.DefaultAsset()
		if err != nil {
			return types.KernelDownloadResult{}, err
		}
	}
	asset, ok := kernel.FindAsset(rel, assetName)
	if !ok {
		return types.KernelDownloadResult{}, fmt.Errorf("kernel release %s has no asset %q", rel.Tag, assetName)
	}
	path, err := kernel.Download(ctx, &http.Client{Timeout: 10 * time.Minute}, asset, d.kernelDir(), req.Force)
	if err != nil {
		return types.KernelDownloadResult{}, err
	}
	return types.KernelDownloadResult{
		Path: path, Release: asset.Release, Asset: asset.Name, SHA256: asset.SHA256,
	}, nil
}

// UseKernel switches the current symlink to a downloaded release.
func (d *Daemon) UseKernel(_ context.Context, release string) (types.KernelUseResult, error) {
	dir := d.kernelDir()
	path, err := kernel.Use(dir, release)
	if err != nil {
		return types.KernelUseResult{}, err
	}
	return types.KernelUseResult{Path: path, Current: filepath.Join(dir, "current")}, nil
}
