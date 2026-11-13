package agent

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Shell frame types for the interactive shell stream. After the shell request
// is accepted, the connection switches from newline-delimited JSON to these
// binary frames (type byte + uint32 length + payload).
const (
	ShellFrameInput  = 'i' // client -> guest: raw terminal input
	ShellFrameResize = 'r' // client -> guest: JSON {"rows":N,"cols":N}
	ShellFrameOutput = 'o' // guest -> client: raw terminal output
	ShellFrameExit   = 'e' // guest -> client: exit code (big-endian uint32)
	ShellFrameError  = 'x' // guest -> client: error text
)

// ShellMaxFrame bounds a single shell frame payload.
const ShellMaxFrame = 256 << 10

// ShellRequest describes an interactive guest shell/command.
type ShellRequest struct {
	Argv []string          `json:"argv,omitempty"` // default: /bin/sh
	Env  map[string]string `json:"env,omitempty"`
	Rows uint16            `json:"rows,omitempty"`
	Cols uint16            `json:"cols,omitempty"`
}

// WriteShellFrame writes one framed shell message.
func WriteShellFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > ShellMaxFrame {
		return fmt.Errorf("agent: shell frame payload too large (%d bytes)", len(payload))
	}
	var hdr [5]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		_, err := w.Write(payload)
		return err
	}
	return nil
}

// ReadShellFrame reads one framed shell message.
func ReadShellFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > ShellMaxFrame {
		return 0, nil, fmt.Errorf("agent: shell frame payload too large (%d bytes)", n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return hdr[0], payload, nil
}

// ShellSession is a host-side interactive guest shell stream.
type ShellSession struct {
	conn net.Conn
	br   *bufio.Reader

	writeMu sync.Mutex
	out     []byte
	done    chan struct{}
	finish  sync.Once
	exit    atomic.Int32
	err     error
}

// OpenShell dials the guest agent over Firecracker's vsock UDS, requests a PTY
// shell and returns a duplex session. The caller must Close it.
func OpenShell(ctx context.Context, vsockUDSPath string, guestCID uint32, req ShellRequest) (*ShellSession, error) {
	if vsockUDSPath == "" {
		return nil, errors.New("agent: empty vsock UDS path")
	}
	if len(req.Argv) == 0 {
		req.Argv = []string{"/bin/sh"}
	}
	if req.Rows == 0 || req.Cols == 0 {
		req.Rows, req.Cols = 24, 80
	}

	dialCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
	}
	var d net.Dialer
	nc, err := d.DialContext(dialCtx, "unix", vsockUDSPath)
	if err != nil {
		return nil, fmt.Errorf("agent: shell connect %s: %w", vsockUDSPath, err)
	}
	if dl, ok := dialCtx.Deadline(); ok {
		_ = nc.SetDeadline(dl)
	}
	if _, err := io.WriteString(nc, fmt.Sprintf("CONNECT %d\n", VsockPort)); err != nil {
		_ = nc.Close()
		return nil, fmt.Errorf("agent: shell CONNECT: %w", err)
	}
	br := bufio.NewReader(nc)
	line, err := br.ReadString('\n')
	if err != nil {
		_ = nc.Close()
		return nil, fmt.Errorf("agent: shell CONNECT reply: %w", err)
	}
	if reply := string(line); len(reply) < 2 || reply[:2] != "OK" {
		_ = nc.Close()
		return nil, fmt.Errorf("agent: shell connect rejected: %q", reply)
	}

	id := fmt.Sprintf("shell-%d", time.Now().UnixNano())
	reqLine, err := WriteLine(Request{
		ID: id, Op: OpShell, Argv: req.Argv, Env: req.Env, Rows: req.Rows, Cols: req.Cols,
	})
	if err != nil {
		_ = nc.Close()
		return nil, err
	}
	if _, err := nc.Write(reqLine); err != nil {
		_ = nc.Close()
		return nil, fmt.Errorf("agent: shell request: %w", err)
	}

	// The agent sends an async ready event first; wait for our shell_started
	// acknowledgement (or an error response with our request ID).
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			_ = nc.Close()
			return nil, fmt.Errorf("agent: shell handshake: %w", err)
		}
		var hdr struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Error    string `json:"error"`
			ExitCode int    `json:"exit_code"`
		}
		if err := json.Unmarshal(line, &hdr); err != nil {
			_ = nc.Close()
			return nil, fmt.Errorf("agent: shell handshake: %w", err)
		}
		if hdr.Type == "shell_started" {
			break
		}
		if hdr.ID == id && hdr.Error != "" {
			_ = nc.Close()
			return nil, fmt.Errorf("agent: shell: %s", hdr.Error)
		}
		// Ignore ready/heartbeat/workload events.
	}
	_ = nc.SetDeadline(time.Time{})

	return &ShellSession{conn: nc, br: br, done: make(chan struct{})}, nil
}

// Read returns PTY output. It returns io.EOF after the guest shell exits or the
// connection closes; ExitCode is valid then.
func (s *ShellSession) Read(p []byte) (int, error) {
	for len(s.out) == 0 {
		typ, payload, err := ReadShellFrame(s.br)
		if err != nil {
			s.finishWith(-1, err)
			return 0, err
		}
		switch typ {
		case ShellFrameOutput:
			s.out = payload
		case ShellFrameExit:
			code := -1
			if len(payload) >= 4 {
				code = int(int32(binary.BigEndian.Uint32(payload)))
			}
			s.finishWith(code, nil)
			return 0, io.EOF
		case ShellFrameError:
			err := fmt.Errorf("agent: shell: %s", payload)
			s.finishWith(-1, err)
			return 0, err
		default:
			// Ignore unknown frames (forward compatibility).
		}
	}
	n := copy(p, s.out)
	s.out = s.out[n:]
	return n, nil
}

// Write sends raw terminal input to the guest.
func (s *ShellSession) Write(p []byte) (int, error) {
	select {
	case <-s.done:
		return 0, io.ErrClosedPipe
	default:
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := WriteShellFrame(s.conn, ShellFrameInput, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Resize tells the guest PTY about a terminal resize.
func (s *ShellSession) Resize(rows, cols uint16) error {
	payload, err := json.Marshal(struct {
		Rows uint16 `json:"rows"`
		Cols uint16 `json:"cols"`
	}{rows, cols})
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return WriteShellFrame(s.conn, ShellFrameResize, payload)
}

// Done is closed when the guest shell exits or the session fails.
func (s *ShellSession) Done() <-chan struct{} { return s.done }

// ExitCode reports the guest shell exit status after Done is closed.
func (s *ShellSession) ExitCode() int { return int(s.exit.Load()) }

// Err reports the terminal session error, if any.
func (s *ShellSession) Err() error { return s.err }

// Close closes the vsock connection. It is safe to call multiple times.
func (s *ShellSession) Close() error {
	s.finishWith(-1, io.ErrClosedPipe)
	return s.conn.Close()
}

func (s *ShellSession) finishWith(code int, err error) {
	s.finish.Do(func() {
		s.exit.Store(int32(code))
		s.err = err
		close(s.done)
	})
}
