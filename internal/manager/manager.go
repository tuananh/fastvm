// Package manager defines the runtime operations exposed to the CLI and HTTP
// API (docs/DESIGN.md §6.2). The daemon implements it.
package manager

import (
	"context"

	"fastvm/internal/agent"
	"fastvm/internal/types"
)

// CreateOptions describes a new sandbox.
type CreateOptions struct {
	Name        string
	Image       string // base disk tag or digest
	Kernel      string // optional installed kernel release for this sandbox
	VCPUs       int
	MaxVCPUs    int
	MemoryMB    int
	MaxMemoryMB int
	Ports       []types.PortMapping
	Volumes     []types.Volume
	DataVolumes []types.DataVolume // per-sandbox writable block volumes
	Cmd         []string           // workload started after agent Ready (detached)
	Env         map[string]string  // workload environment
	Workdir     string             // guest working directory for commands/shells
	AutoPublish bool               // discover and publish guest TCP listeners
	NoAutoSleep bool
	// Start boots the VM immediately (default true when nil). A false value
	// creates the sandbox in StateCreated so it can be started later.
	Start *bool
	// NetworkEnabled defaults to true when nil. False boots the guest with no
	// NIC/TAP at all.
	NetworkEnabled *bool
	AllowHosts     []string
	// FromSnapshot names a snapshot under the daemon's data directory.
	// When set, Image is ignored and the snapshot manifest supplies the base
	// image.
	FromSnapshot string
	// UserData is a shell script run once after the first boot, before Cmd.
	UserData string
}

// Runtime is the daemon-facing sandbox API.
type Runtime interface {
	Create(ctx context.Context, opts CreateOptions) (types.Sandbox, error)
	Start(ctx context.Context, ref string) error
	Snapshot(ctx context.Context, ref, name string) (string, error)
	SnapshotCopy(ctx context.Context, from, name string) (types.Sandbox, error)
	ListSnapshots(ctx context.Context) ([]types.SnapshotInfo, error)
	RemoveSnapshot(ctx context.Context, name string) error
	Get(ctx context.Context, ref string) (types.Sandbox, error)
	Stats(ctx context.Context, ref string) (types.SandboxStats, error)
	List(ctx context.Context) ([]types.Sandbox, error)
	Exec(ctx context.Context, ref string, argv []string, env map[string]string) (agent.Response, error)
	Sleep(ctx context.Context, ref string, deep bool) error
	Wake(ctx context.Context, ref string) error
	Delete(ctx context.Context, ref string) error
	Events(ctx context.Context, limit int) ([]types.Event, error)
	Close() error
}

// Images is the image-management API.
type Images interface {
	PullImage(ctx context.Context, spec PullOptions) (types.BaseDisk, error)
	ListImages(ctx context.Context) ([]types.BaseDisk, error)
	IngestRootfs(ctx context.Context, rootfsDir, tag string) (types.BaseDisk, error)
	RemoveImage(ctx context.Context, ref string, force bool) (types.BaseDisk, error)
}

// PullOptions is a CLI-facing image pull request.
type PullOptions struct {
	Ref    string
	SizeGB int
}
