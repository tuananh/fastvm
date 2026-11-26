package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"fastvm/internal/manager"
	"fastvm/internal/types"
)

func TestCreateWithDataVolume(t *testing.T) {
	if _, err := exec.LookPath("mke2fs"); err != nil {
		t.Skip("mke2fs not installed")
	}
	h := newHarness(t)
	sb, err := h.d.Create(context.Background(), manager.CreateOptions{
		Name: "data-vm", Image: "img:test", Cmd: []string{"sleep", "1000"},
		DataVolumes: []types.DataVolume{{SizeGB: 1, Mount: "/data"}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(sb.DataVolumes) != 1 || sb.DataVolumes[0].Path == "" {
		t.Fatalf("data volumes = %+v", sb.DataVolumes)
	}
	info, err := os.Stat(sb.DataVolumes[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 1<<30 {
		t.Fatalf("size = %d", info.Size())
	}
	h.drv.mu.Lock()
	if len(h.drv.boots) != 1 {
		h.drv.mu.Unlock()
		t.Fatalf("boots = %d", len(h.drv.boots))
	}
	cfg := h.drv.boots[0]
	h.drv.mu.Unlock()
	if len(cfg.DataDisks) != 1 || cfg.DataDisks[0] != sb.DataVolumes[0].Path {
		t.Fatalf("data disks = %+v", cfg.DataDisks)
	}
	if !strings.Contains(cfg.Cmdline, "fastvm.data.0=/data") {
		t.Fatalf("cmdline = %q", cfg.Cmdline)
	}
	if !sb.NoAutoSleep {
		t.Fatal("data volumes should disable auto-sleep")
	}
	if _, err := h.d.Snapshot(context.Background(), sb.ID, "snap"); !errors.Is(err, manager.ErrConflict) {
		t.Fatalf("snapshot error = %v, want conflict", err)
	}
	if err := h.d.Delete(context.Background(), sb.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sb.DataVolumes[0].Path); !os.IsNotExist(err) {
		t.Fatalf("data volume still exists: %v", err)
	}
}

func TestValidateDataVolumes(t *testing.T) {
	cases := []struct {
		name string
		vol  types.DataVolume
		ok   bool
	}{
		{"ok", types.DataVolume{SizeGB: 8, Mount: "/data"}, true},
		{"root", types.DataVolume{SizeGB: 8, Mount: "/"}, false},
		{"relative", types.DataVolume{SizeGB: 8, Mount: "data"}, false},
		{"zero", types.DataVolume{SizeGB: 0, Mount: "/data"}, false},
		{"space", types.DataVolume{SizeGB: 8, Mount: "/my data"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDataVolumes([]types.DataVolume{tc.vol})
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v, ok = %v", err, tc.ok)
			}
		})
	}
}

func TestPullImageUsesStorageSize(t *testing.T) {
	h := newHarness(t)
	h.d.cfg.Storage.DefaultImageSizeGB = 2
	if _, err := h.d.PullImage(context.Background(), manager.PullOptions{Ref: "alpine"}); err != nil {
		t.Fatal(err)
	}
	if len(h.imgs.pulls) != 1 || h.imgs.pulls[0].SizeGB != 2 {
		t.Fatalf("pulls = %+v", h.imgs.pulls)
	}
}
