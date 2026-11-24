package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"fastvm/internal/kernel"
	"fastvm/internal/types"
)

func newKernelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kernel",
		Short: "Manage guest kernels",
		Long: `Manage guest kernels from cloud-hypervisor/linux.

The daemon owns the kernel directory and the "current" symlink. Download and
use do not require sudo; fastvmctl talks to the daemon, which writes its own
kernel directory.`,
	}
	cmd.AddCommand(
		newKernelListCmd(),
		newKernelRemoteListCmd(),
		newKernelDownloadCmd(),
		newKernelUseCmd(),
	)
	return cmd
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func newKernelListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List daemon kernels",
		Args:  noArgsWithHelp,
		RunE: func(_ *cobra.Command, _ []string) error {
			var list types.KernelList
			if err := getJSON("/v1/kernels", &list); err != nil {
				return err
			}
			if len(list.Kernels) == 0 {
				fmt.Printf("No kernels in %s\n", list.Dir)
				return nil
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "\tRELEASE\tASSET\tSIZE\tSHA256\tPATH")
			for _, item := range list.Kernels {
				sha := item.SHA256
				if sha == "" {
					sha = "-"
				}
				release := item.Release
				if release == "" {
					release = item.Name
				}
				asset := item.Asset
				if asset == "" {
					asset = "-"
				}
				marker := ""
				if item.Active {
					marker = "*"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", marker, release, asset, humanBytes(item.SizeBytes), sha, item.Path)
			}
			return tw.Flush()
		},
	}
}

func newKernelRemoteListCmd() *cobra.Command {
	var limit int
	var arch string
	if host, err := kernel.HostArch(); err == nil {
		arch = host
	}
	cmd := &cobra.Command{
		Use:   "remote-ls",
		Short: "List kernels available from cloud-hypervisor/linux",
		Args:  noArgsWithHelp,
		RunE: func(_ *cobra.Command, _ []string) error {
			normalized, err := kernel.NormalizeArch(arch)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			releases, err := kernel.Remote(ctx, &http.Client{Timeout: 30 * time.Second}, limit)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "RELEASE\tASSET\tSIZE\tSHA256")
			for _, rel := range releases {
				for _, asset := range rel.Assets {
					if !kernel.AssetMatchesArch(asset.Name, normalized) ||
						!kernel.DirectBootAsset(asset.Name) {
						continue
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", rel.Tag, asset.Name, humanBytes(asset.Size), asset.SHA256)
				}
			}
			return tw.Flush()
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 10, "number of releases to list")
	cmd.Flags().StringVar(&arch, "arch", arch, "architecture filter (amd64, arm64, riscv64; default: host)")
	return cmd
}

func newKernelDownloadCmd() *cobra.Command {
	var release, assetName string
	var force bool
	cmd := &cobra.Command{
		Use:   "download [release]",
		Short: "Download and verify a kernel into the daemon",
		Example: `  fastvmctl kernel download
  fastvmctl kernel download ch-release-v7.2.8-20261009
  fastvmctl kernel download --asset vmlinux-x86_64`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 1 {
				if release != "" {
					return fmt.Errorf("kernel download: release argument and --release are mutually exclusive")
				}
				release = args[0]
			}
			result, err := kernelDownloadWithProgress(release, func() (types.KernelDownloadResult, error) {
				var out types.KernelDownloadResult
				err := do(http.MethodPost, "/v1/kernels/download", types.KernelDownloadRequest{
					Release: release, Asset: assetName, Force: force,
				}, &out)
				return out, err
			})
			if err != nil {
				return err
			}
			fmt.Printf("%s\t%s\n", result.Path, result.SHA256)
			return nil
		},
	}
	cmd.Flags().StringVar(&release, "release", "", "release tag (default: latest)")
	cmd.Flags().StringVar(&assetName, "asset", "", "asset name (default: host architecture)")
	cmd.Flags().BoolVar(&force, "force", false, "re-download even if the file exists")
	return cmd
}

func newKernelUseCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "use <release>",
		Short:   "Select the daemon default kernel",
		Example: `  fastvmctl kernel use ch-release-v7.2.8-20261009`,
		Args:    exactArgsWithHelp(1),
		RunE: func(_ *cobra.Command, args []string) error {
			var result types.KernelUseResult
			if err := do(http.MethodPost, "/v1/kernels/use", map[string]any{
				"release": args[0],
			}, &result); err != nil {
				return err
			}
			fmt.Printf("%s -> %s\n", result.Current, result.Path)
			return nil
		},
	}
}

func noArgsWithHelp(_ *cobra.Command, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("expected no arguments, got %d", len(args))
	}
	return nil
}
