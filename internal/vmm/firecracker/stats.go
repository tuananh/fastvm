package firecracker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"fastvm/internal/vmm"
)

// userHZ is Linux's USER_HZ, the unit of /proc/<pid>/stat utime/stime. It is
// 100 on all architectures fastvm targets (x86_64, arm64); it is not
// configurable at runtime from userspace without cgo.
const userHZ = 100

// readVMStats reads host-side counters for one VMM process.
//
// Limitations (documented by design, docs/DESIGN.md §5.3):
//   - CPU time is the sum of utime+stime across all VMM threads, including
//     device/API threads, not per-vCPU guest time. It is normalized by vCPU
//     capacity by the caller, so it is a faithful "is the sandbox busy" signal.
//   - Block I/O is /proc/<pid>/io read_bytes/write_bytes, which count bytes
//     fetched from storage, not page-cache hits.
//   - Network counters live in the switch, not the VMM, and stay zero here.
func readVMStats(pid int) (vmm.Stats, error) {
	var st vmm.Stats
	ticks, err := vmmCPUTicks(pid)
	if err != nil {
		return st, err
	}
	st.VCPUTimeNS = ticks * (1_000_000_000 / userHZ)
	read, write, err := vmmBlockIO(pid)
	if err != nil {
		return st, err
	}
	st.BlockReadByte = read
	st.BlockWriteByte = write
	return st, nil
}

// vmmCPUTicks sums utime+stime over every thread of pid.
func vmmCPUTicks(pid int) (uint64, error) {
	dir := fmt.Sprintf("/proc/%d/task", pid)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("firecracker: stats: read %s: %w", dir, err)
	}
	var total uint64
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name(), "stat"))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // thread exited between ReadDir and ReadFile
			}
			return 0, fmt.Errorf("firecracker: stats: read task stat: %w", err)
		}
		utime, stime, err := parseProcStat(string(data))
		if err != nil {
			return 0, fmt.Errorf("firecracker: stats: %w", err)
		}
		total += utime + stime
	}
	return total, nil
}

// parseProcStat extracts utime and stime from /proc/<pid>/stat, skipping the
// comm field which may contain spaces and parentheses.
func parseProcStat(s string) (utime, stime uint64, err error) {
	end := strings.LastIndexByte(s, ')')
	if end < 0 || end+2 > len(s) {
		return 0, 0, errors.New("malformed /proc stat line")
	}
	fields := strings.Fields(s[end+2:])
	// fields[0] is state (field 3); utime is field 14 and stime field 15.
	if len(fields) < 13 {
		return 0, 0, errors.New("malformed /proc stat line: too few fields")
	}
	utime, err = strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse utime: %w", err)
	}
	stime, err = strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse stime: %w", err)
	}
	return utime, stime, nil
}

// vmmBlockIO reads read_bytes/write_bytes from /proc/<pid>/io.
func vmmBlockIO(pid int) (read, write uint64, err error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/io", pid))
	if err != nil {
		return 0, 0, fmt.Errorf("firecracker: stats: read /proc/%d/io: %w", pid, err)
	}
	var gotRead, gotWrite bool
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v, perr := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
		if perr != nil {
			continue
		}
		switch strings.TrimSpace(key) {
		case "read_bytes":
			read, gotRead = v, true
		case "write_bytes":
			write, gotWrite = v, true
		}
	}
	if !gotRead || !gotWrite {
		return 0, 0, fmt.Errorf("firecracker: stats: /proc/%d/io missing read_bytes/write_bytes", pid)
	}
	return read, write, nil
}
