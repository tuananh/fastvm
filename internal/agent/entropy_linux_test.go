package agent

import (
	"encoding/hex"
	"testing"
)

func TestParseSeedFromCmdline(t *testing.T) {
	seed := []byte("0123456789abcdef0123456789abcdef")
	cmdline := "console=ttyS0 root=/dev/vda rw init=/init fastvm.seed=" + hex.EncodeToString(seed)

	got, err := parseSeed(cmdline)
	if err != nil {
		t.Fatalf("parseSeed: %v", err)
	}
	if string(got) != string(seed) {
		t.Fatalf("seed = %x, want %x", got, seed)
	}

	if got, err := parseSeed("console=ttyS0 fastvm.ip=10.0.0.2/30"); err != nil || len(got) != 0 {
		t.Fatalf("unexpected seed without key: %x, %v", got, err)
	}
	if _, err := parseSeed("fastvm.seed=not-hex"); err == nil {
		t.Fatal("expected hex decode error")
	}
}
