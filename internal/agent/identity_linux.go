//go:build linux

package agent

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Kernel-cmdline keys carrying per-boot files that Docker would normally
// bind-mount into a container (/etc/hostname, /etc/resolv.conf, /etc/hosts).
// Values other than the hostname are base64 to survive command-line splitting.
const (
	CmdlineHostnameKey = "fastvm.hostname"
	CmdlineResolvKey   = "fastvm.resolv"
	CmdlineHostsKey    = "fastvm.hosts"

	maxGuestFileBytes = 64 << 10
)

// GuestIdentity is the per-boot identity supplied by the daemon.
type GuestIdentity struct {
	Hostname string
	Resolv   []byte
	Hosts    []byte
}

// ParseCmdlineIdentity extracts the per-boot identity files from a kernel
// command line. It returns (nil, nil) when none of the keys are present.
func ParseCmdlineIdentity(cmdline string) (*GuestIdentity, error) {
	var id GuestIdentity
	found := false
	for _, field := range strings.Fields(cmdline) {
		key, value, ok := strings.Cut(field, "=")
		if !ok || !strings.HasPrefix(key, "fastvm.") {
			continue
		}
		switch key {
		case CmdlineHostnameKey:
			if value == "" {
				continue
			}
			if err := validateHostname(value); err != nil {
				return nil, fmt.Errorf("kernel cmdline %s: %w", key, err)
			}
			id.Hostname = value
			found = true
		case CmdlineResolvKey:
			data, err := decodeCmdlineFile(value)
			if err != nil {
				return nil, fmt.Errorf("kernel cmdline %s: %w", key, err)
			}
			id.Resolv = data
			found = true
		case CmdlineHostsKey:
			data, err := decodeCmdlineFile(value)
			if err != nil {
				return nil, fmt.Errorf("kernel cmdline %s: %w", key, err)
			}
			id.Hosts = data
			found = true
		}
	}
	if !found {
		return nil, nil
	}
	return &id, nil
}

func decodeCmdlineFile(value string) ([]byte, error) {
	if value == "" {
		return nil, nil
	}
	if len(value) > base64.StdEncoding.EncodedLen(maxGuestFileBytes) {
		return nil, errors.New("value is too large")
	}
	data, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("invalid base64: %w", err)
	}
	if len(data) > maxGuestFileBytes {
		return nil, errors.New("decoded value is too large")
	}
	return data, nil
}

func validateHostname(name string) error {
	if len(name) == 0 || len(name) > 63 {
		return errors.New("hostname length must be 1..63")
	}
	for i, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '.'
		if !ok {
			return fmt.Errorf("invalid hostname character %q", r)
		}
		if (i == 0 || i == len(name)-1) && (r == '-' || r == '.') {
			return errors.New("hostname must start and end with a letter or digit")
		}
	}
	return nil
}

// ConfigureGuestIdentityFromCmdline writes /etc/hostname, /etc/resolv.conf and
// /etc/hosts before the workload starts. It is a no-op on older daemons/images
// that do not pass the fastvm.hostname/resolv/hosts keys.
func ConfigureGuestIdentityFromCmdline() error {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return fmt.Errorf("read /proc/cmdline: %w", err)
	}
	id, err := ParseCmdlineIdentity(string(data))
	if err != nil {
		return err
	}
	if id == nil {
		return nil
	}
	var errs []error
	// Setting /etc/hostname alone does not change the kernel hostname; Docker
	// sets the container UTS hostname too, so do the same for the sandbox.
	if id.Hostname != "" {
		if err := unix.Sethostname([]byte(id.Hostname)); err != nil {
			errs = append(errs, fmt.Errorf("set hostname: %w", err))
		}
	}
	if err := WriteGuestIdentity("/etc", id); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// WriteGuestIdentity writes the generated files under etcDir. Files are
// replaced atomically so this also works when the image ships a symlink such as
// /etc/resolv.conf -> /run/systemd/resolve/stub-resolv.conf.
func WriteGuestIdentity(etcDir string, id *GuestIdentity) error {
	if id == nil {
		return nil
	}
	var errs []error
	if id.Hostname != "" {
		if err := writeGuestFile(filepath.Join(etcDir, "hostname"), []byte(id.Hostname+"\n")); err != nil {
			errs = append(errs, err)
		}
	}
	if len(id.Resolv) > 0 {
		if err := writeGuestFile(filepath.Join(etcDir, "resolv.conf"), id.Resolv); err != nil {
			errs = append(errs, err)
		}
	}
	if len(id.Hosts) > 0 {
		if err := writeGuestFile(filepath.Join(etcDir, "hosts"), id.Hosts); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func writeGuestFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".fastvm-identity-*")
	if err != nil {
		return fmt.Errorf("create temporary file for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	cleanup = false
	return nil
}
