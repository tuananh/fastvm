package kernel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func sha(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestRemoteParsesDigests(t *testing.T) {
	var downloadURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/cloud-hypervisor/linux/releases" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"tag_name": "release-1",
			"assets": []map[string]any{{
				"name": "vmlinux-x86_64", "size": 42,
				"digest":               "sha256:" + sha([]byte("kernel")),
				"browser_download_url": downloadURL,
			}},
		}})
	}))
	defer srv.Close()
	downloadURL = srv.URL + "/kernel"
	old := apiBase
	apiBase = srv.URL
	defer func() { apiBase = old }()

	releases, err := Remote(context.Background(), srv.Client(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(releases) != 1 || releases[0].Tag != "release-1" {
		t.Fatalf("releases = %+v", releases)
	}
	asset, ok := FindAsset(releases[0], "vmlinux-x86_64")
	if !ok || asset.SHA256 != sha([]byte("kernel")) || asset.Size != 42 {
		t.Fatalf("asset = %+v ok=%v", asset, ok)
	}
}

func TestDownloadVerifiesSHA(t *testing.T) {
	data := []byte("verified kernel bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(data)
	}))
	defer srv.Close()
	dir := t.TempDir()
	asset := Asset{
		Release: "release-1", Name: "vmlinux", Size: int64(len(data)),
		SHA256: sha(data), DownloadURL: srv.URL,
	}
	path, err := Download(context.Background(), srv.Client(), asset, dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(data) {
		t.Fatalf("downloaded = %q err=%v", got, err)
	}
	if _, err := Download(context.Background(), srv.Client(), asset, dir, false); err != nil {
		t.Fatalf("verified existing file should be reused: %v", err)
	}

	bad := asset
	bad.SHA256 = sha([]byte("different"))
	if _, err := Download(context.Background(), srv.Client(), bad, t.TempDir(), false); err == nil {
		t.Fatal("SHA mismatch should fail")
	}
}

func TestAssetMatchesArch(t *testing.T) {
	cases := []struct {
		name, arch string
		want       bool
	}{
		{"vmlinux-x86_64", "amd64", true},
		{"bzImage-x86_64", "amd64", true},
		{"Image-arm64", "amd64", false},
		{"Image-arm64", "arm64", true},
		{"Image-arm64.gz", "arm64", true},
		{"Image-riscv", "riscv64", true},
		{"Image-riscv.gz", "riscv64", true},
	}
	for _, tc := range cases {
		if got := AssetMatchesArch(tc.name, tc.arch); got != tc.want {
			t.Errorf("AssetMatchesArch(%q, %q) = %v, want %v", tc.name, tc.arch, got, tc.want)
		}
	}
	if _, err := NormalizeArch("mips"); err == nil {
		t.Fatal("unsupported arch should fail")
	}
}

func TestDirectBootAsset(t *testing.T) {
	for _, name := range []string{"vmlinux-x86_64", "Image-arm64", "Image-riscv"} {
		if !DirectBootAsset(name) {
			t.Errorf("DirectBootAsset(%q) = false", name)
		}
	}
	for _, name := range []string{"bzImage-x86_64", "Image-arm64.gz", "Image-riscv.gz"} {
		if DirectBootAsset(name) {
			t.Errorf("DirectBootAsset(%q) = true", name)
		}
	}
}

func TestUseSelectsCurrent(t *testing.T) {
	dir := t.TempDir()
	data := []byte("kernel")
	name := "release-1-vmlinux-x86_64"
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
	// Bootloader-oriented assets must not be selected.
	if err := os.WriteFile(filepath.Join(dir, "release-1-bzImage-x86_64"), []byte("bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, err := Use(dir, "release-1")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != name {
		t.Fatalf("path = %s", path)
	}
	current, err := Current(dir)
	if err != nil {
		t.Fatal(err)
	}
	if current != path {
		t.Fatalf("current = %s, want %s", current, path)
	}
	if _, err := Use(dir, "missing"); err == nil {
		t.Fatal("missing release should fail")
	}
	// Full local names are accepted too.
	if _, err := Use(dir, name); err != nil {
		t.Fatalf("use by full name: %v", err)
	}
}

func TestListLocal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kernel.bin"), []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kernel.bin.sha256"), []byte(sha([]byte("abc"))+"  kernel.bin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	items, err := ListLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].SHA256 != sha([]byte("abc")) || items[0].Size != 3 {
		t.Fatalf("items = %+v", items)
	}
}
