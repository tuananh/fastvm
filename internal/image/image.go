// Package image builds content-addressed ext4 base disks from OCI images or
// rootfs directories (docs/DESIGN.md §6.5).
package image

import (
	"context"

	"fastvm/internal/types"
)

// PullSpec is an OCI image pull request.
type PullSpec struct {
	Ref       string // image reference, e.g. "nginx:alpine"
	SizeGB    int    // base disk size; 0 = default
	AgentPath string // optional legacy agent injection; the daemon uses initramfs instead
	InitPath  string // optional custom guest init to inject
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
	// Pull downloads an OCI image, unpacks it, creates a base disk,
	// and writes an ext4 base disk.
	Pull(ctx context.Context, spec PullSpec) (types.BaseDisk, error)
	// Ingest converts a rootfs directory into an ext4 base disk.
	Ingest(ctx context.Context, spec IngestSpec) (types.BaseDisk, error)
	// List returns all cached base disks.
	List(ctx context.Context) ([]types.BaseDisk, error)
	// Get resolves a tag or digest to a base disk.
	Get(ctx context.Context, ref string) (types.BaseDisk, error)
	// Remove deletes a base disk and all tags pointing at it.
	Remove(ctx context.Context, ref string) (types.BaseDisk, error)
}
