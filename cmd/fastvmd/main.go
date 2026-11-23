// Command fastvmd is the fastvm daemon.
package main

import (
	"context"
	"errors"
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
	"fastvm/internal/version"
	"fastvm/internal/vmm/cloudhypervisor"

	"fastvm/internal/volume"
	"github.com/charmbracelet/fang"
	"github.com/spf13/cobra"
)

func main() {
	root := newRootCmd()
	if err := fang.Execute(context.Background(), root, fang.WithVersion(version.String())); err != nil {
		os.Exit(1)
	}
}

type daemonOptions struct {
	configPath string
	socketPath string
	dataDir    string
	kernelPath string
	agentPath  string
	logLevel   string
}

func newRootCmd() *cobra.Command {
	var opts daemonOptions
	root := &cobra.Command{
		Use:   "fastvmd",
		Short: "fastvm daemon",
		Long: `fastvmd runs the fastvm control plane: the sandbox API, image and volume
management, networking/switch, and Cloud Hypervisor lifecycle.

Run as root (or with CAP_NET_ADMIN/CAP_BPF) so TAPs, nftables, and the L2 switch
can be configured. The daemon owns a unix socket; clients use fastvmctl.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDaemon(cmd.Context(), opts)
		},
	}
	f := root.Flags()
	f.StringVarP(&opts.configPath, "config", "c", "", "path to config YAML (optional)")
	f.StringVar(&opts.socketPath, "socket", "", "override API unix socket path")
	f.StringVar(&opts.dataDir, "data-dir", "", "override data directory")
	f.StringVar(&opts.kernelPath, "kernel", "", "override kernel image path")
	f.StringVar(&opts.agentPath, "agent", "", "override guest agent binary path")
	f.StringVar(&opts.logLevel, "log-level", "info", "log level: debug|info|warn|error")
	return root
}

func runDaemon(ctx context.Context, opts daemonOptions) error {
	cfg, err := config.Load(opts.configPath)
	if err != nil {
		return err
	}
	if opts.socketPath != "" {
		cfg.SocketPath = opts.socketPath
	}
	if opts.dataDir != "" {
		cfg.DataDir = opts.dataDir
	}
	if opts.kernelPath != "" {
		cfg.KernelPath = opts.kernelPath
	}
	if opts.agentPath != "" {
		cfg.AgentPath = opts.agentPath
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return err
	}
	level := slog.LevelInfo
	_ = level.UnmarshalText([]byte(opts.logLevel))
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
		DefaultCOW: cfg.Storage.COW,
	})
	if err != nil {
		return fmt.Errorf("volume manager: %w", err)
	}
	netOpts := network.DefaultOptions()
	netOpts.Bridge = "fastvm0"
	netOpts.SwitchMode = "bridge"
	netOpts.Isolate = cfg.Network.DataPath.IsolateSandboxes
	netOpts.WakeNotify = cfg.Network.DataPath.WakeNotify
	netOpts.IgnorePorts = cfg.Lifecycle.IdleDetection.IgnorePorts

	var (
		sw     l2switch.Switch
		netMgr network.Manager
	)
	if cfg.Network.DataPath.Mode == "" || cfg.Network.DataPath.Mode == "ebpf" {
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
		} else if switchCfg, cfgErr := l2switch.FromConfig(cfg.Network.DataPath); cfgErr != nil {
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
	driver, err := cloudhypervisor.NewDriver(cloudhypervisor.Options{
		Binary:    cfg.VMM.Binary,
		Virtiofsd: cfg.VMM.Virtiofsd,
		RunDir:    filepath.Join(cfg.DataDir, "run"),
	})
	if err != nil {
		return fmt.Errorf("cloud hypervisor driver: %w", err)
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

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := api.New(d, d, log)
	log.Info("fastvmd started", "socket", cfg.SocketPath, "data_dir", cfg.DataDir,
		"switch", netOpts.SwitchMode, "offload", cfg.Memory.OffloadMode)
	errCh := make(chan error, 2)
	go func() { errCh <- srv.Serve(ctx, cfg.SocketPath) }()
	if cfg.API.Host != "" {
		tlsConfig, err := api.LoadTLSConfig(
			filepath.Join(cfg.API.CertPath, "cert.pem"),
			filepath.Join(cfg.API.CertPath, "key.pem"),
			filepath.Join(cfg.API.CertPath, "ca.pem"),
		)
		if err != nil {
			return err
		}
		log.Info("REST API listening with mTLS", "addr", cfg.API.Host, "cert_path", cfg.API.CertPath)
		go func() { errCh <- srv.ServeTLS(ctx, cfg.API.Host, tlsConfig) }()
	}
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
