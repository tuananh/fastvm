// Package types holds shared domain types for fastvm.
package types

import "time"

// State is a sandbox lifecycle state (docs/DESIGN.md §5.1).
type State string

const (
	StateCreating State = "Creating"
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
	HostPort  int    `json:"host_port"`
	GuestPort int    `json:"guest_port"`
	Proto     string `json:"proto"` // "tcp" or "udp"
}

// BaseDisk is a read-only, content-addressed ext4 image built from a Dockerfile
// or a rootfs (docs/DESIGN.md §6.5).
type BaseDisk struct {
	Name      string    `json:"name"`
	Tag       string    `json:"tag,omitempty"`
	Tags      []string  `json:"tags,omitempty"` // current tag aliases, from the tag index
	Digest    string    `json:"digest"`
	Path      string    `json:"path"`
	SizeBytes int64     `json:"size_bytes"`
	Source    string    `json:"source"` // "dockerfile" or "rootfs"
	CreatedAt time.Time `json:"created_at"`
}

// Sandbox is the persisted record for one microVM.
type Sandbox struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	BaseDisk    string        `json:"base_disk"` // digest
	VolumePath  string        `json:"volume_path"`
	VCPUs       int           `json:"vcpus"`
	MemoryMB    int           `json:"memory_mb"`
	State       State         `json:"state"`
	NoAutoSleep bool          `json:"no_auto_sleep"`
	Ports       []PortMapping `json:"ports,omitempty"`

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
	LastActivityAt   time.Time `json:"last_activity_at"`
	WorkloadFinished bool      `json:"workload_finished,omitempty"`
	Error            string    `json:"error,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
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
