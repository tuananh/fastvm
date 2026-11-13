// Command fastvm is the fastvm CLI client.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"fastvm/internal/types"
)

const defaultSocket = "/run/fastvm/fastvm.sock"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "fastvm:", err)
		os.Exit(1)
	}
}

func socketPath() string {
	if s := os.Getenv("FASTVM_SOCKET"); s != "" {
		return s
	}
	return defaultSocket
}

func run(args []string) error {
	// Accept "-socket" before the subcommand too: fastvm -socket X ls
	if len(args) >= 2 && args[0] == "-socket" {
		os.Setenv("FASTVM_SOCKET", args[1])
		args = args[2:]
	}
	if len(args) == 0 {
		usage()
		return errors.New("missing command")
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version":
		var v map[string]string
		if err := getJSON("/v1/version", &v); err != nil {
			return err
		}
		fmt.Printf("fastvm %s\n", v["version"])
		return nil
	case "image":
		return cmdImage(rest)
	case "run":
		return cmdRun(rest)
	case "ls", "list":
		return cmdList(rest)
	case "get", "inspect":
		return cmdGet(rest)
	case "exec":
		return cmdExec(rest)
	case "template":
		return cmdTemplate(rest)
	case "shell":
		return cmdShell(rest)
	case "pause":
		return cmdPause(rest)
	case "resume":
		return cmdResume(rest)
	case "rm", "delete":
		return cmdDelete(rest)
	case "events":
		return cmdEvents(rest)
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `fastvm — micro-VM sandbox runtime

usage: fastvm <command> [flags] [args]

commands:
  image <build|ls|rm>                                     image management
    image build [-f Dockerfile] [-t tag] [-size-gb N] [context]
    image ls
    image rm [--force] <tag|digest>...
  run --image <tag|digest> [flags] -- <cmd>               create a sandbox
  ls                                                      list sandboxes
  get <sandbox>                                           show one sandbox
  exec [-it] <sandbox> -- <cmd> [args...]                 run a command; -it = interactive PTY
  shell <sandbox> [-- <cmd>...]                           shorthand for exec -it <sandbox> -- /bin/sh
  template list                                           list coding-agent templates
  template start <name> [-d] ...                          boot a sandbox from a template
  pause <sandbox> [--deep]                                pause / offload
  resume <sandbox>                                        resume a paused sandbox
  rm <sandbox>                                            delete a sandbox
  events [-n N]                                           recent events
  version                                                 daemon version

global: -socket <path> or FASTVM_SOCKET
sandbox refs: full name, full ID, or a unique ID prefix (Docker-style)
`)
}

// --- HTTP client -----------------------------------------------------------

func client() *http.Client {
	return &http.Client{
		Timeout: 15 * time.Minute, // builds/execs can be long
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath())
			},
		},
	}
}

func do(method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://fastvm"+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client().Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach fastvmd at %s: %w", socketPath(), err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var env struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &env) == nil && env.Error != "" {
			return errors.New(env.Error)
		}
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func getJSON(path string, out any) error { return do(http.MethodGet, path, nil, out) }

// --- commands --------------------------------------------------------------

func cmdImageBuild(args []string) error {
	fs := flag.NewFlagSet("image build", flag.ContinueOnError)
	dockerfile := fs.String("f", "", "path to Dockerfile")
	tag := fs.String("t", "", "image tag")
	sizeGB := fs.Int("size-gb", 0, "base disk size in GiB")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctxDir := "."
	if fs.NArg() > 0 {
		ctxDir = fs.Arg(0)
	}
	if *tag == "" {
		return errors.New("image build: -t tag is required")
	}
	var disk types.BaseDisk
	err := do(http.MethodPost, "/v1/images/build", map[string]any{
		"dockerfile": *dockerfile, "context": ctxDir, "tag": *tag, "size_gb": *sizeGB,
	}, &disk)
	if err != nil {
		return err
	}
	fmt.Printf("%s\t%s\t%d bytes\n", disk.Name, disk.Digest, disk.SizeBytes)
	return nil
}

func cmdImage(args []string) error {
	if len(args) == 0 {
		return errors.New("image: expected subcommand build, ls, or rm")
	}
	switch args[0] {
	case "build":
		return cmdImageBuild(args[1:])
	case "ls", "list":
		return cmdImageList(args[1:])
	case "rm", "remove":
		return cmdImageRemove(args[1:])
	default:
		return fmt.Errorf("image: unknown subcommand %q (use build, ls, or rm)", args[0])
	}
}

func cmdImageRemove(args []string) error {
	fs := flag.NewFlagSet("image rm", flag.ContinueOnError)
	force := fs.Bool("force", false, "delete even while sandboxes reference the image")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("image rm: at least one tag or digest is required")
	}
	for _, ref := range fs.Args() {
		path := "/v1/images?ref=" + url.QueryEscape(ref)
		if *force {
			path += "&force=1"
		}
		var disk types.BaseDisk
		if err := do(http.MethodDelete, path, nil, &disk); err != nil {
			return err
		}
		fmt.Printf("deleted %s (%s)\n", disk.Identity(), short(disk.Digest))
	}
	return nil
}

func cmdImageList(args []string) error {
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
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	image := fs.String("image", "", "base disk tag or digest")
	name := fs.String("name", "", "sandbox name")
	vcpus := fs.Int("vcpus", 0, "vCPUs (0 = default)")
	mem := fs.Int("memory", 0, "memory MiB (0 = default)")
	noAutoSleep := fs.Bool("no-auto-sleep", false, "never auto-sleep")
	var publishes multiFlag
	fs.Var(&publishes, "publish", "publish a port host:guest[/tcp|udp] (repeatable)")
	var envs multiFlag
	fs.Var(&envs, "env", "workload env K=V (repeatable)")
	// Stop flag parsing at "--" so the workload command can have flags.
	flagArgs, cmdArgs := splitDashDash(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if *image == "" {
		return errors.New("run: --image is required")
	}
	ports, err := parsePublishes(publishes)
	if err != nil {
		return err
	}
	env, err := parseEnvs(envs)
	if err != nil {
		return err
	}
	body := map[string]any{
		"name": *name, "image": *image, "vcpus": *vcpus, "memory_mb": *mem,
		"ports": ports, "cmd": cmdArgs, "env": env, "no_auto_sleep": *noAutoSleep,
	}
	var sb types.Sandbox
	if err := do(http.MethodPost, "/v1/sandboxes", body, &sb); err != nil {
		return err
	}
	fmt.Printf("%s\t%s\t%s\n", sb.ID, sb.State, sb.Name)
	return nil
}

func cmdList(args []string) error {
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
}

func cmdGet(args []string) error {
	if len(args) < 1 {
		return errors.New("get: sandbox required")
	}
	var sb types.Sandbox
	if err := getJSON("/v1/sandboxes/"+args[0], &sb); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(sb, "", "  ")
	fmt.Println(string(b))
	return nil
}

func cmdExec(args []string) error {
	opts, err := parseExecArgs(args)
	if err != nil {
		return err
	}
	if opts.Interactive {
		return runShell(opts.Ref, opts.Argv)
	}
	var resp struct {
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		Error    string `json:"error"`
	}
	err = do(http.MethodPost, "/v1/sandboxes/"+opts.Ref+"/exec", map[string]any{"argv": opts.Argv}, &resp)
	if err != nil {
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

func cmdPause(args []string) error {
	deep := false
	rest := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--deep" {
			deep = true
			continue
		}
		rest = append(rest, a)
	}
	if len(rest) < 1 {
		return errors.New("pause: sandbox required")
	}
	return do(http.MethodPost, "/v1/sandboxes/"+rest[0]+"/sleep", map[string]any{"deep": deep}, nil)
}

func cmdResume(args []string) error {
	if len(args) < 1 {
		return errors.New("resume: sandbox required")
	}
	return do(http.MethodPost, "/v1/sandboxes/"+args[0]+"/wake", nil, nil)
}

func cmdDelete(args []string) error {
	if len(args) < 1 {
		return errors.New("rm: sandbox required")
	}
	return do(http.MethodDelete, "/v1/sandboxes/"+args[0], nil, nil)
}

func cmdEvents(args []string) error {
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	n := fs.Int("n", 50, "number of events")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var events []types.Event
	if err := getJSON(fmt.Sprintf("/v1/events?limit=%d", *n), &events); err != nil {
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
}

// --- helpers ---------------------------------------------------------------

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func splitDashDash(args []string) (flags, cmd []string) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

func parsePublishes(vals []string) ([]types.PortMapping, error) {
	var out []types.PortMapping
	for _, v := range vals {
		proto := "tcp"
		if i := strings.LastIndexByte(v, '/'); i >= 0 {
			proto = v[i+1:]
			v = v[:i]
		}
		parts := strings.Split(v, ":")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid --publish %q (want host:guest)", v)
		}
		hp, err1 := strconv.Atoi(parts[0])
		gp, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil || hp < 1 || gp < 1 {
			return nil, fmt.Errorf("invalid --publish %q", v)
		}
		if proto != "tcp" && proto != "udp" {
			return nil, fmt.Errorf("invalid protocol %q", proto)
		}
		out = append(out, types.PortMapping{HostPort: hp, GuestPort: gp, Proto: proto})
	}
	return out, nil
}

func parseEnvs(vals []string) (map[string]string, error) {
	if len(vals) == 0 {
		return nil, nil
	}
	env := make(map[string]string, len(vals))
	for _, v := range vals {
		i := strings.IndexByte(v, '=')
		if i <= 0 {
			return nil, fmt.Errorf("invalid --env %q (want K=V)", v)
		}
		env[v[:i]] = v[i+1:]
	}
	return env, nil
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
