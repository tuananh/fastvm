package main

import (
	"os"
	"testing"

	"fastvm/internal/types"
)

func TestParseVolumeFlags(t *testing.T) {
	hosts, data, err := parseVolumeFlags([]string{
		"size=8GiB,mount=/data",
		"size=2GiB,mount=/scratch",
		"/tmp/project:/workspace:ro",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 || hosts[0].GuestPath != "/workspace" || !hosts[0].ReadOnly {
		t.Fatalf("hosts = %+v", hosts)
	}
	if len(data) != 2 || data[0].SizeGB != 8 || data[1].Mount != "/scratch" {
		t.Fatalf("data = %+v", data)
	}
}

func TestParseDataVolumeSpec(t *testing.T) {
	dv, err := parseDataVolumeSpec("size=8GiB")
	if err != nil {
		t.Fatal(err)
	}
	if dv.SizeGB != 8 || dv.Mount != "/data" {
		t.Fatalf("volume = %+v", dv)
	}
	for _, bad := range []string{"mount=/data", "size=8GiB,mount=relative", "size=0"} {
		if _, err := parseDataVolumeSpec(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

func TestMergeDataVolumes(t *testing.T) {
	got := mergeDataVolumes(
		[]types.DataVolume{{SizeGB: 8, Mount: "/data"}},
		[]types.DataVolume{{SizeGB: 2, Mount: "/scratch"}, {SizeGB: 16, Mount: "/data"}},
	)
	if len(got) != 2 || got[0].SizeGB != 16 || got[1].Mount != "/scratch" {
		t.Fatalf("merged = %+v", got)
	}
}

func TestLoadFVMFileVolumes(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/vol.fvmfile"
	data := `version: 1
name: vol
image: alpine
volumes:
  - size: 8GiB
    mount: /data
  - size: 2GiB
    mount: /scratch
    name: scratch
  - host: ./project
    guest: /workspace
    read_only: true
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	spec, err := LoadFVMFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.DataVolumes) != 2 || spec.DataVolumes[0].SizeGB != 8 || spec.DataVolumes[1].Mount != "/scratch" {
		t.Fatalf("data volumes = %+v", spec.DataVolumes)
	}
	if len(spec.Volumes) != 1 || !spec.Volumes[0].ReadOnly {
		t.Fatalf("host volumes = %+v", spec.Volumes)
	}
}
