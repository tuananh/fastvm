//go:build linux

package agent

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCmdlineIdentity(t *testing.T) {
	resolv := "nameserver 8.8.8.8\n"
	hosts := "127.0.0.1 localhost\n"
	cmd := "console=ttyS0 fastvm.hostname=0123456789ab" +
		" fastvm.resolv=" + base64.StdEncoding.EncodeToString([]byte(resolv)) +
		" fastvm.hosts=" + base64.StdEncoding.EncodeToString([]byte(hosts))
	id, err := ParseCmdlineIdentity(cmd)
	if err != nil {
		t.Fatalf("ParseCmdlineIdentity: %v", err)
	}
	if id == nil {
		t.Fatal("nil identity")
	}
	if id.Hostname != "0123456789ab" || string(id.Resolv) != resolv || string(id.Hosts) != hosts {
		t.Fatalf("identity = %+v", id)
	}
}

func TestParseCmdlineIdentityNoKeys(t *testing.T) {
	id, err := ParseCmdlineIdentity("console=ttyS0 root=/dev/vda")
	if err != nil || id != nil {
		t.Fatalf("got %+v, %v; want nil, nil", id, err)
	}
}

func TestParseCmdlineIdentityRejectsInvalid(t *testing.T) {
	for _, cmd := range []string{
		"fastvm.hostname=-bad",
		"fastvm.hostname=has_underscore",
		"fastvm.resolv=@@@",
	} {
		if _, err := ParseCmdlineIdentity(cmd); err == nil {
			t.Errorf("expected error for %q", cmd)
		}
	}
}

func TestWriteGuestIdentityReplacesSymlink(t *testing.T) {
	etc := t.TempDir()
	target := filepath.Join(etc, "real-resolv.conf")
	if err := os.WriteFile(target, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(etc, "resolv.conf")); err != nil {
		t.Fatal(err)
	}
	id := &GuestIdentity{
		Hostname: "sandbox-1",
		Resolv:   []byte("nameserver 1.1.1.1\n"),
		Hosts:    []byte("127.0.0.1 localhost\n"),
	}
	if err := WriteGuestIdentity(etc, id); err != nil {
		t.Fatalf("WriteGuestIdentity: %v", err)
	}
	fi, err := os.Lstat(filepath.Join(etc, "resolv.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("resolv.conf is still a symlink")
	}
	for name, want := range map[string]string{
		"hostname":    "sandbox-1\n",
		"resolv.conf": "nameserver 1.1.1.1\n",
		"hosts":       "127.0.0.1 localhost\n",
	} {
		got, err := os.ReadFile(filepath.Join(etc, name))
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if strings.Contains(string(mustRead(t, target)), "nameserver 1.1.1.1") {
		t.Error("symlink target was modified")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
