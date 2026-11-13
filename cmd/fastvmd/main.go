// Command fastvmd is the fastvm daemon.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"fastvm/internal/agent"
	"fastvm/internal/api"
	"fastvm/internal/config"
	"fastvm/internal/daemon"
	"fastvm/internal/image"
	"fastvm/internal/network"
	"fastvm/internal/store"
	l2switch "fastvm/internal/switch"
	"fastvm/internal/vmm/firecracker"
	"fastvm/internal/volume"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "fastvmd:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("fastvmd", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config YAML (optional)")
	socketPath := fs.String("socket", "", "override API unix socket path")
	dataDir := fs.String("data-dir", "", "override data directory")
	kernelPath := fs.String("kernel", "", "override kernel image path")
	agentPath := fs.String("agent", "", "override guest agent binary path")
	logLevel := fs.String("log-level", "info", "log level: debug|info|warn|error")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *socketPath != "" {
		cfg.SocketPath = *socketPath
	}
	if *dataDir != "" {
		cfg.DataDir = *dataDir
	}
	if *kernelPath != "" {
		cfg.KernelPath = *kernelPath
	}
	if *agentPath != "" {
		cfg.AgentPath = *agentPath
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return err
	}
	level := slog.LevelInfo
	_ = level.UnmarshalText([]byte(*logLevel))
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if _, err := os.Stat(cfg.KernelPath); err != nil {
		log.Warn("kernel image not found; sandboxes will fail to boot", "path", cfg.KernelPath, "err", err)
	}

	st, err := store.Open(filepath.Join(cfg.DataDir, "state.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	images, err := image.NewBuilder(image.Options{
		BaseDir:   filepath.Join(cfg.DataDir, "images"),
		AgentPath: cfg.AgentPath,
		InitPath:  cfg.InitPath,
	})
	if err != nil {
		return fmt.Errorf("image builder: %w", err)
	}
	volumes, err := volume.NewManager(volume.Options{
		VolDir:     filepath.Join(cfg.DataDir, "volumes"),
		DefaultCOW: cfg.Volume.COW,
	})
	if err != nil {
		return fmt.Errorf("volume manager: %w", err)
	}
	netOpts := network.DefaultOptions()
	netOpts.Bridge = "fastvm0"
	netOpts.SwitchMode = "bridge"
	netOpts.Isolate = cfg.Switch.IsolateSandboxes
	netOpts.WakeNotify = cfg.Switch.WakeNotify
	netOpts.IgnorePorts = cfg.Idle.IgnorePorts

	var (
		sw     l2switch.Switch
		netMgr network.Manager
	)
	if cfg.Switch.Mode == "" || cfg.Switch.Mode == "ebpf" {
		// eBPF mode needs its switch uplink port before the program is loaded
		// (the ifindex is written into the global config map).
		ebpfOpts := netOpts
		ebpfOpts.SwitchMode = "ebpf"
		em, emErr := network.NewManager(ebpfOpts)
		if emErr != nil {
			log.Warn("eBPF network topology unavailable; using Linux bridge", "err", emErr)
		} else if up, ok := em.(network.UplinkProvider); !ok {
			log.Warn("eBPF network manager has no uplink provider; using Linux bridge")
			_ = em.Close()
		} else if uplink, upErr := up.UplinkPort(); upErr != nil {
			log.Warn("eBPF uplink unavailable; using Linux bridge", "err", upErr)
			_ = em.Close()
		} else if switchCfg, cfgErr := l2switch.FromConfig(cfg.Switch); cfgErr != nil {
			log.Warn("invalid switch config; using Linux bridge", "err", cfgErr)
			_ = em.Close()
		} else {
			switchCfg.UplinkIfIndex = uplink.IfIndex
			resolved, switchErr := l2switch.OpenWithFallback(switchCfg)
			if switchErr != nil || resolved == nil || resolved.Mode() != l2switch.ModeEBPF {
				if switchErr != nil {
					log.Warn("eBPF switch unavailable; using Linux bridge", "err", switchErr)
				} else {
					if resolved != nil {
						_ = resolved.Close()
					}
					log.Info("using Linux bridge + AF_PACKET wake path")
				}
				_ = em.Close()
			} else {
				sw = resolved
				netMgr = em
				netOpts.SwitchMode = "ebpf"
				log.Info("eBPF L2 switch enabled", "uplink", uplink.Name, "uplink_ifindex", uplink.IfIndex)
			}
		}
	}
	if netMgr == nil {
		netOpts.SwitchMode = "bridge"
		var err error
		netMgr, err = network.NewManager(netOpts)
		if err != nil {
			return fmt.Errorf("network manager: %w", err)
		}
	}
	if sw != nil {
		defer sw.Close()
	}
	driver, err := firecracker.NewDriver(firecracker.Options{
		Binary: "firecracker",
		RunDir: filepath.Join(cfg.DataDir, "run"),
	})
	if err != nil {
		return fmt.Errorf("firecracker driver: %w", err)
	}

	dialAgent := func(ctx context.Context, vsockUDS string, cid uint32) (agent.Client, error) {
		return agent.DialFirecracker(ctx, vsockUDS, cid, 20*time.Second)
	}
	d, err := daemon.New(daemon.Deps{
		Config: cfg, Store: st, Images: images, Volumes: volumes,
		Network: netMgr, Switch: sw, VMM: driver, DialAgent: dialAgent, Log: log,
	})
	if err != nil {
		_ = netMgr.Close()
		return err
	}
	defer d.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := api.New(d, d, log)
	log.Info("fastvmd started", "socket", cfg.SocketPath, "data_dir", cfg.DataDir,
		"switch", netOpts.SwitchMode, "offload", cfg.Memory.OffloadMode)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ctx, cfg.SocketPath) }()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	case <-ctx.Done():
	}
	log.Info("fastvmd stopping")
	return nil
}
