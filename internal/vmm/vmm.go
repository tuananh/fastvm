// Package vmm defines the microVM driver contract (docs/DESIGN.md §6.2).
package vmm

import (
	"context"

	"fastvm/internal/types"
)

// BootConfig describes a cold boot.
type BootConfig struct {
	SandboxID   string
	KernelPath  string
	DiskPath    string // raw writable volume
	VCPUs       int
	MemoryMB    int
	TAPName     string // empty = no NIC
	MAC         string
	GuestCID    uint32
	VsockUDPath string // host-side unix socket path for virtio-vsock
	APISocket   string // host-side unix socket for the VMM API
	SerialPath  string // serial console log file
	Cmdline     string // kernel cmdline
}

// Snapshot is a paused memory+device-state checkpoint on disk.
type Snapshot struct {
	Dir       string
	MemPath   string
	StatePath string
}

// RestoreConfig describes loading a paused snapshot into a fresh VMM process.
type RestoreConfig struct {
	SandboxID   string
	Snapshot    Snapshot
	TAPName     string
	VsockUDPath string
	APISocket   string
	SerialPath  string
	Resume      bool // false: restore paused (dormant); true: restore and run
}

// Stats aliases the shared VM counter type.
type Stats = types.VMStats

// VM is one running VMM process.
type VM interface {
	// Pause halts vCPUs, keeping memory and devices resident.
	Pause(ctx context.Context) error
	// Resume restarts vCPUs.
	Resume(ctx context.Context) error
	// Snapshot writes a paused memory+device snapshot into dir.
	Snapshot(ctx context.Context, dir string) (Snapshot, error)
	// Stop terminates the VMM process.
	Stop(ctx context.Context) error
	// Wait blocks until the VMM process exits.
	Wait(ctx context.Context) error
	// Stats returns current counters.
	Stats(ctx context.Context) (Stats, error)
	// APISocket reports the VMM API socket path.
	APISocket() string
}

// Driver launches VMM processes.
type Driver interface {
	Boot(ctx context.Context, cfg BootConfig) (VM, error)
	Restore(ctx context.Context, cfg RestoreConfig) (VM, error)
}
