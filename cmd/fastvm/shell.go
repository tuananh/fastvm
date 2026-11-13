package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"fastvm/internal/agent"
)

// cmdShell is shorthand for `exec -it <sandbox> -- /bin/sh`.
func cmdShell(args []string) error {
	if len(args) == 0 {
		return errors.New("shell: sandbox required")
	}
	ref := args[0]
	argv := []string{"/bin/sh"}
	if rest := args[1:]; len(rest) > 0 {
		if rest[0] == "--" {
			rest = rest[1:]
		}
		if len(rest) > 0 {
			argv = rest
		}
	}
	return runShell(ref, argv)
}

// runShell opens an interactive PTY session in the guest over the daemon's
// HTTP-upgrade shell endpoint.
func runShell(ref string, argv []string) error {
	if len(argv) == 0 {
		argv = []string{"/bin/sh"}
	}
	rows, cols := uint16(24), uint16(80)
	stdinFd := int(os.Stdin.Fd())
	termios, _ := unix.IoctlGetTermios(stdinFd, unix.TCGETS)
	interactive := termios != nil
	if interactive {
		if ws, err := unix.IoctlGetWinsize(stdinFd, unix.TIOCGWINSZ); err == nil && ws.Row > 0 && ws.Col > 0 {
			rows, cols = ws.Row, ws.Col
		}
		if raw, err := makeRaw(stdinFd); err == nil {
			defer func() { _ = unix.IoctlSetTermios(stdinFd, unix.TCSETS, raw) }()
		}
	}

	env := map[string]string{}
	if term := os.Getenv("TERM"); term != "" {
		env["TERM"] = term
	}
	payload, err := json.Marshal(agent.ShellRequest{Argv: argv, Env: env, Rows: rows, Cols: cols})
	if err != nil {
		return err
	}
	conn, err := net.Dial("unix", socketPath())
	if err != nil {
		return fmt.Errorf("shell: connect daemon: %w", err)
	}
	defer conn.Close()

	fmt.Fprintf(conn, "POST /v1/sandboxes/%s/shell HTTP/1.1\r\n"+
		"Host: fastvm\r\n"+
		"Connection: Upgrade\r\n"+
		"Upgrade: fastvm-shell\r\n"+
		"Content-Type: application/json\r\n"+
		"Content-Length: %d\r\n\r\n", url.PathEscape(ref), len(payload))
	if _, err := conn.Write(payload); err != nil {
		return fmt.Errorf("shell: send request: %w", err)
	}

	br := bufio.NewReader(conn)
	// Bound the upgrade handshake so a stale daemon/image cannot hang the CLI.
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return fmt.Errorf("shell: read response: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		return fmt.Errorf("shell: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	_ = conn.SetReadDeadline(time.Time{})

	var writeMu sync.Mutex
	writeFrame := func(typ byte, data []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return agent.WriteShellFrame(conn, typ, data)
	}

	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				if werr := writeFrame(agent.ShellFrameInput, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				// Send an EOF (Ctrl-D) to the guest PTY instead of tearing the
				// shell down, so a command with closed stdin can still run to
				// completion and return its output.
				_ = writeFrame(agent.ShellFrameInput, []byte{0x04})
				return
			}
		}
	}()

	if interactive {
		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		go func() {
			for range winch {
				if ws, err := unix.IoctlGetWinsize(stdinFd, unix.TIOCGWINSZ); err == nil && ws.Row > 0 && ws.Col > 0 {
					data, _ := json.Marshal(struct {
						Rows uint16 `json:"rows"`
						Cols uint16 `json:"cols"`
					}{ws.Row, ws.Col})
					_ = writeFrame(agent.ShellFrameResize, data)
				}
			}
		}()
	}

	for {
		typ, data, err := agent.ReadShellFrame(br)
		if err != nil {
			return fmt.Errorf("shell: connection closed: %w", err)
		}
		switch typ {
		case agent.ShellFrameOutput:
			if _, err := os.Stdout.Write(data); err != nil {
				return err
			}
		case agent.ShellFrameExit:
			code := int(int32(binary.BigEndian.Uint32(data)))
			if code != 0 {
				return fmt.Errorf("shell exited with code %d", code)
			}
			return nil
		case agent.ShellFrameError:
			return fmt.Errorf("shell: %s", strings.TrimSpace(string(data)))
		}
	}
}

// makeRaw puts fd into raw mode and returns the previous settings.
func makeRaw(fd int) (*unix.Termios, error) {
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, err
	}
	raw := *old
	raw.Iflag &^= unix.BRKINT | unix.ICRNL | unix.INPCK | unix.ISTRIP | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Cflag |= unix.CS8
	raw.Lflag &^= unix.ECHO | unix.ICANON | unix.IEXTEN | unix.ISIG
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &raw); err != nil {
		return nil, err
	}
	return old, nil
}
