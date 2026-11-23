package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"fastvm/internal/config"
	"fastvm/internal/types"
)

func TestValidateVolumes(t *testing.T) {
	share := t.TempDir()
	other := t.TempDir()
	cfg := config.Default()
	cfg.HostVolumes.AllowedPrefixes = []string{share}

	if err := os.MkdirAll(filepath.Join(share, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	valid := types.Volume{HostPath: filepath.Join(share, "data"), GuestPath: "/mnt/data"}
	if err := validateVolumes(cfg, []types.Volume{valid}); err != nil {
		t.Fatalf("valid volume rejected: %v", err)
	}
	if err := validateVolumes(cfg, nil); err != nil {
		t.Fatalf("empty volumes rejected: %v", err)
	}

	cases := []struct {
		name    string
		volumes []types.Volume
	}{
		{"outside allowlist", []types.Volume{{HostPath: filepath.Join(other, "data"), GuestPath: "/mnt"}}},
		{"relative host", []types.Volume{{HostPath: "relative/path", GuestPath: "/mnt"}}},
		{"unclean host", []types.Volume{{HostPath: share + "/a/../b", GuestPath: "/mnt"}}},
		{"relative guest", []types.Volume{{HostPath: share, GuestPath: "mnt"}}},
		{"guest root", []types.Volume{{HostPath: share, GuestPath: "/"}}},
		{"guest colon", []types.Volume{{HostPath: share, GuestPath: "/mnt:a"}}},
		{"guest comma", []types.Volume{{HostPath: share, GuestPath: "/mnt,a"}}},
		{"duplicate guest", []types.Volume{
			{HostPath: share, GuestPath: "/mnt/a"},
			{HostPath: share, GuestPath: "/mnt/a"},
		}},
		{"missing host dir", []types.Volume{{HostPath: filepath.Join(share, "does-not-exist"), GuestPath: "/mnt"}}},
	}
	for _, tc := range cases {
		if err := validateVolumes(cfg, tc.volumes); err == nil {
			t.Errorf("%s: expected an error", tc.name)
		}
	}
}
