package agent

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

// serveShell runs an interactive PTY command on a dedicated agent connection.
// After the shell_started event the connection carries binary shell frames.
func (s *Server) serveShell(ctx context.Context, c *conn, req Request) {
	arg := req.Argv
	if len(arg) == 0 {
		arg = []string{"/bin/sh"}
	}
	master, slave, err := openPTY()
	if err != nil {
		_ = c.send(Response{ID: req.ID, ExitCode: -1, Error: err.Error()})
		return
	}
	defer master.Close()
	if err := setPTYWinsize(master, req.Rows, req.Cols); err != nil {
		_ = slave.Close()
		_ = c.send(Response{ID: req.ID, ExitCode: -1, Error: fmt.Sprintf("set pty size: %v", err)})
		return
	}

	cmd := exec.Command(arg[0], arg[1:]...)
	cmd.Env = mergeEnv(req.Env)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		_ = slave.Close()
		_ = c.send(Response{ID: req.ID, ExitCode: -1, Error: fmt.Sprintf("start shell: %v", err)})
		return
	}
	_ = slave.Close()

	if err := c.send(Event{Type: "shell_started", Message: "pty"}); err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		return
	}

	// PTY output -> frames.
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		buf := make([]byte, 32<<10)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				if werr := WriteShellFrame(c.nc, ShellFrameOutput, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Client frames (input/resize) -> PTY.
	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		br := bufio.NewReader(c.nc)
		for {
			typ, payload, err := ReadShellFrame(br)
			if err != nil {
				return
			}
			switch typ {
			case ShellFrameInput:
				if _, err := master.Write(payload); err != nil {
					return
				}
			case ShellFrameResize:
				var sz struct {
					Rows uint16 `json:"rows"`
					Cols uint16 `json:"cols"`
				}
				if err := json.Unmarshal(payload, &sz); err == nil {
					_ = setPTYWinsize(master, sz.Rows, sz.Cols)
				}
			}
		}
	}()

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	var waitErr error
	select {
	case waitErr = <-waitCh:
		// Let the output goroutine drain the PTY before closing the master.
		<-outDone
	case <-ctx.Done():
		_ = master.Close()
		waitErr = <-waitCh
		<-outDone
	case <-inputDone:
		// Client disconnected: hang up the shell and reap it.
		_ = master.Close()
		select {
		case waitErr = <-waitCh:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			waitErr = <-waitCh
		}
		<-outDone
	}
	_ = master.Close()

	code := exitCode(waitErr)
	var payload [4]byte
	binary.BigEndian.PutUint32(payload[:], uint32(int32(code)))
	_ = WriteShellFrame(c.nc, ShellFrameExit, payload[:])
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}
