package l2switch

import (
	"bytes"
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	bpfobj "fastvm/internal/switch/bpf"
)

// TestEmbeddedObjectPresent checks that a compiled eBPF object is committed and
// embedded (go:embed would fail the build otherwise, but a stale/empty file is
// still worth catching).
func TestEmbeddedObjectPresent(t *testing.T) {
	obj := bpfobj.L2Switch
	if len(obj) == 0 {
		t.Fatal("embedded bpf object is empty; run make -C bpf")
	}
	if len(obj) < 4 || !bytes.Equal(obj[:4], []byte{0x7f, 'E', 'L', 'F'}) {
		t.Fatalf("embedded bpf object is not an ELF (first bytes %x)", obj[:min(4, len(obj))])
	}
}

// TestObjectMatchesSource regenerates the object from bpf/l2switch.c with the
// documented flags and compares it with the embedded copy, so a source change
// without `make -C bpf` is caught. Skips when clang or the source is absent;
// FASTVM_BPF_COMPARE=0 downgrades a mismatch to a skip for toolchains that are
// not byte-identical to the one that produced the committed object.
func TestObjectMatchesSource(t *testing.T) {
	clang, err := exec.LookPath("clang")
	if err != nil {
		t.Skip("clang not available; skipping eBPF regenerate-and-compare")
	}
	bpfDir, err := filepath.Abs(filepath.Join("..", "..", "bpf"))
	if err != nil {
		t.Fatalf("resolve bpf dir: %v", err)
	}
	src := filepath.Join(bpfDir, "l2switch.c")
	if _, err := os.Stat(src); err != nil {
		t.Skipf("bpf source not available: %v", err)
	}

	out := filepath.Join(t.TempDir(), "l2switch_bpfel.o")
	cmd := exec.Command(clang,
		"-O2", "-g", "-target", "bpf", "-D__TARGET_ARCH_x86",
		"-Wall", "-Werror", "-c", "l2switch.c", "-o", out)
	cmd.Dir = bpfDir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile bpf/l2switch.c: %v\n%s", err, output)
	}
	regenerated, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read regenerated object: %v", err)
	}
	if len(regenerated) == 0 {
		t.Fatal("regenerated object is empty")
	}

	if bytes.Equal(regenerated, bpfobj.L2Switch) {
		return
	}
	got := sha256.Sum256(regenerated)
	want := sha256.Sum256(bpfobj.L2Switch)
	msg := "embedded eBPF object is stale: regenerate with `make -C bpf`\n" +
		"  embedded:    " + byteDigest(want[:]) + "\n" +
		"  regenerated: " + byteDigest(got[:])
	if os.Getenv("FASTVM_BPF_COMPARE") == "0" {
		t.Skip(msg)
	}
	t.Error(msg)
}

func byteDigest(b []byte) string {
	const hex = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, hex[c>>4], hex[c&0x0f])
	}
	return string(out)
}

// TestEBPFLoadGated attempts to load the real object. It skips when the
// environment cannot load eBPF (this is the normal developer case:
// unprivileged_bpf_disabled=2); on a privileged host it exercises the loader,
// map init and ringbuf setup without attaching to a port.
func TestEBPFLoadGated(t *testing.T) {
	if err := Probe(); err != nil {
		t.Skipf("eBPF unavailable: %v", err)
	}
	s, err := Open(Config{Mode: ModeEBPF, IsolateSandboxes: true, WakeNotify: true})
	if err != nil {
		if IsUnsupported(err) {
			t.Skipf("eBPF load not permitted in this environment: %v", err)
		}
		t.Fatalf("Open eBPF: %v", err)
	}
	if s.Mode() != ModeEBPF {
		t.Errorf("Mode = %q", s.Mode())
	}
	ctx := t.Context()
	if _, err := s.Stats(ctx, "missing"); err == nil {
		t.Error("Stats for unknown port succeeded")
	}
	if err := s.SetAsleep(ctx, "missing", true); err == nil {
		t.Error("SetAsleep for unknown port succeeded")
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}
