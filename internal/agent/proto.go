// Package agent defines the guest-agent wire protocol (newline-delimited JSON
// over virtio-vsock) and the host-side client contract.
package agent

import (
	"context"
	"encoding/json"
)

// VsockPort is the guest agent's vsock port.
const VsockPort uint32 = 1024

// Op names.
const (
	OpReady     = "ready"
	OpEnv       = "env"
	OpNetwork   = "network"
	OpHostname  = "hostname"
	OpExec      = "exec"
	OpShell     = "shell"
	OpSignal    = "signal"
	OpTouch     = "touch"
	OpBusy      = "busy"
	OpStats     = "stats"
	OpPorts     = "ports"
	OpFileStat  = "file_stat"
	OpFileRead  = "file_read"
	OpFileWrite = "file_write"
	OpWorkload  = "workload" // guest -> host event
	OpHeartbeat = "heartbeat"
)

// Event type names pushed guest -> host. They are carried in Event.Type.
const (
	EventReady           = "ready"
	EventWorkloadStarted = "workload_started"
	EventWorkloadExit    = "workload_exit"
	EventWorkloadBusy    = "workload_busy"
	EventTouch           = "touch"
)

// Request is sent host -> guest.
type Request struct {
	ID        string            `json:"id"`
	Op        string            `json:"op"`
	Argv      []string          `json:"argv,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Stdin     string            `json:"stdin,omitempty"`
	Dir       string            `json:"dir,omitempty"`
	Signal    string            `json:"signal,omitempty"`
	TimeoutMS int               `json:"timeout_ms,omitempty"`
	// Detach starts Argv as the sandbox workload in the background and returns
	// immediately; workload_started/workload_exit events follow on Events.
	Detach bool `json:"detach,omitempty"`
	// Busy is the advisory workload-busy hint carried by OpBusy.
	Busy bool `json:"busy,omitempty"`
	// Rows/Cols are the initial PTY size for OpShell.
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
	// Network configures the guest NIC after a snapshot restore (new sandbox
	// network identity). Address is CIDR, e.g. 10.77.0.6/30.
	Network *NetworkConfig `json:"network,omitempty"`
	// Hostname renames the guest after a snapshot restore.
	Hostname string `json:"hostname,omitempty"`
	// File transfer fields (OpFileStat/OpFileRead/OpFileWrite).
	Path     string `json:"path,omitempty"`
	Offset   int64  `json:"offset,omitempty"`
	Mode     uint32 `json:"mode,omitempty"`
	Data     []byte `json:"data,omitempty"`
	MaxBytes int    `json:"max_bytes,omitempty"`
}

// NetworkConfig is the post-restore guest NIC configuration.
type NetworkConfig struct {
	Address string `json:"address"`
	Gateway string `json:"gateway,omitempty"`
}

// Response is sent guest -> host for a Request.
type Response struct {
	ID       string `json:"id"`
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	Ports    []int  `json:"ports,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
	FileMode uint32 `json:"file_mode,omitempty"`
	Data     []byte `json:"data,omitempty"`
	Error    string `json:"error,omitempty"`

	// Guest memory stats (OpStats) in bytes.
	MemoryTotalBytes     uint64 `json:"memory_total_bytes,omitempty"`
	MemoryAvailableBytes uint64 `json:"memory_available_bytes,omitempty"`
	MemoryUsedBytes      uint64 `json:"memory_used_bytes,omitempty"`
}

// GuestStats is a guest-internal memory snapshot.
type GuestStats struct {
	MemoryTotalBytes     uint64
	MemoryAvailableBytes uint64
	MemoryUsedBytes      uint64
}

// Event is pushed guest -> host asynchronously.
type Event struct {
	Type    string `json:"type"` // ready, workload_started, workload_exit, workload_busy, touch
	Message string `json:"message,omitempty"`
	Code    int    `json:"code,omitempty"`
}

// Client is the host-side agent connection.
type Client interface {
	// WaitReady blocks until the guest agent reports ready.
	WaitReady(ctx context.Context) error
	// Exec runs a command in the guest.
	Exec(ctx context.Context, req Request) (Response, error)
	// SetEnv sets the sandbox base environment merged into every guest process.
	SetEnv(ctx context.Context, env map[string]string) error
	// Ports lists non-loopback TCP listening ports in the guest.
	Ports(ctx context.Context) ([]int, error)
	// FileStat returns the size and mode of a guest file.
	FileStat(ctx context.Context, path string) (size int64, mode uint32, err error)
	// FileRead reads up to maxBytes from a guest file at offset.
	FileRead(ctx context.Context, path string, offset int64, maxBytes int) (data []byte, size int64, mode uint32, err error)
	// FileWrite writes data at offset, creating parent directories on first write.
	FileWrite(ctx context.Context, path string, offset int64, mode uint32, data []byte) error
	// SetNetwork reconfigures the guest NIC after a snapshot restore.
	SetNetwork(ctx context.Context, address, gateway string) error
	// SetHostname renames the guest after a snapshot restore.
	SetHostname(ctx context.Context, hostname string) error
	// Stats returns guest-internal memory usage.
	Stats(ctx context.Context) (GuestStats, error)
	// Touch reports explicit guest activity.
	Touch(ctx context.Context) error
	// Events streams agent events until the channel closes.
	Events() <-chan Event
	// Close closes the connection.
	Close() error
}

// WriteLine encodes v as one JSON line.
func WriteLine(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
