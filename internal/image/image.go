// Package image builds content-addressed ext4 base disks from Dockerfiles or
// rootfs directories (docs/DESIGN.md §6.5).
package image

import (
	"context"

	"fastvm/internal/types"
)

// BuildSpec is a Dockerfile build request.
type BuildSpec struct {
	Dockerfile string // path to Dockerfile (default: <context>/Dockerfile)
	ContextDir string // build context
	Tag        string // image name/tag, e.g. "nginx:alpine"
	SizeGB     int    // base disk size; 0 = default
	KernelPath string // optional pinned kernel to record (not embedded)
	AgentPath  string // guest agent binary to inject
	InitPath   string // guest init script to inject
}

// IngestSpec imports an already-unpacked rootfs.
type IngestSpec struct {
	RootfsDir string
	Tag       string
	SizeGB    int
	AgentPath string
	InitPath  string
}

// Builder builds and stores base disks.
type Builder interface {
	// Build turns a Dockerfile into an OCI image, unpacks it, injects the guest
	// agent/init, and writes an ext4 base disk.
	Build(ctx context.Context, spec BuildSpec) (types.BaseDisk, error)
	// Ingest converts a rootfs directory into an ext4 base disk.
	Ingest(ctx context.Context, spec IngestSpec) (types.BaseDisk, error)
	// List returns all cached base disks.
	List(ctx context.Context) ([]types.BaseDisk, error)
	// Get resolves a tag or digest to a base disk.
	Get(ctx context.Context, ref string) (types.BaseDisk, error)
	// Remove deletes a base disk and all tags pointing at it.
	Remove(ctx context.Context, ref string) (types.BaseDisk, error)
}
