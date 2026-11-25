//go:build linux

package agent

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// guestMemoryStats reads guest-visible memory pressure from /proc/meminfo.
func guestMemoryStats() (GuestStats, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return GuestStats{}, fmt.Errorf("read /proc/meminfo: %w", err)
	}
	return parseMeminfo(string(data))
}

// parseMeminfo computes memory the same way `free` reports "used":
//
//	used = MemTotal - MemFree - Buffers - Cached - SReclaimable
//
// MemAvailable is deliberately not used for "used": it includes reclaimable
// page cache, which makes the number look larger than `free`'s used column.
func parseMeminfo(data string) (GuestStats, error) {
	values := make(map[string]uint64)
	for _, line := range strings.Split(data, "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		n, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		values[key] = n * 1024
	}
	total := values["MemTotal"]
	if total == 0 {
		return GuestStats{}, errors.New("/proc/meminfo missing MemTotal")
	}
	free := values["MemFree"]
	cache := values["Buffers"] + values["Cached"] + values["SReclaimable"]
	used := total
	if free+cache < used {
		used -= free + cache
	} else {
		used = 0
	}
	available := values["MemAvailable"]
	if available == 0 {
		available = free + values["Buffers"] + values["Cached"]
	}
	if available > total {
		available = total
	}
	return GuestStats{
		MemoryTotalBytes:     total,
		MemoryAvailableBytes: available,
		MemoryUsedBytes:      used,
		MemoryBalloonedBytes: values["Balloon"],
	}, nil
}
