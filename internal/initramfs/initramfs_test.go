package initramfs

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildContainsInit(t *testing.T) {
	dir := t.TempDir()
	agent := filepath.Join(dir, "fastvm-agent")
	data := []byte("static-agent-binary")
	if err := os.WriteFile(agent, data, 0o755); err != nil {
		t.Fatal(err)
	}
	path, err := Build(agent, filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "agent-"+sha(data)+".cpio.gz" {
		t.Fatalf("path = %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, []byte(newcMagic)) {
		t.Fatalf("not a newc archive: %q", raw[:6])
	}
	if !bytes.Contains(raw, []byte("init\x00")) || !bytes.Contains(raw, data) {
		t.Fatal("archive is missing /init or the agent payload")
	}
	if !bytes.Contains(raw, []byte(trailer)) {
		t.Fatal("archive is missing the trailer")
	}
	if second, err := Build(agent, filepath.Join(dir, "out")); err != nil || second != path {
		t.Fatalf("cached build = %q err=%v", second, err)
	}
}

func sha(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestIsInitramfsMode(t *testing.T) {
	if !IsInitramfsMode([]byte("console=ttyS0 fastvm.initramfs=1 root=/dev/vda")) {
		t.Fatal("expected initramfs mode")
	}
	if IsInitramfsMode([]byte("console=ttyS0 root=/dev/vda")) {
		t.Fatal("unexpected initramfs mode")
	}
}

func TestBuildMissingAgent(t *testing.T) {
	if _, err := Build(filepath.Join(t.TempDir(), "missing"), t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "agent") {
		t.Fatalf("missing agent error = %v", err)
	}
}
