package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// process supervises one Firecracker VMM process. Its stdout/stderr are
// redirected to the VMM log file so boot failures can include a tail of the
// VMM's own diagnostics.
type process struct {
	cmd     *exec.Cmd
	logPath string
	logFile *os.File
	done    chan struct{}

	errMu sync.Mutex
	err   error
}

// waitLoop reaps the process and closes the log file.
func (p *process) waitLoop() {
	err := p.cmd.Wait()
	p.errMu.Lock()
	p.err = err
	p.errMu.Unlock()
	_ = p.logFile.Close()
	close(p.done)
}

func (p *process) pid() int { return p.cmd.Process.Pid }

func (p *process) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *process) waitErr() error {
	p.errMu.Lock()
	defer p.errMu.Unlock()
	return p.err
}

// wait blocks until the process exits or ctx is done.
func (p *process) wait(ctx context.Context) error {
	select {
	case <-p.done:
		if err := p.waitErr(); err != nil {
			return fmt.Errorf("firecracker: VMM exited: %w", err)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("firecracker: wait for VMM: %w", ctx.Err())
	}
}

// stop gracefully terminates the VMM: SIGTERM to its process group, then
// SIGKILL if it does not exit within timeout. It is idempotent.
func (p *process) stop(ctx context.Context, timeout time.Duration) error {
	if p.exited() {
		return nil
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	_ = syscall.Kill(-p.pid(), syscall.SIGTERM)
	if waitDone(p.done, timeout) {
		return nil
	}
	_ = syscall.Kill(-p.pid(), syscall.SIGKILL)
	if waitDone(p.done, timeout) {
		return nil
	}
	return fmt.Errorf("firecracker: VMM pid %d did not exit after SIGKILL within %s", p.pid(), timeout)
}

func waitDone(done <-chan struct{}, timeout time.Duration) bool {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

// spawn starts the VMM binary with a fresh API socket and VMM log. Stale socket
// files from a previous process are removed first (Firecracker refuses to bind
// over an existing socket).
func (d *Driver) spawn(apiSock, vmmLog, vsockUDSPath string) (*process, error) {
	if dir := filepath.Dir(apiSock); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("firecracker: create API socket dir: %w", err)
		}
	}
	if dir := filepath.Dir(vmmLog); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("firecracker: create log dir: %w", err)
		}
	}
	if err := os.Remove(apiSock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("firecracker: remove stale API socket %s: %w", apiSock, err)
	}
	if vsockUDSPath != "" {
		if err := os.Remove(vsockUDSPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("firecracker: remove stale vsock socket %s: %w", vsockUDSPath, err)
		}
	}

	logFile, err := os.OpenFile(vmmLog, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return nil, fmt.Errorf("firecracker: open VMM log: %w", err)
	}

	args := make([]string, 0, len(d.args)+2)
	args = append(args, d.args...)
	args = append(args, "--api-sock", apiSock)
	cmd := exec.Command(d.binary, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Own process group: Stop signals the whole group and a Ctrl-C in the
	// daemon's terminal does not kill VMs behind its back.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("firecracker: start %s: %w", d.binary, err)
	}
	p := &process{cmd: cmd, logPath: vmmLog, logFile: logFile, done: make(chan struct{})}
	go p.waitLoop()
	return p, nil
}

// logTail returns up to max bytes from the end of path, or "".
func logTail(path string, max int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return ""
	}
	offset := int64(0)
	if st.Size() > max {
		offset = st.Size() - max
	}
	if _, err := f.Seek(offset, 0); err != nil {
		return ""
	}
	buf := make([]byte, st.Size()-offset)
	n, _ := f.Read(buf)
	return string(buf[:n])
}
