package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"fastvm/internal/types"
	"fastvm/internal/version"
)

// exactArgsWithHelp validates an exact argument count and shows command help on
// mismatch, so users see the useful synopsis/examples instead of only Cobra's
// generic argument-count error.
func exactArgsWithHelp(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) == n {
			return nil
		}
		_ = cmd.Help()
		return fmt.Errorf("missing arguments: expected %d, got %d", n, len(args))
	}
}

// maxArgsWithHelp shows command help when more than n arguments are provided.
func maxArgsWithHelp(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) <= n {
			return nil
		}
		_ = cmd.Help()
		return fmt.Errorf("too many arguments: expected at most %d, got %d", n, len(args))
	}
}

// minArgsWithHelp shows command help when fewer than n arguments are provided.
func minArgsWithHelp(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) >= n {
			return nil
		}
		_ = cmd.Help()
		return fmt.Errorf("missing arguments: expected at least %d, got %d", n, len(args))
	}
}

// --- image -----------------------------------------------------------------

func newImageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "image",
		Short: "Manage base images",
	}
	cmd.AddCommand(newImagePullCmd(), newImageListCmd(), newImageRemoveCmd())
	return cmd
}

func newImagePullCmd() *cobra.Command {
	var sizeGB int
	cmd := &cobra.Command{
		Use:     "pull <image>",
		Short:   "Pull an OCI image into the base-disk store",
		Example: `  fastvmctl image pull alpine`,
		Args:    exactArgsWithHelp(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ref := args[0]
			disk, err := pullImageWithProgress(ref, func() (types.BaseDisk, error) {
				var pulled types.BaseDisk
				if err := do(http.MethodPost, "/v1/images/pull", map[string]any{
					"ref": ref, "size_gb": sizeGB,
				}, &pulled); err != nil {
					return types.BaseDisk{}, err
				}
				return pulled, nil
			})
			if err != nil {
				return err
			}
			fmt.Printf("%s\t%s\t%d bytes\n", disk.Name, disk.Digest, disk.SizeBytes)
			return nil
		},
	}
	cmd.Flags().IntVar(&sizeGB, "size-gb", 0, "base disk size in GiB")
	return cmd
}

func newImageListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List base images",
		Example: `  fastvmctl image ls`,
		Args:    cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			var disks []types.BaseDisk
			if err := getJSON("/v1/images", &disks); err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "TAG\tDIGEST\tSIZE\tSOURCE")
			for _, d := range disks {
				tag := "-"
				if len(d.Tags) > 0 {
					tag = strings.Join(d.Tags, ",")
				} else if d.Tag != "" {
					tag = d.Tag
				}
				fmt.Fprintf(tw, "%s\t%s\t%d MiB\t%s\n", tag, short(d.Digest), d.SizeBytes/(1<<20), d.Source)
			}
			return tw.Flush()
		},
	}
}

func newImageRemoveCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:     "rm <tag|digest>...",
		Aliases: []string{"remove"},
		Short:   "Delete base images",
		Example: `  fastvmctl image rm alpine`,
		Args:    minArgsWithHelp(1),
		RunE: func(_ *cobra.Command, refs []string) error {
			for _, ref := range refs {
				path := "/v1/images?ref=" + url.QueryEscape(ref)
				if force {
					path += "&force=1"
				}
				var disk types.BaseDisk
				if err := do(http.MethodDelete, path, nil, &disk); err != nil {
					return err
				}
				fmt.Printf("deleted %s (%s)\n", disk.Identity(), short(disk.Digest))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "delete even while sandboxes reference the image")
	return cmd
}

// --- sandbox lifecycle ------------------------------------------------------

type networkOptions struct {
	net         bool
	noNet       bool
	allowHosts  []string
	autoPublish bool
	interactive bool
	tty         bool
	noAutoSleep bool
}

func addNetworkFlags(cmd *cobra.Command, opts *networkOptions) {
	cmd.Flags().BoolVar(&opts.net, "net", false, "enable networking (default: off)")
	cmd.Flags().BoolVar(&opts.noNet, "no-net", false, "explicitly disable networking")
	cmd.Flags().StringArrayVar(&opts.allowHosts, "allow-host", nil,
		"allow egress only to this host (repeatable; implies --net)")
	cmd.Flags().BoolVar(&opts.autoPublish, "auto-publish", false, "discover and publish guest TCP listening ports >= 1024")
	cmd.Flags().BoolVar(&opts.noAutoSleep, "no-auto-sleep", false, "never auto-sleep")
}

func (o *networkOptions) enabled() bool { return o.net && !o.noNet }

func (o *networkOptions) validate(cmdName string) error {
	if o.net && o.noNet {
		return fmt.Errorf("%s: --net and --no-net are mutually exclusive", cmdName)
	}
	if len(o.allowHosts) > 0 || o.autoPublish {
		if o.noNet {
			return fmt.Errorf("%s: --allow-host/--auto-publish cannot be combined with --no-net", cmdName)
		}
		o.net = true
	}
	return nil
}

func newRunCmd() *cobra.Command {
	var image, name, file, fromSnapshot, entrypoint, workdir string
	var vcpus, memory int
	var publishes, envs, volumes []string
	var userData string
	var detach, noStart bool
	var opts networkOptions
	cmd := &cobra.Command{
		Use:   "run [flags] -- <command> [args...]",
		Short: "Create, boot, or start a sandbox",
		Long: `Create and boot a sandbox from an image or an .fvmfile, or boot an
already-created sandbox.

The image reference is built into a base disk on demand if it is not already in
the image store. Pass the guest command after -- so its own flags are not parsed
by fastvmctl.

By default run waits for the command and streams its stdout/stderr, then removes
the one-off sandbox. An interactive -it shell is also removed on exit. Use -d to
keep the sandbox and start the command in the background. Use --no-start to
create without booting, then resume it later. A run with no command leaves the
sandbox running and prints its ID/state/name.

With -f, explicitly passed creation flags override the .fvmfile values; --env
merges per key and --volume wins for a matching guest path. --entrypoint overrides
the fvmfile entrypoint, like Docker's --entrypoint. -w/--workdir sets the guest
working directory for the command and interactive shell. --auto-publish discovers
guest TCP listeners >= 1024 and publishes them 1:1 on 127.0.0.1 at the same
host port. Privileged ports are skipped; use an explicit --publish/-p mapping for
them. Occupied host ports are skipped and retried later; they are never
reassigned.

--volume accepts a host directory share (/host[:guest][:ro]) or a writable
block data volume (size=8GiB,mount=/data). Data volumes are cloned from a blank
ext4 template and disable automatic sleep and snapshots.`,
		Example: `  fastvmctl run --net --image alpine -- uname -a
  fastvmctl run --net -it --image alpine -- /bin/sh
  fastvmctl run -f examples/coding-agents/codex.fvmfile
  fastvmctl run -f examples/coding-agents/copilot.fvmfile -it --entrypoint bash
  fastvmctl run --image alpine --volume size=8GiB,mount=/data
  fastvmctl run --image alpine --volume /host/project:/workspace
  fastvmctl run --image alpine --name agent --no-start
  fastvmctl run --from-snapshot original-vm-20261119-182000 --name copy-1
  fastvmctl run --name agent`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cobraCmd *cobra.Command, cmdArgs []string) error {
			if detach && (opts.interactive || opts.tty) {
				return errors.New("--detach and --interactive/--tty are mutually exclusive")
			}
			if noStart && (detach || opts.interactive || opts.tty || len(cmdArgs) > 0 || entrypoint != "") {
				return errors.New("--no-start cannot be combined with a command, --entrypoint, -d, -i, or -t")
			}

			if fromSnapshot != "" {
				if file != "" || cobraCmd.Flags().Changed("image") {
					return errors.New("--from-snapshot cannot be combined with --image or --file")
				}
				for _, flagName := range []string{"vcpus", "memory", "publish", "env", "volume", "net", "no-net", "allow-host", "auto-publish", "no-auto-sleep", "entrypoint", "workdir"} {
					if cobraCmd.Flags().Changed(flagName) {
						return fmt.Errorf("--%s does not apply with --from-snapshot", flagName)
					}
				}
				if noStart {
					return errors.New("--no-start does not apply with --from-snapshot")
				}
				if name == "" {
					return errors.New("--name is required with --from-snapshot")
				}
				var sb types.Sandbox
				if err := do(http.MethodPost, "/v1/sandboxes", map[string]any{
					"name": name, "from_snapshot": fromSnapshot, "start": true,
				}, &sb); err != nil {
					return err
				}
				if detach || len(cmdArgs) == 0 {
					fmt.Printf("%s\t%s\t%s\n", sb.ID, sb.State, sb.Name)
					return nil
				}
				if opts.interactive || opts.tty {
					return removeAfterRun(sb.ID, runShell(sb.ID, cmdArgs, nil))
				}
				return removeAfterRun(sb.ID, execSandbox(sb.ID, cmdArgs, nil))
			}

			runArgs := append([]string(nil), cmdArgs...)
			var ports []types.PortMapping
			var env map[string]string
			var vols []types.Volume
			var dataVols []types.DataVolume
			noAutoSleep := false

			if file != "" {
				spec, err := LoadFVMFile(file)
				if err != nil {
					return err
				}
				changed := func(flag string) bool { return cobraCmd.Flags().Changed(flag) }

				if !changed("image") {
					image = spec.Image
				}
				if name == "" {
					name = spec.Name
				}
				if !changed("workdir") {
					workdir = spec.Workdir
				}
				if !changed("vcpus") {
					vcpus = spec.VCPUs
				}
				if !changed("memory") {
					memory = spec.MemoryMB
				}
				if changed("publish") {
					if ports, err = parsePublishes(publishes); err != nil {
						return err
					}
				} else {
					ports = spec.Ports
				}
				if changed("env") {
					flagEnv, err := parseEnvs(envs)
					if err != nil {
						return err
					}
					env = mergeEnvMaps(spec.Env, flagEnv)
				} else {
					env = spec.Env
				}
				if changed("volume") {
					extraVols, extraData, err := parseVolumeFlags(volumes)
					if err != nil {
						return err
					}
					vols = mergeVolumes(spec.Volumes, extraVols)
					dataVols = mergeDataVolumes(spec.DataVolumes, extraData)
				} else {
					vols = spec.Volumes
					dataVols = append([]types.DataVolume(nil), spec.DataVolumes...)
				}

				if changed("net") {
					opts.net = true
				}
				if changed("no-net") {
					opts.noNet = true
				}
				if !changed("net") && !changed("no-net") {
					opts.net, opts.noNet = spec.NetworkEnabled, false
				}
				if !changed("auto-publish") {
					opts.autoPublish = spec.AutoPublish
				}
				if !changed("allow-host") {
					opts.allowHosts = spec.AllowHosts
				}
				if changed("no-net") && !changed("allow-host") {
					// Disabling the NIC overrides the file allowlist too.
					opts.allowHosts = nil
				}
				if err := opts.validate("run"); err != nil {
					return err
				}
				if changed("no-auto-sleep") {
					noAutoSleep = opts.noAutoSleep
				} else {
					noAutoSleep = spec.NoAutoSleep
				}
				userData = spec.UserData
				if noStart && spec.UserData != "" {
					return errors.New("--no-start cannot be combined with user_data")
				}
				ep := spec.Entrypoint
				if changed("entrypoint") {
					ep = []string{entrypoint}
				}
				runArgs = resolveWorkload(ep, spec.Command, runArgs)
			} else {
				if err := opts.validate("run"); err != nil {
					return err
				}
				if image == "" {
					if name == "" {
						_ = cobraCmd.Help()
						return errors.New("--image, --file, or --name is required")
					}
					if noStart {
						return errors.New("--no-start requires --image or --file")
					}
					if len(cmdArgs) > 0 || entrypoint != "" || workdir != "" || detach || opts.interactive || opts.tty {
						return errors.New("booting an existing sandbox does not take a command, --entrypoint, --workdir, or -d/-i/-t")
					}
					return resumeSandbox(name)
				}
				var err error
				if ports, err = parsePublishes(publishes); err != nil {
					return err
				}
				if env, err = parseEnvs(envs); err != nil {
					return err
				}
				if vols, dataVols, err = parseVolumeFlags(volumes); err != nil {
					return err
				}
				if entrypoint != "" {
					runArgs = resolveWorkload([]string{entrypoint}, nil, runArgs)
				}
				noAutoSleep = opts.noAutoSleep
			}

			if workdir != "" && (!filepath.IsAbs(workdir) || filepath.Clean(workdir) != workdir || workdir == "/") {
				return fmt.Errorf("--workdir %q must be an absolute clean path below /", workdir)
			}
			if err := ensureImage(image); err != nil {
				return err
			}

			workload := []string(nil)
			if detach {
				workload = runArgs
			}
			body := map[string]any{
				"name": name, "image": image, "workdir": workdir,
				"vcpus": vcpus, "memory_mb": memory,
				"ports": ports, "volumes": vols, "data_volumes": dataVols, "cmd": workload, "env": env,
				"no_auto_sleep": noAutoSleep, "start": !noStart,
				"network_enabled": opts.enabled(), "allow_hosts": opts.allowHosts,
				"user_data": userData, "auto_publish": opts.autoPublish,
			}
			var sb types.Sandbox
			if err := do(http.MethodPost, "/v1/sandboxes", body, &sb); err != nil {
				return err
			}
			if noStart || detach {
				fmt.Printf("%s\t%s\t%s\n", sb.ID, sb.State, sb.Name)
				return nil
			}
			if opts.interactive || opts.tty {
				argv := runArgs
				if len(argv) == 0 {
					argv = []string{"/bin/sh"}
				}
				return removeAfterRun(sb.ID, runShell(sb.ID, argv, env))
			}
			if len(runArgs) > 0 {
				return removeAfterRun(sb.ID, execSandbox(sb.ID, runArgs, env))
			}
			fmt.Printf("%s\t%s\t%s\n", sb.ID, sb.State, sb.Name)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVarP(&file, "file", "f", "", "load sandbox config from an .fvmfile")
	f.StringVar(&fromSnapshot, "from-snapshot", "", "boot a new sandbox from a saved snapshot")
	f.StringVar(&entrypoint, "entrypoint", "", "override the image/fvmfile entrypoint")
	f.StringVarP(&workdir, "workdir", "w", "", "guest working directory")
	f.StringVar(&image, "image", "", "OCI image or base disk tag/digest")
	f.StringVar(&name, "name", "", "sandbox name or ID (boot it if no image/file is given)")
	f.IntVar(&vcpus, "vcpus", 0, "vCPUs (0 = default)")
	f.IntVar(&memory, "memory", 0, "memory MiB (0 = default)")
	f.StringArrayVar(&publishes, "publish", nil, "publish a port host:guest[/tcp|udp]")
	f.StringArrayVar(&envs, "env", nil, "workload env K=V")
	f.StringArrayVarP(&volumes, "volume", "v", nil, "mount a host directory (host[:guest][:ro]) or data volume (size=8GiB,mount=/data)")
	f.BoolVarP(&detach, "detach", "d", false, "run the command in the background and print the sandbox ID")
	f.BoolVar(&noStart, "no-start", false, "create without booting")
	f.BoolVarP(&opts.interactive, "interactive", "i", false, "attach an interactive PTY")
	f.BoolVarP(&opts.tty, "tty", "t", false, "allocate a TTY")
	addNetworkFlags(cmd, &opts)
	return cmd
}

func newSnapshotCmd() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "snapshot [sandbox]",
		Short: "Create, list, or remove snapshots",
		Example: `  fastvmctl snapshot dev
  fastvmctl snapshot ls
  fastvmctl run --from-snapshot dev-20261121-001234 --name dev2`,
		Long: `Pause a sandbox, save its RAM/CPU state and writable disk under the daemon
data directory, then resume it. Prints the generated snapshot name for use with
'run --from-snapshot'.

  snapshot <sandbox> [--name <name>]
                       create a snapshot (name generated by default)
  snapshot ls          list saved snapshots
  snapshot rm <name>   delete saved snapshots`,
		Args: maxArgsWithHelp(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			if len(args) != 1 {
				return errors.New("snapshot: expected one sandbox")
			}
			var resp struct {
				Snapshot string `json:"snapshot"`
			}
			if err := do(http.MethodPost, "/v1/sandboxes/"+url.PathEscape(args[0])+"/snapshot",
				map[string]any{"name": name}, &resp); err != nil {
				return err
			}
			fmt.Println(resp.Snapshot)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "snapshot name (default: <sandbox>-<timestamp>)")
	cmd.AddCommand(newSnapshotListCmd(), newSnapshotRemoveCmd())
	return cmd
}

func newSnapshotListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List saved snapshots",
		Example: `  fastvmctl snapshot ls`,
		Args:    cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			var snapshots []types.SnapshotInfo
			if err := getJSON("/v1/snapshots", &snapshots); err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tCREATED\tIMAGE\tVCPUS\tMEM")
			for _, c := range snapshots {
				created := "-"
				if !c.CreatedAt.IsZero() {
					created = c.CreatedAt.Local().Format("2006-01-02 15:04:05")
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%dM\n",
					c.Name, created, short(c.ImageDigest), c.VCPUs, c.MemoryMB)
			}
			return tw.Flush()
		},
	}
}

func newSnapshotRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rm <snapshot>...",
		Short:   "Delete one or more saved snapshots",
		Example: `  fastvmctl snapshot rm before-upgrade`,
		Args:    minArgsWithHelp(1),
		RunE: func(_ *cobra.Command, names []string) error {
			var errs []error
			for _, name := range names {
				if err := do(http.MethodDelete, "/v1/snapshots/"+url.PathEscape(name), nil, nil); err != nil {
					errs = append(errs, fmt.Errorf("rm snapshot %s: %w", name, err))
				}
			}
			return errors.Join(errs...)
		},
	}
}

// mergeEnvMaps returns base with override applied; CLI env wins per key.
func mergeEnvMaps(base, override map[string]string) map[string]string {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(override))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range override {
		out[k] = v
	}
	return out
}

// mergeDataVolumes appends CLI data volumes, replacing file volumes with the
// same mount path so an explicit flag wins.
func mergeDataVolumes(base, extra []types.DataVolume) []types.DataVolume {
	out := append([]types.DataVolume(nil), base...)
	index := make(map[string]int, len(out))
	for i, v := range out {
		index[v.Mount] = i
	}
	for _, v := range extra {
		if i, ok := index[v.Mount]; ok {
			out[i] = v
		} else {
			index[v.Mount] = len(out)
			out = append(out, v)
		}
	}
	return out
}

// mergeVolumes appends CLI volumes, replacing file volumes with the same
// guest path so an explicit flag wins.
func mergeVolumes(base, extra []types.Volume) []types.Volume {
	out := append([]types.Volume(nil), base...)
	index := make(map[string]int, len(out))
	for i, vol := range out {
		index[vol.GuestPath] = i
	}
	for _, vol := range extra {
		if i, ok := index[vol.GuestPath]; ok {
			out[i] = vol
			continue
		}
		index[vol.GuestPath] = len(out)
		out = append(out, vol)
	}
	return out
}

// --- inspection/control ----------------------------------------------------

func newListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "ps",
		Short:   "List sandboxes",
		Example: `  fastvmctl ps`,
		Args:    cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			var list []types.Sandbox
			if err := getJSON("/v1/sandboxes", &list); err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tNAME\tSTATE\tIMAGE\tIP\tVCPUS\tMEM")
			for _, sb := range list {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%dM\n",
					short(sb.ID), sb.Name, sb.State, short(sb.BaseDisk), sb.IP, sb.VCPUs, sb.MemoryMB)
			}
			return tw.Flush()
		},
	}
}

func newGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "get <sandbox>",
		Aliases: []string{"inspect"},
		Short:   "Show one sandbox as JSON",
		Example: `  fastvmctl get dev`,
		Args:    exactArgsWithHelp(1),
		RunE: func(_ *cobra.Command, args []string) error {
			var sb types.Sandbox
			if err := getJSON("/v1/sandboxes/"+args[0], &sb); err != nil {
				return err
			}
			b, _ := json.MarshalIndent(sb, "", "  ")
			fmt.Println(string(b))
			return nil
		},
	}
}

// execSandbox runs a non-interactive guest command and writes its stdout/stderr
// to the CLI streams. It returns the standard error for non-zero exit codes.
func execSandbox(ref string, argv []string, env map[string]string) error {
	var resp struct {
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		Error    string `json:"error"`
	}
	body := map[string]any{"argv": argv}
	if len(env) > 0 {
		body["env"] = env
	}
	if err := do(http.MethodPost, "/v1/sandboxes/"+url.PathEscape(ref)+"/exec", body, &resp); err != nil {
		return err
	}
	if resp.Stdout != "" {
		fmt.Print(resp.Stdout)
	}
	if resp.Stderr != "" {
		fmt.Fprint(os.Stderr, resp.Stderr)
	}
	if resp.Error != "" {
		fmt.Fprintln(os.Stderr, "guest:", resp.Error)
	}
	if resp.ExitCode != 0 {
		return fmt.Errorf("command exited with code %d", resp.ExitCode)
	}
	return nil
}

// removeAfterRun implements Docker-style foreground cleanup: a one-off
// sandbox is deleted when its command/shell exits. Detached runs never reach
// this helper.
func removeAfterRun(id string, runErr error) error {
	deleteErr := do(http.MethodDelete, "/v1/sandboxes/"+url.PathEscape(id), nil, nil)
	if runErr != nil {
		if deleteErr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not remove sandbox %s: %v\n", short(id), deleteErr)
		}
		return runErr
	}
	return deleteErr
}

func newExecCmd() *cobra.Command {
	var interactive, tty bool
	cmd := &cobra.Command{
		Use:   "exec [flags] <sandbox> -- <command> [args...]",
		Short: "Run a command in a sandbox",
		Example: `  fastvmctl exec dev -- uname -a
  fastvmctl exec -it dev -- /bin/bash`,
		Args: minArgsWithHelp(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, argv := args[0], args[1:]
			interactive = interactive || tty
			if interactive {
				if len(argv) == 0 {
					argv = []string{"/bin/sh"}
				}
				return runShell(ref, argv, nil)
			}
			if len(argv) == 0 {
				_ = cmd.Help()
				return errors.New("command required (or use -it for an interactive shell)")
			}
			return execSandbox(ref, argv, nil)
		},
	}
	cmd.Flags().BoolVarP(&interactive, "interactive", "i", false, "attach an interactive PTY")
	cmd.Flags().BoolVarP(&tty, "tty", "t", false, "allocate a TTY")
	return cmd
}

func newShellCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "shell <sandbox> [-- <command>...]",
		Short:   "Open an interactive shell",
		Example: `  fastvmctl shell dev`,
		Args:    minArgsWithHelp(1),
		RunE: func(_ *cobra.Command, args []string) error {
			argv := args[1:]
			if len(argv) == 0 {
				argv = []string{"/bin/sh"}
			}
			return runShell(args[0], argv, nil)
		},
	}
}

func newPauseCmd() *cobra.Command {
	var deep bool
	cmd := &cobra.Command{
		Use:   "pause <sandbox>",
		Short: "Pause or offload a sandbox",
		Example: `  fastvmctl pause dev
  fastvmctl pause dev --deep`,
		Args: exactArgsWithHelp(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return do(http.MethodPost, "/v1/sandboxes/"+url.PathEscape(args[0])+"/sleep",
				map[string]any{"deep": deep}, nil)
		},
	}
	cmd.Flags().BoolVar(&deep, "deep", false, "offload memory to disk")
	return cmd
}

func newResumeCmd() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:     "resume [sandbox]",
		Short:   "Boot a created sandbox or wake a paused/dormant one",
		Example: `  fastvmctl resume dev`,
		Long: `Make an existing sandbox active:
  Created -> boot it
  Paused/Dormant -> wake/restore it

Running/idle sandboxes return an error; use exec instead.`,
		Args: maxArgsWithHelp(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := name
			if ref == "" && len(args) > 0 {
				ref = args[0]
			}
			if ref == "" {
				_ = cmd.Help()
				return errors.New("sandbox required")
			}
			return resumeSandbox(ref)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "sandbox name or ID")
	return cmd
}

// resumeSandbox dispatches the state-dependent meaning of `resume`.
func resumeSandbox(ref string) error {
	var sb types.Sandbox
	if err := getJSON("/v1/sandboxes/"+url.PathEscape(ref), &sb); err != nil {
		return err
	}
	switch sb.State {
	case types.StateCreated:
		if err := do(http.MethodPost, "/v1/sandboxes/"+url.PathEscape(sb.ID)+"/start", nil, nil); err != nil {
			return err
		}
	case types.StatePaused, types.StateDormant:
		if err := do(http.MethodPost, "/v1/sandboxes/"+url.PathEscape(sb.ID)+"/wake", nil, nil); err != nil {
			return err
		}
	case types.StateRunning, types.StateIdle:
		return fmt.Errorf("sandbox is already %s; use exec to run a command", sb.State)
	default:
		return fmt.Errorf("cannot resume sandbox in state %s", sb.State)
	}
	if err := getJSON("/v1/sandboxes/"+url.PathEscape(sb.ID), &sb); err != nil {
		return err
	}
	fmt.Printf("%s\t%s\t%s\n", sb.ID, sb.State, sb.Name)
	return nil
}

func newDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rm <sandbox>...",
		Aliases: []string{"delete"},
		Short:   "Delete one or more sandboxes",
		Example: `  fastvmctl rm dev`,
		Args:    minArgsWithHelp(1),
		RunE: func(_ *cobra.Command, refs []string) error {
			var errs []error
			for _, ref := range refs {
				if err := do(http.MethodDelete, "/v1/sandboxes/"+url.PathEscape(ref), nil, nil); err != nil {
					errs = append(errs, fmt.Errorf("rm %s: %w", ref, err))
				}
			}
			return errors.Join(errs...)
		},
	}
}

func newEventsCmd() *cobra.Command {
	limit := 50
	cmd := &cobra.Command{
		Use:   "events",
		Short: "Show recent daemon events",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			var events []types.Event
			if err := getJSON(fmt.Sprintf("/v1/events?limit=%d", limit), &events); err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "TIME\tNAME\tID\tTYPE\tMESSAGE")
			for _, ev := range events {
				name, id := ev.SandboxName, short(ev.SandboxID)
				if ev.SandboxID == "" || ev.SandboxID == "daemon" {
					name, id = "daemon", "-"
				} else if name == "" {
					name = "(unknown)"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
					ev.Time.Format("15:04:05.000"), name, id, ev.Type, ev.Message)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().IntVarP(&limit, "limit", "n", 50, "number of events")
	return cmd
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show CLI and daemon versions",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			fmt.Printf("fastvmctl %s\n", version.String())
			var v map[string]string
			if err := getJSON("/v1/version", &v); err != nil {
				return err
			}
			fmt.Printf("fastvmd %s\n", v["version"])
			return nil
		},
	}
}
