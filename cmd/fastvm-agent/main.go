// Command fastvm-agent is the static guest agent injected into fastvm base images.
// It runs as PID 1 inside the microVM, serves the newline-delimited JSON
// protocol over virtio-vsock (see internal/agent), and owns the sandbox
// workload (docs/DESIGN.md §6.2, §6.4).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mdlayher/vsock"

	"fastvm/internal/agent"
)

// vsockListenTimeout bounds waiting for /dev/vsock to appear. The virtio-vsock
// device may still be probing when PID 1 starts.
const vsockListenTimeout = 5 * time.Second

const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// ensurePATH makes bare-command exec work even when the injected init script
// did not export PATH (older base disks). exec.LookPath uses the agent's own
// environment, so this must be set before any exec.Command call.
func ensurePATH() {
	if os.Getenv("PATH") == "" {
		_ = os.Setenv("PATH", defaultPath)
	}
}

func main() {
	ensurePATH()
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	// Credit the host-provided seed before anything can call getrandom(); a
	// fresh microVM's CRNG otherwise may not be initialized yet.
	if seeded, err := agent.SeedEntropyFromCmdline(); err != nil {
		log.Printf("fastvm-agent: entropy seed failed: %v", err)
	} else if seeded {
		log.Printf("fastvm-agent: seeded guest entropy from kernel cmdline")
	}

	port := flag.Uint("port", uint(agent.VsockPort), "virtio-vsock port to listen on")
	workloadConfig := flag.String("workload-config", "/etc/fastvm/workload.json", "optional workload JSON to start when the agent starts")
	workloadLog := flag.String("workload-log", agent.DefaultWorkloadLog, "file that detached workload stdout/stderr is appended to")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if err := run(ctx, uint32(*port), *workloadConfig, *workloadLog); err != nil {
		log.Printf("fastvm-agent: fatal: %v", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, port uint32, workloadConfig, workloadLog string) error {
	log.Printf("fastvm-agent: starting pid=%d vsock_port=%d", os.Getpid(), port)

	// Docker-style per-container identity files: the daemon passes them on the
	// kernel cmdline because fastvm has no bind-mount path into the guest.
	if err := agent.ConfigureGuestIdentityFromCmdline(); err != nil {
		log.Printf("fastvm-agent: guest identity files: %v", err)
	} else {
		log.Printf("fastvm-agent: configured /etc/hostname, /etc/resolv.conf, /etc/hosts")
	}

	ln, err := listenVsock(ctx, port)
	if err != nil {
		return fmt.Errorf("listen vsock port %d: %w", port, err)
	}
	defer ln.Close()
	log.Printf("fastvm-agent: listening on vsock port %d", port)

	configureGuestNetwork(ctx)

	srv := agent.NewServer(workloadLog)
	if workloadConfig != "" {
		w, err := agent.LoadWorkload(workloadConfig)
		if err != nil {
			log.Printf("fastvm-agent: ignoring workload config: %v", err)
		} else if w != nil {
			if err := srv.StartWorkload(*w); err != nil {
				log.Printf("fastvm-agent: workload start failed: %v", err)
			}
		}
	}

	if err := srv.Serve(ctx, ln); err != nil {
		return err
	}
	log.Printf("fastvm-agent: shutting down")
	return nil
}

// configureGuestNetwork applies the fastvm.ip/fastvm.gw kernel cmdline keys to
// eth0 before the workload starts and before the agent reports Ready. The
// virtio-net device can probe asynchronously, so retry briefly.
func configureGuestNetwork(ctx context.Context) {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		log.Printf("fastvm-agent: cannot read /proc/cmdline: %v; guest NIC left unconfigured", err)
		return
	}
	cfg, err := agent.ParseCmdlineNetwork(string(data))
	if err != nil {
		log.Printf("fastvm-agent: guest network cmdline: %v; guest NIC left unconfigured", err)
		return
	}
	if cfg == nil {
		log.Printf("fastvm-agent: no %s on kernel cmdline; guest NIC left unconfigured", agent.CmdlineIPKey)
		return
	}
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for {
		lastErr = agent.ConfigureGuestNetwork(agent.GuestIface, cfg)
		if lastErr == nil {
			log.Printf("fastvm-agent: configured %s %s/%d via %v", agent.GuestIface, cfg.Address, cfg.PrefixLen, cfg.Gateway)
			return
		}
		if time.Now().After(deadline) {
			log.Printf("fastvm-agent: guest network config failed: %v", lastErr)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// listenVsock retries until /dev/vsock exists and the port can be bound.
func listenVsock(ctx context.Context, port uint32) (*vsock.Listener, error) {
	deadline := time.Now().Add(vsockListenTimeout)
	var lastErr error
	for {
		ln, err := vsock.Listen(port, nil)
		if err == nil {
			return ln, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return nil, lastErr
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
