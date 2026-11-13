//go:build linux

package agent

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// CmdlineSeedKey is the kernel-cmdline key carrying a host-generated entropy
// seed. The daemon passes fresh random bytes on every cold boot, and the agent
// credits them to the guest kernel's random pool. Without this, getrandom() in
// a fresh microVM can block until the CRNG initializes (no virtio-rng device in
// the current device model), which makes tools like coding agents hang.
const CmdlineSeedKey = "fastvm.seed"

// SeedEntropyFromCmdline parses /proc/cmdline and credits the fastvm.seed value
// to the kernel random pool.
func SeedEntropyFromCmdline() (bool, error) {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return false, fmt.Errorf("read /proc/cmdline: %w", err)
	}
	seed, err := parseSeed(string(data))
	if err != nil {
		return false, err
	}
	if len(seed) == 0 {
		return false, nil
	}
	if err := addEntropy(seed); err != nil {
		return false, err
	}
	return true, nil
}

func parseSeed(cmdline string) ([]byte, error) {
	for _, field := range strings.Fields(cmdline) {
		value, ok := strings.CutPrefix(field, CmdlineSeedKey+"=")
		if !ok {
			continue
		}
		seed, err := hex.DecodeString(value)
		if err != nil {
			return nil, fmt.Errorf("invalid %s: %w", CmdlineSeedKey, err)
		}
		return seed, nil
	}
	return nil, nil
}

// addEntropy credits seed bytes to the kernel random pool via RNDADDENTROPY.
func addEntropy(seed []byte) error {
	f, err := os.OpenFile("/dev/urandom", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open /dev/urandom: %w", err)
	}
	defer f.Close()

	// struct rand_pool_info { int entropy_count; int buf_size; __u8 buf[]; }
	buf := make([]byte, 8+len(seed))
	binary.LittleEndian.PutUint32(buf[0:4], uint32(len(seed)*8))
	binary.LittleEndian.PutUint32(buf[4:8], uint32(len(seed)))
	copy(buf[8:], seed)
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), unix.RNDADDENTROPY, uintptr(unsafe.Pointer(&buf[0])))
	if errno != 0 {
		return fmt.Errorf("RNDADDENTROPY: %w", errno)
	}
	return nil
}
