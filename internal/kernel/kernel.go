// Package kernel manages Cloud Hypervisor guest kernels. Only the pinned
// cloud-hypervisor/linux repository is supported.
package kernel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	// Repo is the only kernel source fastvm downloads from.
	Repo = "cloud-hypervisor/linux"
	// SystemDir is the root-owned kernel directory used by the daemon default.
	SystemDir = "/var/lib/fastvm/kernels"
)

// apiBase is a variable so tests can point Remote at a local server.
var apiBase = "https://api.github.com"

// Asset is one downloadable release asset.
type Asset struct {
	Release     string `json:"release"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	DownloadURL string `json:"download_url"`
}

// Release is one cloud-hypervisor/linux release.
type Release struct {
	Tag         string    `json:"tag"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []Asset   `json:"assets"`
}

// LocalKernel is one kernel file in the local cache.
type LocalKernel struct {
	Path   string
	Name   string
	Size   int64
	SHA256 string
}

// NormalizeArch maps common architecture spellings to Go-style names.
func NormalizeArch(arch string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(arch)) {
	case "amd64", "x86_64", "x64":
		return "amd64", nil
	case "arm64", "aarch64":
		return "arm64", nil
	case "riscv64", "riscv":
		return "riscv64", nil
	default:
		return "", fmt.Errorf("kernel: unsupported architecture %q", arch)
	}
}

// HostArch returns the normalized host architecture.
func HostArch() (string, error) { return NormalizeArch(runtime.GOARCH) }

// DefaultAsset returns the best kernel asset for the current host architecture.
func DefaultAsset() (string, error) {
	arch, err := HostArch()
	if err != nil {
		return "", err
	}
	switch arch {
	case "amd64":
		return "vmlinux-x86_64", nil
	case "arm64":
		return "Image-arm64", nil
	default:
		return "Image-riscv", nil
	}
}

// DirectBootAsset reports whether an asset is a kernel image fastvm boots
// directly. Bootloader-oriented bzImage and compressed images are excluded.
func DirectBootAsset(name string) bool {
	switch name {
	case "vmlinux-x86_64", "Image-arm64", "Image-riscv":
		return true
	default:
		return false
	}
}

// AssetMatchesArch reports whether an asset name targets the given architecture.
func AssetMatchesArch(name, arch string) bool {
	switch arch {
	case "amd64":
		return strings.HasSuffix(name, "-x86_64")
	case "arm64":
		return strings.HasSuffix(name, "-arm64") || strings.HasSuffix(name, "-arm64.gz")
	case "riscv64":
		return strings.HasSuffix(name, "-riscv") || strings.HasSuffix(name, "-riscv.gz")
	default:
		return false
	}
}

type githubRelease struct {
	TagName     string    `json:"tag_name"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []struct {
		Name               string `json:"name"`
		Size               int64  `json:"size"`
		Digest             string `json:"digest"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

// Remote lists cloud-hypervisor/linux releases with SHA-256 digests.
func Remote(ctx context.Context, client *http.Client, limit int) ([]Release, error) {
	if limit <= 0 {
		limit = 10
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	url := fmt.Sprintf("%s/repos/%s/releases?per_page=%d", apiBase, Repo, limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "fastvm")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kernel: list releases: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return nil, fmt.Errorf("kernel: list releases: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var raw []githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("kernel: decode releases: %w", err)
	}
	releases := make([]Release, 0, len(raw))
	for _, rel := range raw {
		out := Release{Tag: rel.TagName, PublishedAt: rel.PublishedAt}
		for _, a := range rel.Assets {
			out.Assets = append(out.Assets, Asset{
				Release:     rel.TagName,
				Name:        a.Name,
				Size:        a.Size,
				SHA256:      strings.TrimPrefix(a.Digest, "sha256:"),
				DownloadURL: a.BrowserDownloadURL,
			})
		}
		releases = append(releases, out)
	}
	return releases, nil
}

// FindAsset selects an asset from a release.
func FindAsset(rel Release, name string) (Asset, bool) {
	for _, a := range rel.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// LatestRelease returns the newest release in the list.
func LatestRelease(releases []Release) (Release, bool) {
	if len(releases) == 0 {
		return Release{}, false
	}
	return releases[0], true
}

// FindRelease returns a release by tag.
func FindRelease(releases []Release, tag string) (Release, bool) {
	for _, rel := range releases {
		if rel.Tag == tag {
			return rel, true
		}
	}
	return Release{}, false
}

// SplitLocalName splits "<release>-<asset>" using the known direct-boot assets.
func SplitLocalName(name string) (release, asset string, ok bool) {
	for _, candidate := range []string{"vmlinux-x86_64", "Image-arm64", "Image-riscv"} {
		if strings.HasSuffix(name, "-"+candidate) {
			return strings.TrimSuffix(name, "-"+candidate), candidate, true
		}
	}
	return "", "", false
}

// LocalPath returns where an asset is stored: <dir>/<release>-<asset>.
func LocalPath(dir string, asset Asset) string {
	return filepath.Join(dir, asset.Release+"-"+asset.Name)
}

// ListLocal lists kernel files in dir. A matching .sha256 sidecar is used when
// present; otherwise SHA256 is empty.
func ListLocal(dir string) ([]LocalKernel, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []LocalKernel
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 ||
			strings.HasSuffix(entry.Name(), ".sha256") || strings.HasSuffix(entry.Name(), ".tmp") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		k := LocalKernel{Path: filepath.Join(dir, entry.Name()), Name: entry.Name(), Size: info.Size()}
		if data, err := os.ReadFile(k.Path + ".sha256"); err == nil {
			fields := strings.Fields(string(data))
			if len(fields) > 0 {
				k.SHA256 = fields[0]
			}
		}
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ErrNotFound means no local kernel matched a reference.
var ErrNotFound = errors.New("kernel not found")

// Resolved is a locally installed, direct-boot kernel selected by reference.
type Resolved struct {
	Path    string
	Name    string
	Release string
	Asset   string
}

// Resolve selects a local kernel by release tag or full local name.
func Resolve(dir, ref string) (Resolved, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Resolved{}, fmt.Errorf("%w: empty reference", ErrNotFound)
	}
	arch, err := HostArch()
	if err != nil {
		return Resolved{}, err
	}
	items, err := ListLocal(dir)
	if err != nil {
		return Resolved{}, err
	}
	var matches []Resolved
	for _, item := range items {
		release, asset, ok := SplitLocalName(item.Name)
		if !ok || !AssetMatchesArch(item.Name, arch) {
			continue
		}
		if item.Name != ref && release != ref {
			continue
		}
		matches = append(matches, Resolved{Path: item.Path, Name: item.Name, Release: release, Asset: asset})
	}
	switch len(matches) {
	case 0:
		return Resolved{}, fmt.Errorf("%w: %q", ErrNotFound, ref)
	case 1:
		return matches[0], nil
	default:
		return Resolved{}, fmt.Errorf("kernel %q is ambiguous", ref)
	}
}

// Available returns installed direct-boot kernel names for error messages.
func Available(dir string) []string {
	items, err := ListLocal(dir)
	if err != nil {
		return nil
	}
	arch, _ := HostArch()
	seen := make(map[string]bool)
	var names []string
	for _, item := range items {
		release, _, ok := SplitLocalName(item.Name)
		if !ok || !AssetMatchesArch(item.Name, arch) || seen[release] {
			continue
		}
		seen[release] = true
		names = append(names, release)
	}
	sort.Strings(names)
	return names
}

// Current returns the file that the "current" symlink points to, if any.
func Current(dir string) (string, error) {
	target, err := os.Readlink(filepath.Join(dir, "current"))
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(dir, target)
	}
	return target, nil
}

// Use points the current kernel symlink at a downloaded release. The daemon
// config should use <dir>/current as its kernel_path.
func Use(dir, version string) (string, error) {
	version = strings.TrimSpace(version)
	if version == "" {
		return "", errors.New("kernel: version is required")
	}
	resolved, err := Resolve(dir, version)
	if err != nil {
		return "", err
	}
	tmp := filepath.Join(dir, ".current.tmp")
	_ = os.Remove(tmp)
	if err := os.Symlink(resolved.Name, tmp); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, filepath.Join(dir, "current")); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return resolved.Path, nil
}

// Download fetches and verifies one asset into dir. It returns the local path.
// Existing files are accepted only when their SHA-256 matches.
func Download(ctx context.Context, client *http.Client, asset Asset, dir string, force bool) (string, error) {
	if asset.SHA256 == "" {
		return "", fmt.Errorf("kernel: %s has no SHA-256 digest", asset.Name)
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := LocalPath(dir, asset)
	if !force {
		if got, err := fileSHA256(dst); err == nil {
			if strings.EqualFold(got, asset.SHA256) {
				return dst, nil
			}
			return "", fmt.Errorf("kernel: %s exists with SHA-256 %s, want %s; use --force", dst, got, asset.SHA256)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.DownloadURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "fastvm")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("kernel: download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("kernel: download %s: %s", asset.Name, resp.Status)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(dst)+".tmp-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, asset.SHA256) {
		return "", fmt.Errorf("kernel: SHA-256 mismatch for %s: got %s, want %s", asset.Name, got, asset.SHA256)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return "", err
	}
	ok = true
	if err := os.WriteFile(dst+".sha256", []byte(asset.SHA256+"  "+filepath.Base(dst)+"\n"), 0o644); err != nil {
		return "", err
	}
	return dst, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
