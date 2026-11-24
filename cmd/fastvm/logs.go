package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"fastvm/internal/types"
)

const (
	guestStdoutLog = "/var/log/fastvm/stdout.log"
	guestStderrLog = "/var/log/fastvm/stderr.log"
)

var logSources = []string{"stdout", "stderr"}

func newLogsCmd() *cobra.Command {
	var sources []string
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs <sandbox>",
		Short: "Show captured sandbox logs",
		Example: `  fastvmctl logs dev
  fastvmctl logs --source console dev
  fastvmctl logs --source stdout,stderr -f dev`,
		Long: `Print logs captured for a sandbox.

Sources:
  stdout    non-interactive session stdout
  stderr    non-interactive session stderr
  console   raw serial console: boot, kernel, init, agent diagnostics
  system    daemon lifecycle events

PTY sessions are deliberately not persisted: a PTY merges stdout and stderr
and echoes terminal input, so capturing it would mix user keystrokes into logs.
Use the live shell for terminal output.

Use --follow to stream new output.`,
		Args: exactArgsWithHelp(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogs(cmd.Context(), args[0], sources, follow)
		},
	}
	cmd.Flags().StringSliceVar(&sources, "source", logSources, "comma-separated sources: stdout,stderr,console,system")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "stream new log output")
	return cmd
}

func runLogs(ctx context.Context, ref string, sources []string, follow bool) error {
	seen := make(map[string]bool)
	offsets := make(map[string]int64)
	first := true

	for {
		for _, source := range sources {
			data, err := fetchLogSource(ctx, ref, source, seen)
			if err != nil {
				return err
			}
			if source == "system" {
				// System output is already deduplicated by event key.
				if len(data) > 0 {
					if first && len(sources) > 1 {
						fmt.Fprintf(os.Stdout, "==> %s <==\n", source)
					}
					_, _ = os.Stdout.Write(data)
				}
				continue
			}
			off := offsets[source]
			if len(data) < int(off) {
				off = 0 // file was rotated or truncated
			}
			if len(data) > int(off) {
				if first && len(sources) > 1 {
					fmt.Fprintf(os.Stdout, "==> %s <==\n", source)
				}
				if _, err := os.Stdout.Write(data[off:]); err != nil {
					return err
				}
				offsets[source] = int64(len(data))
			}
		}
		first = false
		if !follow {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func fetchLogSource(ctx context.Context, ref, source string, seen map[string]bool) ([]byte, error) {
	switch source {
	case "stdout":
		return fetchGuestLog(ctx, ref, guestStdoutLog)
	case "stderr":
		return fetchGuestLog(ctx, ref, guestStderrLog)
	case "console":
		return fetchConsoleLog(ctx, ref)
	case "system":
		return fetchSystemLog(ctx, ref, seen)
	case "output":
		return nil, errors.New("logs: PTY output is not persisted because it contains terminal input echo")
	default:
		return nil, fmt.Errorf("logs: unknown source %q", source)
	}
}

func fetchGuestLog(ctx context.Context, ref, path string) ([]byte, error) {
	q := url.Values{}
	q.Set("path", path)
	resp, err := rawAPIRequest(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(ref)+"/file?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode >= 300 {
		err := copyResponseError(resp)
		if strings.Contains(strings.ToLower(err.Error()), "no such file or directory") {
			return nil, nil
		}
		return nil, err
	}
	return io.ReadAll(resp.Body)
}

func fetchConsoleLog(ctx context.Context, ref string) ([]byte, error) {
	resp, err := rawAPIRequest(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(ref)+"/console", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode >= 300 {
		return nil, copyResponseError(resp)
	}
	return io.ReadAll(resp.Body)
}

func fetchSystemLog(ctx context.Context, ref string, seen map[string]bool) ([]byte, error) {
	var sb types.Sandbox
	if err := getJSON("/v1/sandboxes/"+url.PathEscape(ref), &sb); err != nil {
		return nil, err
	}
	var events []types.Event
	if err := getJSON("/v1/events?limit=1000", &events); err != nil {
		return nil, err
	}
	var out strings.Builder
	for _, ev := range events {
		if ev.SandboxID != sb.ID {
			continue
		}
		key := ev.Time.String() + "\x00" + ev.Type + "\x00" + ev.Message
		if seen[key] {
			continue
		}
		seen[key] = true
		fmt.Fprintf(&out, "%s %-7s %s\n", ev.Time.Format(time.RFC3339), ev.Type, ev.Message)
	}
	return []byte(out.String()), nil
}
