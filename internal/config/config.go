// Package config loads and validates the fastvm daemon configuration
// (docs/DESIGN.md §5.4 defaults).
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
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
	Network    Network `yaml:"network"`
	API        API     `yaml:"api"`

	Lifecycle Lifecycle `yaml:"lifecycle"`
	Memory    Memory    `yaml:"memory"`
	Storage   Storage   `yaml:"storage"`
	Defaults  Defaults  `yaml:"defaults"`

	// VMM selects the VMM executable(s). Empty values use the built-in defaults.
	VMM VMMConfig `yaml:"vmm"`
	// HostVolumes configures host-directory virtio-fs mounts.
	HostVolumes HostVolumes `yaml:"host_volumes"`
}

// Lifecycle configures state transitions and the idle/wake policies that drive
// them.
type Lifecycle struct {
	IdleWindow     Duration      `yaml:"idle_window"`
	PauseAfter     Duration      `yaml:"pause_after"`
	OffloadAfter   Duration      `yaml:"offload_after"`
	DestroyAfter   Duration      `yaml:"destroy_after"`
	OnWorkloadExit string        `yaml:"on_workload_exit"` // idle|pause|offload|stop
	IdleDetection  IdleDetection `yaml:"idle_detection"`
	WakePolicy     WakePolicy    `yaml:"wake_policy"`
}

// IdleDetection configures the debounced idle predicate (docs/DESIGN.md §5.3).
type IdleDetection struct {
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

// API configures the optional TCP REST transport. The unix socket is always
// available; TCP requires mTLS.
type API struct {
	Host     string `yaml:"host"`      // host:port; empty disables TCP
	CertPath string `yaml:"cert_path"` // dir with ca.pem, cert.pem, key.pem
}

// WakePolicy configures wake-on-network policy.
type WakePolicy struct {
	TCP               bool     `yaml:"tcp"`
	UDP               bool     `yaml:"udp"`
	ICMP              bool     `yaml:"icmp"`
	AllowedPorts      []int    `yaml:"allowed_ports"`
	MaxWakesPerMinute int      `yaml:"max_wakes_per_minute"`
	CoalesceWindow    Duration `yaml:"coalesce_window"`
}

// Storage configures sandbox writable storage.
type Storage struct {
	COW              string `yaml:"cow"` // reflink|dm-thin|copy
	RootSizeGB       int    `yaml:"root_size_gb"`
	DiscardOnDestroy bool   `yaml:"discard_on_destroy"`
}

// VMMConfig selects the VMM backend executables.
type VMMConfig struct {
	Binary    string `yaml:"binary"`    // cloud-hypervisor executable
	Virtiofsd string `yaml:"virtiofsd"` // virtiofsd executable for volumes
}

// HostVolumes restricts which host directories may be mounted as volumes.
// An empty AllowedPrefixes denies all host-directory volumes.
type HostVolumes struct {
	AllowedPrefixes []string `yaml:"allowed_prefixes"`
}

// Network configures guest-visible networking and the host datapath. Empty
// DNS/Search means the daemon mirrors the host's /etc/resolv.conf, like Docker.
type Network struct {
	DNS      []string `yaml:"dns"`
	Search   []string `yaml:"search"`
	DataPath DataPath `yaml:"datapath"`
}

// DataPath configures the L2 forwarding datapath backend.
type DataPath struct {
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
			IdleDetection: IdleDetection{
				CPUThresholdPct: 1.0,
				CPUWakePct:      5.0,
				SampleInterval:  Duration(time.Second),
				ExitHold:        Duration(3 * time.Second),
				Warmup:          Duration(10 * time.Second),
				BlockIOBytes:    65536,
				KeepaliveAware:  true,
				IgnorePorts:     nil,
			},
			WakePolicy: WakePolicy{
				TCP:               true,
				UDP:               true,
				ICMP:              true,
				MaxWakesPerMinute: 6,
				CoalesceWindow:    Duration(100 * time.Millisecond),
			},
		},
		Network:  Network{DataPath: DataPath{Mode: "ebpf", IsolateSandboxes: true, WakeNotify: true}},
		Memory:   Memory{OffloadMode: "file", EvictAbove: 0.90, ReserveMB: 1024},
		Storage:  Storage{COW: "reflink", RootSizeGB: 8, DiscardOnDestroy: true},
		VMM:      VMMConfig{Binary: "cloud-hypervisor", Virtiofsd: "virtiofsd"},
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
	if c.Lifecycle.IdleDetection.CPUThresholdPct < 0 || c.Lifecycle.IdleDetection.CPUThresholdPct > 100 {
		errs = append(errs, "idle.cpu_threshold_pct must be in [0,100]")
	}
	if c.Lifecycle.IdleDetection.CPUWakePct < c.Lifecycle.IdleDetection.CPUThresholdPct {
		errs = append(errs, "idle.cpu_wake_pct must be >= cpu_threshold_pct")
	}
	switch c.Memory.OffloadMode {
	case "", "file", "uffd", "swap", "never":
	default:
		errs = append(errs, "memory.offload_mode must be file|uffd|swap|never")
	}
	switch c.Storage.COW {
	case "", "reflink", "dm-thin", "copy":
	default:
		errs = append(errs, "storage.cow must be reflink|dm-thin|copy")
	}
	switch c.Network.DataPath.Mode {
	case "", "ebpf", "bridge":
	default:
		errs = append(errs, "network.datapath.mode must be ebpf|bridge")
	}
	for _, prefix := range c.HostVolumes.AllowedPrefixes {
		if !filepath.IsAbs(prefix) || filepath.Clean(prefix) != prefix {
			errs = append(errs, "host_volumes.allowed_prefixes entries must be absolute clean paths")
			break
		}
	}
	for _, dns := range c.Network.DNS {
		if ip := net.ParseIP(strings.TrimSpace(dns)); ip == nil || ip.To4() == nil || ip.IsLoopback() {
			errs = append(errs, "network.dns entries must be non-loopback IPv4 addresses")
			break
		}
	}
	if c.API.Host != "" {
		if _, _, err := net.SplitHostPort(c.API.Host); err != nil {
			errs = append(errs, "api.host must be host:port")
		}
		if strings.TrimSpace(c.API.CertPath) == "" {
			errs = append(errs, "api.host requires api.cert_path")
		}
	} else if c.API.CertPath != "" {
		errs = append(errs, "api.cert_path requires api.host")
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
