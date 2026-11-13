// Package config loads and validates the fastvm daemon configuration
// (docs/DESIGN.md §5.4 defaults).
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration with YAML/JSON support ("30s", "2m").
type Duration time.Duration

func (d Duration) D() time.Duration { return time.Duration(d) }
func (d Duration) String() string   { return time.Duration(d).String() }

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// Config is the daemon configuration.
type Config struct {
	DataDir    string  `yaml:"data_dir"`
	SocketPath string  `yaml:"socket_path"`
	KernelPath string  `yaml:"kernel_path"`
	AgentPath  string  `yaml:"agent_path"` // guest agent binary injected into base disks
	InitPath   string  `yaml:"init_path"`  // optional host path to init script injected into base disks
	Switch     Switch  `yaml:"switch"`
	Network    Network `yaml:"network"`

	Lifecycle Lifecycle `yaml:"lifecycle"`
	Idle      Idle      `yaml:"idle"`
	Memory    Memory    `yaml:"memory"`
	Wake      Wake      `yaml:"wake"`
	Volume    Volume    `yaml:"volume"`
	Defaults  Defaults  `yaml:"defaults"`
}

// Lifecycle configures state transitions.
type Lifecycle struct {
	IdleWindow     Duration `yaml:"idle_window"`
	PauseAfter     Duration `yaml:"pause_after"`
	OffloadAfter   Duration `yaml:"offload_after"`
	DestroyAfter   Duration `yaml:"destroy_after"`
	OnWorkloadExit string   `yaml:"on_workload_exit"` // idle|pause|offload|stop
}

// Idle configures the debounced idle predicate (docs/DESIGN.md §5.3).
type Idle struct {
	CPUThresholdPct float64  `yaml:"cpu_threshold_pct"`
	CPUWakePct      float64  `yaml:"cpu_wake_pct"`
	SampleInterval  Duration `yaml:"sample_interval"`
	ExitHold        Duration `yaml:"exit_hold"`
	Warmup          Duration `yaml:"warmup"`
	BlockIOBytes    int64    `yaml:"block_io_bytes"`
	KeepaliveAware  bool     `yaml:"keepalive_aware"`
	IgnorePorts     []int    `yaml:"ignore_ports"`
}

// Memory configures guest-RAM offload.
type Memory struct {
	OffloadMode string  `yaml:"offload_mode"` // file|uffd|swap|never
	EvictAbove  float64 `yaml:"evict_above"`
	ReserveMB   int     `yaml:"reserve_mb"`
}

// Wake configures wake-on-network policy.
type Wake struct {
	TCP               bool     `yaml:"tcp"`
	UDP               bool     `yaml:"udp"`
	ICMP              bool     `yaml:"icmp"`
	AllowedPorts      []int    `yaml:"allowed_ports"`
	MaxWakesPerMinute int      `yaml:"max_wakes_per_minute"`
	CoalesceWindow    Duration `yaml:"coalesce_window"`
}

// Volume configures CoW runtime volumes.
type Volume struct {
	COW              string `yaml:"cow"` // reflink|dm-thin|copy
	SizeGB           int    `yaml:"size_gb"`
	DiscardOnDestroy bool   `yaml:"discard_on_destroy"`
}

// Network configures guest-visible networking. Empty DNS/Search means the
// daemon mirrors the host's /etc/resolv.conf, like Docker does for containers.
type Network struct {
	DNS    []string `yaml:"dns"`
	Search []string `yaml:"search"`
}

// Switch configures the L2 switch backend.
type Switch struct {
	Mode             string `yaml:"mode"` // ebpf|bridge
	IsolateSandboxes bool   `yaml:"isolate_sandboxes"`
	WakeNotify       bool   `yaml:"wake_notify"`
}

// Defaults are per-sandbox defaults.
type Defaults struct {
	VCPUs       int  `yaml:"vcpus"`
	MemoryMB    int  `yaml:"memory_mb"`
	NoAutoSleep bool `yaml:"no_auto_sleep"`
}

// Default returns the built-in configuration from docs/DESIGN.md §5.4.
func Default() *Config {
	return &Config{
		DataDir:    "/var/lib/fastvm",
		SocketPath: "/run/fastvm/fastvm.sock",
		KernelPath: "/var/lib/fastvm/kernels/vmlinux",
		AgentPath:  "/var/lib/fastvm/agent/fastvm-agent",
		InitPath:   "",
		Lifecycle: Lifecycle{
			IdleWindow:     Duration(30 * time.Second),
			PauseAfter:     Duration(2 * time.Minute),
			OffloadAfter:   Duration(10 * time.Minute),
			DestroyAfter:   Duration(24 * time.Hour),
			OnWorkloadExit: "idle",
		},
		Idle: Idle{
			CPUThresholdPct: 1.0,
			CPUWakePct:      5.0,
			SampleInterval:  Duration(time.Second),
			ExitHold:        Duration(3 * time.Second),
			Warmup:          Duration(10 * time.Second),
			BlockIOBytes:    65536,
			KeepaliveAware:  true,
			IgnorePorts:     nil,
		},
		Memory: Memory{OffloadMode: "file", EvictAbove: 0.90, ReserveMB: 1024},
		Wake: Wake{
			TCP:               true,
			UDP:               true,
			ICMP:              true,
			MaxWakesPerMinute: 6,
			CoalesceWindow:    Duration(100 * time.Millisecond),
		},
		Volume:   Volume{COW: "reflink", SizeGB: 8, DiscardOnDestroy: true},
		Switch:   Switch{Mode: "ebpf", IsolateSandboxes: true, WakeNotify: true},
		Defaults: Defaults{VCPUs: 2, MemoryMB: 512},
	}
}

// Load reads YAML from path over the defaults. An empty path returns defaults.
func Load(path string) (*Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, cfg.Validate()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate checks semantic constraints.
func (c *Config) Validate() error {
	var errs []string
	if c.DataDir == "" {
		errs = append(errs, "data_dir must be set")
	}
	if c.Lifecycle.IdleWindow.D() < 0 || c.Lifecycle.PauseAfter.D() < 0 ||
		c.Lifecycle.OffloadAfter.D() < 0 || c.Lifecycle.DestroyAfter.D() < 0 {
		errs = append(errs, "lifecycle durations must not be negative")
	}
	switch c.Lifecycle.OnWorkloadExit {
	case "", "idle", "pause", "offload", "stop":
	default:
		errs = append(errs, "lifecycle.on_workload_exit must be idle|pause|offload|stop")
	}
	if c.Idle.CPUThresholdPct < 0 || c.Idle.CPUThresholdPct > 100 {
		errs = append(errs, "idle.cpu_threshold_pct must be in [0,100]")
	}
	if c.Idle.CPUWakePct < c.Idle.CPUThresholdPct {
		errs = append(errs, "idle.cpu_wake_pct must be >= cpu_threshold_pct")
	}
	switch c.Memory.OffloadMode {
	case "", "file", "uffd", "swap", "never":
	default:
		errs = append(errs, "memory.offload_mode must be file|uffd|swap|never")
	}
	switch c.Volume.COW {
	case "", "reflink", "dm-thin", "copy":
	default:
		errs = append(errs, "volume.cow must be reflink|dm-thin|copy")
	}
	switch c.Switch.Mode {
	case "", "ebpf", "bridge":
	default:
		errs = append(errs, "switch.mode must be ebpf|bridge")
	}
	for _, dns := range c.Network.DNS {
		if ip := net.ParseIP(strings.TrimSpace(dns)); ip == nil || ip.To4() == nil || ip.IsLoopback() {
			errs = append(errs, "network.dns entries must be non-loopback IPv4 addresses")
			break
		}
	}
	for _, domain := range c.Network.Search {
		if strings.TrimSpace(domain) == "" || strings.ContainsAny(domain, " \t#;") {
			errs = append(errs, "network.search entries must be non-empty domain names")
			break
		}
	}
	if c.Defaults.VCPUs < 1 || c.Defaults.MemoryMB < 64 {
		errs = append(errs, "defaults require vcpus>=1 and memory_mb>=64")
	}
	if len(errs) > 0 {
		return errors.New("invalid config: " + strings.Join(errs, "; "))
	}
	return nil
}
