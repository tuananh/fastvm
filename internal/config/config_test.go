package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultsValidate(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
	if c.Lifecycle.IdleWindow.D() != 30*time.Second {
		t.Fatalf("idle window = %v", c.Lifecycle.IdleWindow.D())
	}
	if c.Idle.CPUThresholdPct != 1.0 || c.Idle.CPUWakePct != 5.0 {
		t.Fatalf("cpu thresholds = %v/%v", c.Idle.CPUThresholdPct, c.Idle.CPUWakePct)
	}
	if c.Wake.MaxWakesPerMinute != 6 || !c.Wake.TCP {
		t.Fatalf("wake defaults = %+v", c.Wake)
	}
}

func TestLoadYAMLOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
data_dir: /tmp/fastvm-data
kernel_path: /tmp/vmlinux
lifecycle:
  idle_window: 5s
  pause_after: 30s
  offload_after: 5m
  destroy_after: 1h
idle:
  cpu_threshold_pct: 2.5
  cpu_wake_pct: 10
  sample_interval: 250ms
  warmup: 3s
  block_io_bytes: 4096
  ignore_ports: [8081]
memory:
  offload_mode: never
volume:
  cow: copy
switch:
  mode: bridge
defaults:
  vcpus: 1
  memory_mb: 128
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.DataDir != "/tmp/fastvm-data" || c.Lifecycle.IdleWindow.D() != 5*time.Second {
		t.Fatalf("bad override: %+v", c)
	}
	if c.Idle.SampleInterval.D() != 250*time.Millisecond || c.Idle.BlockIOBytes != 4096 {
		t.Fatalf("idle override = %+v", c.Idle)
	}
	if c.Memory.OffloadMode != "never" || c.Volume.COW != "copy" || c.Switch.Mode != "bridge" {
		t.Fatalf("modes = %+v", c)
	}
	if len(c.Idle.IgnorePorts) != 1 || c.Idle.IgnorePorts[0] != 8081 {
		t.Fatalf("ignore ports = %v", c.Idle.IgnorePorts)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("expected error")
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	cases := map[string]func(*Config){
		"negative duration":    func(c *Config) { c.Lifecycle.IdleWindow = Duration(-time.Second) },
		"wake below threshold": func(c *Config) { c.Idle.CPUWakePct = 0.5 },
		"bad offload mode":     func(c *Config) { c.Memory.OffloadMode = "magic" },
		"bad cow":              func(c *Config) { c.Volume.COW = "magic" },
		"bad switch":           func(c *Config) { c.Switch.Mode = "magic" },
		"bad exit policy":      func(c *Config) { c.Lifecycle.OnWorkloadExit = "explode" },
		"no data dir":          func(c *Config) { c.DataDir = "" },
		"tiny defaults":        func(c *Config) { c.Defaults.VCPUs = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := Default()
			mutate(c)
			if err := c.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestDurationParsing(t *testing.T) {
	var d Duration
	if err := d.UnmarshalYAML(mustNode(t, "1500ms")); err != nil {
		t.Fatal(err)
	}
	if d.D() != 1500*time.Millisecond {
		t.Fatalf("duration = %v", d.D())
	}
	if err := d.UnmarshalYAML(mustNode(t, "not-a-duration")); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestValidateRejectsBadNetwork(t *testing.T) {
	for _, tc := range []struct {
		name string
		net  Network
	}{
		{"loopback dns", Network{DNS: []string{"127.0.0.1"}}},
		{"bad dns", Network{DNS: []string{"not-an-ip"}}},
		{"ipv6 dns", Network{DNS: []string{"fd00::1"}}},
		{"empty search", Network{Search: []string{" "}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.Network = tc.net
			if err := cfg.Validate(); err == nil {
				t.Fatalf("expected validation error for %+v", tc.net)
			}
		})
	}
	cfg := Default()
	cfg.Network = Network{DNS: []string{"1.1.1.1"}, Search: []string{"example.com"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid network config rejected: %v", err)
	}
}
