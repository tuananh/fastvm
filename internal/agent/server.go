package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// DefaultWorkloadLog is where detached workload output is appended inside
	// the guest. It falls back to the serial console when the path is not
	// writable.
	DefaultWorkloadLog = "/var/log/fastvm/workload.log"

	// defaultExecTimeout bounds a synchronous exec request without an explicit
	// timeout.
	defaultExecTimeout = 30 * time.Second
	// maxRequestLine is the largest accepted request line (JSON). It bounds
	// stdin payloads and argv; responses are bounded separately by maxCapture.
	maxRequestLine = 4 << 20
	// maxCapture bounds each captured stdout/stderr buffer in a Response.
	maxCapture = 1 << 20
)

// Workload is the optional guest workload started by the agent, normally read
// from /etc/fastvm/workload.json (docs/DESIGN.md §6.2).
type Workload struct {
	Argv []string          `json:"argv"`
	Env  map[string]string `json:"env,omitempty"`
	Dir  string            `json:"dir,omitempty"`
}

// LoadWorkload reads a workload JSON file. A missing file returns (nil, nil).
func LoadWorkload(path string) (*Workload, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("agent: read workload config %s: %w", path, err)
	}
	var w Workload
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, fmt.Errorf("agent: parse workload config %s: %w", path, err)
	}
	if len(w.Argv) == 0 {
		return nil, fmt.Errorf("agent: workload config %s: argv must not be empty", path)
	}
	return &w, nil
}

// Server is the guest-side agent: it serves the newline-delimited JSON protocol
// on a vsock listener and owns the optional sandbox workload.
type Server struct {
	workloadLog string

	mu       sync.Mutex
	conns    map[*conn]struct{}
	workload *workloadState
	busy     bool
}

type workloadState struct {
	argv     []string
	cmd      *exec.Cmd
	running  bool
	exitCode int
}

// NewServer returns a Server. workloadLogPath overrides DefaultWorkloadLog when
// non-empty.
func NewServer(workloadLogPath string) *Server {
	if workloadLogPath == "" {
		workloadLogPath = DefaultWorkloadLog
	}
	return &Server{workloadLog: workloadLogPath, conns: make(map[*conn]struct{})}
}

// Serve accepts agent connections on ln until ctx is canceled. It returns nil
// on graceful shutdown.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = ln.Close()
			s.closeAll()
		case <-done:
		}
	}()
	defer close(done)

	var wg sync.WaitGroup
	for {
		nc, err := ln.Accept()
		if err != nil {
			wg.Wait()
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("agent: accept: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handleConn(ctx, nc)
		}()
	}
}

// StartWorkload starts the sandbox workload detached from any client
// connection. Events are broadcast to connected clients and replayed to new
// ones.
func (s *Server) StartWorkload(w Workload) error { return s.startWorkload(w) }

// Busy reports the last workload_busy advisory value.
func (s *Server) Busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy
}

func (s *Server) handleConn(ctx context.Context, nc net.Conn) {
	c := &conn{s: s, nc: nc, bw: bufio.NewWriter(nc)}
	s.addConn(c)
	defer func() {
		s.removeConn(c)
		_ = nc.Close()
	}()

	_ = c.send(Event{Type: EventReady, Message: "fastvm-agent"})
	s.replayState(c)

	sc := bufio.NewScanner(nc)
	sc.Buffer(make([]byte, 64*1024), maxRequestLine)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			log.Printf("agent: dropping malformed request: %v", err)
			continue
		}
		if req.Op == OpShell {
			// The shell owns this connection from here on. Remove it from the
			// broadcast set: async JSON events would corrupt the frame stream.
			s.removeConn(c)
			s.serveShell(ctx, c, req)
			return
		}
		go s.dispatch(ctx, c, req)
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil && !errors.Is(err, io.EOF) {
		log.Printf("agent: connection read error: %v", err)
	}
}

func (s *Server) dispatch(ctx context.Context, c *conn, req Request) {
	resp := Response{ID: req.ID}
	switch req.Op {
	case OpExec:
		if req.Detach {
			if err := s.startWorkload(Workload{Argv: req.Argv, Env: req.Env}); err != nil {
				resp.ExitCode = -1
				resp.Error = err.Error()
			}
		} else {
			resp = s.exec(ctx, req)
		}
	case OpSignal:
		if err := s.signal(req.Signal); err != nil {
			resp.ExitCode = -1
			resp.Error = err.Error()
		}
	case OpTouch:
		s.broadcast(Event{Type: EventTouch})
	case OpBusy:
		s.mu.Lock()
		s.busy = req.Busy
		s.mu.Unlock()
		s.broadcast(Event{Type: EventWorkloadBusy, Message: fmt.Sprintf("busy=%t", req.Busy)})
	case OpHeartbeat, OpReady:
		// No-op acknowledgement.
	default:
		resp.ExitCode = -1
		resp.Error = fmt.Sprintf("unknown op %q", req.Op)
	}
	if err := c.send(resp); err != nil {
		log.Printf("agent: send response: %v", err)
	}
}

func (s *Server) exec(ctx context.Context, req Request) Response {
	resp := Response{ID: req.ID}
	if len(req.Argv) == 0 {
		resp.ExitCode = -1
		resp.Error = "argv must not be empty"
		return resp
	}
	timeout := defaultExecTimeout
	if req.TimeoutMS > 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, req.Argv[0], req.Argv[1:]...)
	cmd.Env = mergeEnv(req.Env)
	if req.Stdin != "" {
		cmd.Stdin = strings.NewReader(req.Stdin)
	}
	var stdout, stderr cappedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	resp.Stdout = stdout.String()
	resp.Stderr = stderr.String()
	switch {
	case err == nil:
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		resp.ExitCode = -1
		resp.Error = fmt.Sprintf("command timed out after %s", timeout)
	case errors.Is(runCtx.Err(), context.Canceled):
		resp.ExitCode = -1
		resp.Error = "command canceled"
	default:
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			resp.ExitCode = exitErr.ExitCode()
		} else {
			resp.ExitCode = -1
			resp.Error = err.Error()
		}
	}
	return resp
}

func (s *Server) startWorkload(w Workload) error {
	if len(w.Argv) == 0 {
		return errors.New("workload argv must not be empty")
	}
	s.mu.Lock()
	if s.workload != nil && s.workload.running {
		s.mu.Unlock()
		return errors.New("workload already running")
	}
	s.mu.Unlock()

	out, err := s.openWorkloadLog()
	if err != nil {
		log.Printf("agent: workload log %s unavailable (%v); using serial console", s.workloadLog, err)
		out = nil
	}
	cmd := exec.Command(w.Argv[0], w.Argv[1:]...)
	cmd.Env = mergeEnv(w.Env)
	cmd.Dir = w.Dir
	if out != nil {
		cmd.Stdout = out
		cmd.Stderr = out
	} else {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		if out != nil {
			_ = out.Close()
		}
		return fmt.Errorf("start workload %q: %w", w.Argv[0], err)
	}

	st := &workloadState{argv: w.Argv, cmd: cmd, running: true}
	s.mu.Lock()
	s.workload = st
	s.mu.Unlock()

	log.Printf("agent: workload started pid=%d argv=%v", cmd.Process.Pid, w.Argv)
	s.broadcast(Event{Type: EventWorkloadStarted, Message: strings.Join(w.Argv, " ")})

	go func() {
		waitErr := cmd.Wait()
		code := 0
		if waitErr != nil {
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) {
				code = exitErr.ExitCode()
			} else {
				code = -1
				log.Printf("agent: workload wait: %v", waitErr)
			}
		}
		s.mu.Lock()
		st.running = false
		st.exitCode = code
		s.mu.Unlock()
		if out != nil {
			_ = out.Close()
		}
		log.Printf("agent: workload exited code=%d argv=%v", code, st.argv)
		s.broadcast(Event{Type: EventWorkloadExit, Code: code, Message: strings.Join(st.argv, " ")})
	}()
	return nil
}

func (s *Server) signal(name string) error {
	sig, err := parseSignal(name)
	if err != nil {
		return err
	}
	s.mu.Lock()
	st := s.workload
	running := st != nil && st.running && st.cmd != nil && st.cmd.Process != nil
	pid := 0
	if running {
		pid = st.cmd.Process.Pid
	}
	s.mu.Unlock()
	if !running {
		return errors.New("no running workload to signal")
	}
	// The workload runs in its own process group so a shell's children receive
	// the signal too.
	if err := syscall.Kill(-pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal workload (pgid %d): %w", pid, err)
	}
	return nil
}

func (s *Server) replayState(c *conn) {
	s.mu.Lock()
	st := s.workload
	busy := s.busy
	var argv []string
	running, exitCode := false, 0
	if st != nil {
		argv, running, exitCode = st.argv, st.running, st.exitCode
	}
	s.mu.Unlock()
	if st != nil {
		if running {
			_ = c.send(Event{Type: EventWorkloadStarted, Message: strings.Join(argv, " ")})
		} else {
			_ = c.send(Event{Type: EventWorkloadExit, Code: exitCode, Message: strings.Join(argv, " ")})
		}
	}
	if busy {
		_ = c.send(Event{Type: EventWorkloadBusy, Message: "busy=true"})
	}
}

func (s *Server) openWorkloadLog() (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(s.workloadLog), 0o750); err != nil {
		return nil, err
	}
	return os.OpenFile(s.workloadLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
}

func (s *Server) broadcast(ev Event) {
	s.mu.Lock()
	conns := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		if err := c.send(ev); err != nil {
			log.Printf("agent: broadcast: %v", err)
		}
	}
}

func (s *Server) addConn(c *conn) {
	s.mu.Lock()
	s.conns[c] = struct{}{}
	s.mu.Unlock()
}

func (s *Server) removeConn(c *conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

func (s *Server) closeAll() {
	s.mu.Lock()
	conns := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.nc.Close()
	}
}

type conn struct {
	s  *Server
	nc net.Conn
	mu sync.Mutex
	bw *bufio.Writer
}

func (c *conn) send(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	line, err := WriteLine(v)
	if err != nil {
		return err
	}
	if _, err := c.bw.Write(line); err != nil {
		return err
	}
	return c.bw.Flush()
}

// cappedBuffer collects output up to maxCapture bytes and notes truncation.
type cappedBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if remain := maxCapture - b.buf.Len(); remain > 0 {
		if len(p) > remain {
			p = p[:remain]
			b.truncated = true
		}
		_, _ = b.buf.Write(p)
	} else {
		b.truncated = true
	}
	return n, nil
}

func (b *cappedBuffer) String() string {
	s := b.buf.String()
	if b.truncated {
		s += "\n[output truncated by fastvm-agent]"
	}
	return s
}

// mergeEnv returns os.Environ with overrides applied.
func mergeEnv(overrides map[string]string) []string {
	env := os.Environ()
	index := make(map[string]int, len(env))
	for i, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok {
			index[k] = i
		}
	}
	for k, v := range overrides {
		kv := k + "=" + v
		if i, ok := index[k]; ok {
			env[i] = kv
		} else {
			env = append(env, kv)
		}
	}
	return env
}

func parseSignal(name string) (syscall.Signal, error) {
	n := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(name)), "SIG")
	sigs := map[string]syscall.Signal{
		"HUP":    syscall.SIGHUP,
		"INT":    syscall.SIGINT,
		"QUIT":   syscall.SIGQUIT,
		"ILL":    syscall.SIGILL,
		"TRAP":   syscall.SIGTRAP,
		"ABRT":   syscall.SIGABRT,
		"BUS":    syscall.SIGBUS,
		"FPE":    syscall.SIGFPE,
		"KILL":   syscall.SIGKILL,
		"USR1":   syscall.SIGUSR1,
		"SEGV":   syscall.SIGSEGV,
		"USR2":   syscall.SIGUSR2,
		"PIPE":   syscall.SIGPIPE,
		"ALRM":   syscall.SIGALRM,
		"TERM":   syscall.SIGTERM,
		"CHLD":   syscall.SIGCHLD,
		"CONT":   syscall.SIGCONT,
		"STOP":   syscall.SIGSTOP,
		"TSTP":   syscall.SIGTSTP,
		"TTIN":   syscall.SIGTTIN,
		"TTOU":   syscall.SIGTTOU,
		"URG":    syscall.SIGURG,
		"XCPU":   syscall.SIGXCPU,
		"XFSZ":   syscall.SIGXFSZ,
		"VTALRM": syscall.SIGVTALRM,
		"PROF":   syscall.SIGPROF,
		"WINCH":  syscall.SIGWINCH,
		"IO":     syscall.SIGIO,
		"SYS":    syscall.SIGSYS,
	}
	if sig, ok := sigs[n]; ok {
		return sig, nil
	}
	return 0, fmt.Errorf("unknown signal %q", name)
}
