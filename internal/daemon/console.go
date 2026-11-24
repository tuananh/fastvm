package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"

	"fastvm/internal/manager"
)

// ConsoleLog returns the raw serial-console output captured on the host. It
// works without the guest agent, so it is useful for boot failures.
func (d *Daemon) ConsoleLog(ctx context.Context, ref string) ([]byte, error) {
	sb, err := d.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	if sb.SerialPath == "" {
		return nil, fmt.Errorf("%w: console log is unavailable", manager.ErrNotFound)
	}
	data, err := os.ReadFile(sb.SerialPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}
