package agent

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// agentFixture runs a Server on a Unix socket for tests (the real guest uses a
// vsock listener; the JSON protocol is identical).
type agentFixture struct {
	sock string
	srv  *Server
}

func startAgentServer(t *testing.T) *agentFixture {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "agent.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := NewServer(filepath.Join(t.TempDir(), "workload.log"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("agent server did not stop")
		}
	})
	return &agentFixture{sock: sock, srv: srv}
}

// vsockProxy emulates Firecracker's host-side vsock UDS bridge: the host client
// connects and sends "CONNECT <port>", the proxy acknowledges "OK <port>" and
// pipes the stream to the guest listener.
type vsockProxy struct {
	sock string
	ln   net.Listener

	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

func startVsockProxy(t *testing.T, guestSock string) *vsockProxy {
	t.Helper()
	p := &vsockProxy{sock: filepath.Join(t.TempDir(), "vsock.sock"), conns: make(map[net.Conn]struct{})}
	ln, err := net.Listen("unix", p.sock)
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p.ln = ln
	t.Cleanup(func() {
		_ = ln.Close()
		p.mu.Lock()
		for c := range p.conns {
			_ = c.Close()
		}
		p.mu.Unlock()
	})
	go func() {
		for {
			host, err := ln.Accept()
			if err != nil {
				return
			}
			go p.handle(host, guestSock)
		}
	}()
	return p
}

func (p *vsockProxy) handle(host net.Conn, guestSock string) {
	p.track(host)
	defer p.untrack(host)
	defer host.Close()

	br := bufio.NewReader(host)
	_ = host.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := br.ReadString('\n')
	if err != nil || !strings.HasPrefix(strings.TrimSpace(line), "CONNECT ") {
		return
	}
	_ = host.SetReadDeadline(time.Time{})
	guest, err := net.Dial("unix", guestSock)
	if err != nil {
		return
	}
	p.track(guest)
	defer p.untrack(guest)
	defer guest.Close()

	if _, err := io.WriteString(host, "OK 1024\n"); err != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(guest, br)
		if cw, ok := guest.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		close(done)
	}()
	_, _ = io.Copy(host, guest)
	<-done
}

func (p *vsockProxy) track(c net.Conn) {
	p.mu.Lock()
	p.conns[c] = struct{}{}
	p.mu.Unlock()
}

func (p *vsockProxy) untrack(c net.Conn) {
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
}

// reset drops all currently established proxy connections, simulating a vsock
// reset after a Firecracker snapshot restore.
func (p *vsockProxy) reset() {
	p.mu.Lock()
	conns := make([]net.Conn, 0, len(p.conns))
	for c := range p.conns {
		conns = append(conns, c)
	}
	p.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

func dialTestClient(t *testing.T, guestSock string) Client {
	t.Helper()
	proxy := startVsockProxy(t, guestSock)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cl, err := DialFirecracker(ctx, proxy.sock, 3, 5*time.Second)
	if err != nil {
		t.Fatalf("DialFirecracker: %v", err)
	}
	if err := cl.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	return cl
}

func TestClientExec(t *testing.T) {
	f := startAgentServer(t)
	cl := dialTestClient(t, f.sock)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := cl.Exec(ctx, Request{Argv: []string{"/bin/sh", "-c", "printf 'hello %s' \"$NAME\""}, Env: map[string]string{"NAME": "fastvm"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if resp.ExitCode != 0 || resp.Stdout != "hello fastvm" || resp.Error != "" {
		t.Fatalf("unexpected response: %+v", resp)
	}

	resp, err = cl.Exec(ctx, Request{Argv: []string{"/bin/sh", "-c", "echo out; echo err >&2; exit 7"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if resp.ExitCode != 7 || resp.Stdout != "out\n" || resp.Stderr != "err\n" {
		t.Fatalf("unexpected response for exit 7: %+v", resp)
	}

	resp, err = cl.Exec(ctx, Request{Argv: []string{"/bin/cat"}, Stdin: "piped"})
	if err != nil {
		t.Fatalf("Exec stdin: %v", err)
	}
	if resp.Stdout != "piped" {
		t.Fatalf("stdin not delivered: %+v", resp)
	}
}

func TestClientExecTimeout(t *testing.T) {
	f := startAgentServer(t)
	cl := dialTestClient(t, f.sock)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	resp, err := cl.Exec(ctx, Request{Argv: []string{"/bin/sleep", "10"}, TimeoutMS: 100})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if resp.ExitCode == 0 || !strings.Contains(resp.Error, "timed out") {
		t.Fatalf("expected timeout response, got %+v", resp)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout took too long: %s", elapsed)
	}
}

func TestClientDetachedWorkloadEvents(t *testing.T) {
	f := startAgentServer(t)
	cl := dialTestClient(t, f.sock)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	resp, err := cl.Exec(ctx, Request{Detach: true, Argv: []string{"/bin/sh", "-c", "sleep 0.2; exit 3"}})
	if err != nil {
		t.Fatalf("detached Exec: %v", err)
	}
	if resp.Error != "" {
		t.Fatalf("detached Exec error: %+v", resp)
	}

	deadline := time.After(10 * time.Second)
	var started, exited bool
	var exitCode int
	for !exited {
		select {
		case ev := <-cl.Events():
			switch ev.Type {
			case EventWorkloadStarted:
				started = true
			case EventWorkloadExit:
				exited = true
				exitCode = ev.Code
			}
		case <-deadline:
			t.Fatalf("workload events not received (started=%v)", started)
		}
	}
	if !started {
		t.Fatal("workload_started event not received")
	}
	if exitCode != 3 {
		t.Fatalf("workload_exit code = %d, want 3", exitCode)
	}
}

func TestClientReconnectAfterReset(t *testing.T) {
	f := startAgentServer(t)
	proxy := startVsockProxy(t, f.sock)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cl, err := DialFirecracker(ctx, proxy.sock, 3, 5*time.Second)
	if err != nil {
		t.Fatalf("DialFirecracker: %v", err)
	}
	defer cl.Close()
	if err := cl.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	resp, err := cl.Exec(ctx, Request{Argv: []string{"/bin/echo", "first"}})
	if err != nil || resp.Stdout != "first\n" {
		t.Fatalf("first exec: resp=%+v err=%v", resp, err)
	}

	proxy.reset()
	// Lazy reconnect on the next request.
	resp, err = cl.Exec(ctx, Request{Argv: []string{"/bin/echo", "second"}})
	if err != nil {
		t.Fatalf("exec after vsock reset: %v", err)
	}
	if resp.Stdout != "second\n" {
		t.Fatalf("unexpected exec output after reset: %+v", resp)
	}
}

func TestClientTouchAndBusy(t *testing.T) {
	f := startAgentServer(t)
	cl := dialTestClient(t, f.sock)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := cl.Touch(ctx); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	if _, err := cl.Exec(ctx, Request{Op: OpBusy, Busy: true}); err != nil {
		t.Fatalf("busy: %v", err)
	}
	if !f.srv.Busy() {
		t.Fatal("server did not record busy advisory")
	}

	waitEvent := func(want string) Event {
		t.Helper()
		for {
			select {
			case ev := <-cl.Events():
				if ev.Type == want {
					return ev
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("event %q not received", want)
			}
		}
	}
	waitEvent(EventTouch)
	ev := waitEvent(EventWorkloadBusy)
	if !strings.Contains(ev.Message, "busy=true") {
		t.Fatalf("unexpected busy event: %+v", ev)
	}
}

func TestClientClosedErrors(t *testing.T) {
	f := startAgentServer(t)
	cl := dialTestClient(t, f.sock)
	if err := cl.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := cl.Exec(context.Background(), Request{Argv: []string{"/bin/true"}}); err == nil {
		t.Fatal("Exec after Close should fail")
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-cl.Events():
			if !ok {
				return
			}
			// Drain any ready/workload events queued before Close.
		case <-deadline:
			t.Fatal("events channel was not closed")
		}
	}
}

func TestLoadWorkload(t *testing.T) {
	dir := t.TempDir()
	if w, err := LoadWorkload(filepath.Join(dir, "missing.json")); err != nil || w != nil {
		t.Fatalf("missing config: w=%v err=%v", w, err)
	}

	good := filepath.Join(dir, "good.json")
	if err := os.WriteFile(good, []byte(`{"argv":["nginx","-g","daemon off;"],"env":{"A":"b"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := LoadWorkload(good)
	if err != nil {
		t.Fatalf("LoadWorkload: %v", err)
	}
	if len(w.Argv) != 3 || w.Env["A"] != "b" {
		t.Fatalf("unexpected workload: %+v", w)
	}

	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, []byte(`{"env":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWorkload(empty); err == nil {
		t.Fatal("empty argv should be rejected")
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWorkload(bad); err == nil {
		t.Fatal("malformed JSON should be rejected")
	}
}

func TestParseSignal(t *testing.T) {
	tests := []struct {
		name    string
		want    string
		wantErr bool
	}{
		{name: "SIGTERM", want: "terminated"},
		{name: "term", want: "terminated"},
		{name: "SIGKILL", want: "killed"},
		{name: "USR1", want: "user defined signal 1"},
		{name: "nonsense", wantErr: true},
		{name: "", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sig, err := parseSignal(tc.name)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", sig)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSignal(%q): %v", tc.name, err)
			}
			if sig.String() != tc.want {
				t.Fatalf("parseSignal(%q) = %v, want %s", tc.name, sig, tc.want)
			}
		})
	}
}

func TestServerSignalDetachedWorkload(t *testing.T) {
	f := startAgentServer(t)
	cl := dialTestClient(t, f.sock)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if _, err := cl.Exec(ctx, Request{Detach: true, Argv: []string{"/bin/sleep", "30"}}); err != nil {
		t.Fatalf("detach: %v", err)
	}
	// Wait for the workload to be recorded as running.
	deadline := time.Now().Add(5 * time.Second)
	for !workloadRunning(f.srv) {
		if time.Now().After(deadline) {
			t.Fatal("workload did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	resp, err := cl.Exec(ctx, Request{Op: OpSignal, Signal: "SIGKILL"})
	if err != nil {
		t.Fatalf("signal: %v", err)
	}
	if resp.Error != "" {
		t.Fatalf("signal response error: %+v", resp)
	}

	for {
		select {
		case ev := <-cl.Events():
			if ev.Type == EventWorkloadExit {
				return
			}
		case <-time.After(10 * time.Second):
			t.Fatal("workload_exit event not received after signal")
		}
	}
}

func workloadRunning(s *Server) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.workload != nil && s.workload.running
}

func TestExecEmptyArgv(t *testing.T) {
	f := startAgentServer(t)
	cl := dialTestClient(t, f.sock)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := cl.Exec(ctx, Request{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if resp.Error == "" || resp.ExitCode != -1 {
		t.Fatalf("expected argv error, got %+v", resp)
	}
}

func TestDialFirecrackerMissingGuest(t *testing.T) {
	// No proxy/guest listener: connect attempts must fail within the timeout.
	path := filepath.Join(t.TempDir(), "missing.sock")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := DialFirecracker(ctx, path, 3, 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected dial error for missing guest")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("dial took too long: %s", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "connect") {
		t.Fatalf("unexpected error: %v", err)
	}
}
