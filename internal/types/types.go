// Package types holds shared domain types for fastvm.
package types

import "time"

// State is a sandbox lifecycle state (docs/DESIGN.md §5.1).

// MaxUserDataBytes bounds cloud-init-style user_data scripts accepted by the
// daemon and the .fvmfile loader.
const MaxUserDataBytes = 1 << 20

// SnapshotInfo is one saved snapshot under the daemon data directory.
type SnapshotInfo struct {
	Name           string    `json:"name"`
	CreatedAt      time.Time `json:"created_at"`
	ImageDigest    string    `json:"image_digest"`
	VCPUs          int       `json:"vcpus"`
	MemoryMB       int       `json:"memory_mb"`
	NetworkEnabled bool      `json:"network_enabled"`
}

type State string

const (
	StateCreating State = "Creating"
	StateCreated  State = "Created"
	StateStarting State = "Starting"
	StateRunning  State = "Running"
	StateIdle     State = "Idle"
	StatePaused   State = "Paused"
	StateDormant  State = "Dormant"
	StateResuming State = "Resuming"
	StateStopping State = "Stopping"
	StateStopped  State = "Stopped"
	StateFailed   State = "Failed"
)

// Sleeping reports whether the sandbox is in a state that only the wake path
// (or an API call) can leave.
func (s State) Sleeping() bool { return s == StatePaused || s == StateDormant }

// PortMapping maps a host port to a guest port.
type PortMapping struct {
	HostPort     int    `json:"host_port"`
	GuestPort    int    `json:"guest_port"`
	Proto        string `json:"proto"` // "tcp" or "udp"
	LoopbackOnly bool   `json:"loopback_only,omitempty"`
}

// BaseDisk is a read-only, content-addressed ext4 image built from an OCI
// image or a rootfs (docs/DESIGN.md §6.5).
type BaseDisk struct {
	Name      string    `json:"name"`
	Tag       string    `json:"tag,omitempty"`
	Tags      []string  `json:"tags,omitempty"` // current tag aliases, from the tag index
	Digest    string    `json:"digest"`
	Path      string    `json:"path"`
	SizeBytes int64     `json:"size_bytes"`
	Source    string    `json:"source"` // "registry" or "rootfs"
	CreatedAt time.Time `json:"created_at"`
}

// Sandbox is the persisted record for one microVM.
type Sandbox struct {
	ID             string        `json:"id"`
	Name           string        `json:"name"`
	BaseDisk       string        `json:"base_disk"`             // digest
	Kernel         string        `json:"kernel,omitempty"`      // selected release
	KernelPath     string        `json:"kernel_path,omitempty"` // resolved host path
	VolumePath     string        `json:"volume_path"`
	VCPUs          int           `json:"vcpus"`
	MaxVCPUs       int           `json:"max_vcpus,omitempty"`
	MemoryMB       int           `json:"memory_mb"`
	MaxMemoryMB    int           `json:"max_memory_mb,omitempty"`
	State          State         `json:"state"`
	Workdir        string        `json:"workdir,omitempty"`
	AutoPublish    bool          `json:"auto_publish,omitempty"`
	NoAutoSleep    bool          `json:"no_auto_sleep"`
	NetworkEnabled bool          `json:"network_enabled"`
	AllowHosts     []string      `json:"allow_hosts,omitempty"`
	Ports          []PortMapping `json:"ports,omitempty"`
	Volumes        []Volume      `json:"volumes,omitempty"`
	DataVolumes    []DataVolume  `json:"data_volumes,omitempty"`

	// Network allocation.
	NetnsPath string `json:"netns_path,omitempty"`
	VethHost  string `json:"veth_host,omitempty"`
	VethIn    string `json:"veth_in,omitempty"`
	TAPName   string `json:"tap_name,omitempty"`
	MAC       string `json:"mac,omitempty"`
	IP        string `json:"ip,omitempty"`
	Gateway   string `json:"gateway,omitempty"`

	// Runtime bookkeeping.
	VMMAPISock       string    `json:"vmm_api_sock,omitempty"`
	VsockUDPath      string    `json:"vsock_ud_path,omitempty"`
	SerialPath       string    `json:"serial_path,omitempty"`
	SnapshotDir      string    `json:"snapshot_dir,omitempty"`
	GuestCID         uint32    `json:"guest_cid,omitempty"`
	WakeCount        int       `json:"wake_count"`
	SleepCount       int       `json:"sleep_count,omitempty"`
	ExecCount        int       `json:"exec_count,omitempty"`
	LastActivityAt   time.Time `json:"last_activity_at"`
	WorkloadFinished bool      `json:"workload_finished,omitempty"`
	Error            string    `json:"error,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Volume is a host directory shared into the sandbox over virtio-fs.
type Volume struct {
	HostPath  string `json:"host_path"`
	GuestPath string `json:"guest_path"`
	ReadOnly  bool   `json:"read_only,omitempty"`
}

// DataVolume is a writable block volume cloned from a blank ext4 template.
type DataVolume struct {
	Name   string `json:"name,omitempty"`
	SizeGB int    `json:"size_gb"`
	Mount  string `json:"mount"`
	Path   string `json:"path,omitempty"`
}

// Event is an append-only lifecycle/audit record.
type Event struct {
	Time        time.Time `json:"time"`
	SandboxID   string    `json:"sandbox_id"`
	SandboxName string    `json:"sandbox_name,omitempty"` // captured at append time
	Type        string    `json:"type"`                   // created, state, wake, sleep, error, ...
	Message     string    `json:"message"`
}

// NetStats are per-sandbox counters shared by the wake/idle logic.
type NetStats struct {
	RxBytes   uint64 `json:"rx_bytes"`
	TxBytes   uint64 `json:"tx_bytes"`
	RxPackets uint64 `json:"rx_packets"`
	TxPackets uint64 `json:"tx_packets"`
	Drops     uint64 `json:"drops"`
}

// VMStats are counters read from the VMM.
type VMStats struct {
	VCPUTimeNS     uint64 `json:"vcpu_time_ns"`
	BlockReadByte  uint64 `json:"block_read_bytes"`
	BlockWriteByte uint64 `json:"block_write_bytes"`
	RxBytes        uint64 `json:"rx_bytes"`
	TxBytes        uint64 `json:"tx_bytes"`
	// MemoryRSSBytes is the current resident set size of the VMM process,
	// sampled from /proc/<pid>/status VmRSS.
	MemoryRSSBytes uint64 `json:"memory_rss_bytes,omitempty"`
}

// SandboxStats is a point-in-time utilization snapshot for one sandbox.
type SandboxStats struct {
	SandboxID      string  `json:"sandbox_id"`
	Name           string  `json:"name"`
	State          State   `json:"state"`
	CPUPercent     float64 `json:"cpu_percent"`
	VCPUs          int     `json:"vcpus"`
	MemoryMB       int     `json:"memory_mb"`
	MemoryRSSBytes uint64  `json:"memory_rss_bytes"`
	// GuestMemoryUsedBytes is guest-internal memory usage from the agent.
	GuestMemoryUsedBytes      uint64    `json:"guest_memory_used_bytes,omitempty"`
	GuestMemoryTotalBytes     uint64    `json:"guest_memory_total_bytes,omitempty"`
	GuestMemoryBalloonedBytes uint64    `json:"guest_memory_ballooned_bytes,omitempty"`
	UptimeSeconds             int64     `json:"uptime_seconds"`
	WakeCount                 int       `json:"wake_count"`
	BlockReadBytes            uint64    `json:"block_read_bytes"`
	BlockWriteBytes           uint64    `json:"block_write_bytes"`
	RxBytes                   uint64    `json:"rx_bytes"`
	TxBytes                   uint64    `json:"tx_bytes"`
	CapturedAt                time.Time `json:"captured_at"`
}

// Now is a test seam for time.
var Now = time.Now

// Identity returns the most specific user-facing name for a base disk.
func (d BaseDisk) Identity() string {
	if d.Tag != "" {
		return d.Tag
	}
	if d.Name != "" {
		return d.Name
	}
	return d.Digest
}
