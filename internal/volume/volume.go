// Package volume manages per-sandbox CoW runtime volumes cloned from base
// disks (docs/DESIGN.md §6.5).
package volume

import (
	"context"

	"fastvm/internal/types"
)

// Volume is a writable runtime disk.
type Volume struct {
	SandboxID string `json:"sandbox_id"`
	Path      string `json:"path"`
	BaseDisk  string `json:"base_disk"` // digest
	SizeBytes int64  `json:"size_bytes"`
	COW       string `json:"cow"` // reflink|copy|dm-thin
}

// Manager creates and removes runtime volumes.
type Manager interface {
	// Create clones base into a writable volume for sandboxID. clone is
	// "reflink", "dm-thin" or "copy"; empty uses the configured default.
	Create(ctx context.Context, sandboxID string, base types.BaseDisk, sizeGB int, clone string) (Volume, error)
	// Remove discards the sandbox volume (best effort if missing).
	Remove(ctx context.Context, sandboxID string) error
	// Get returns a volume if it exists.
	Get(ctx context.Context, sandboxID string) (Volume, bool, error)
}
