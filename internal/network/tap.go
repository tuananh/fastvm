package network

import (
	"fmt"
	"regexp"

	"golang.org/x/sys/unix"
)

const tunDevice = "/dev/net/tun"

var ifaceNameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,15}$`)

// validateIfaceName rejects names that Linux would not accept (IFNAMSIZ is 16)
// or that could be misinterpreted by rule renderers.
func validateIfaceName(name string) error {
	if !ifaceNameRE.MatchString(name) {
		return fmt.Errorf("network: invalid interface name %q (1-15 chars of [A-Za-z0-9_.-])", name)
	}
	return nil
}

// createPersistentTAP creates a TAP device in the current network namespace and
// marks it persistent (TUNSETPERSIST) so the daemon can close the fd and the
// VMM can open /dev/net/tun by name later. It is idempotent when the TAP
// already exists: reopening a persistent TAP succeeds.
//
// Requires CAP_NET_ADMIN in the current network namespace.
func createPersistentTAP(name string) error {
	if err := validateIfaceName(name); err != nil {
		return err
	}
	fd, err := unix.Open(tunDevice, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("network: open %s: %w", tunDevice, err)
	}
	defer unix.Close(fd)

	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return fmt.Errorf("network: build TAP ifreq %q: %w", name, err)
	}
	ifr.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		return fmt.Errorf("network: create TAP %q: %w (needs CAP_NET_ADMIN in the daemon network namespace)", name, err)
	}
	if err := unix.IoctlSetInt(fd, unix.TUNSETPERSIST, 1); err != nil {
		return fmt.Errorf("network: persist TAP %q: %w", name, err)
	}
	return nil
}
