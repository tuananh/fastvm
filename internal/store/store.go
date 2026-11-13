// Package store persists sandbox records and events (docs/DESIGN.md §6.2).
package store

import (
	"context"

	"fastvm/internal/types"
)

// Store is the persistent record store.
type Store interface {
	PutSandbox(ctx context.Context, sb *types.Sandbox) error
	GetSandbox(ctx context.Context, id string) (*types.Sandbox, bool, error)
	ListSandboxes(ctx context.Context) ([]*types.Sandbox, error)
	DeleteSandbox(ctx context.Context, id string) error

	AppendEvent(ctx context.Context, ev types.Event) error
	ListEvents(ctx context.Context, limit int) ([]types.Event, error)

	Close() error
}
