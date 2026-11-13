package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// defaultDialTimeout bounds DialFirecracker when the caller passes zero.
	defaultDialTimeout = 10 * time.Second
	// eventsBuffer is the Events channel backlog. Delivery is best-effort: when
	// the buffer is full the oldest event is dropped.
	eventsBuffer = 256
	// dialRetryInterval is the pause between connect attempts while the guest
	// agent is still starting.
	dialRetryInterval = 50 * time.Millisecond
)

// DialFirecracker connects to the guest agent through a Firecracker vsock
// host-side Unix socket. guestCID is accepted for API symmetry with the
// Firecracker vsock contract; the UDS belongs to one VM so the host protocol
// ("CONNECT <port>") does not need it.
//
// The dial retries until the guest agent starts listening, bounded by ctx and
// timeout (zero means defaultDialTimeout). WaitReady then blocks until the
// agent's ready event arrives.
func DialFirecracker(ctx context.Context, vsockUDSPath string, guestCID uint32, timeout time.Duration) (Client, error) {
	if vsockUDSPath == "" {
		return nil, errors.New("agent: empty vsock UDS path")
	}
	if timeout <= 0 {
		timeout = defaultDialTimeout
	}
	cl := &client{
		sockPath: vsockUDSPath,
		guestCID: guestCID,
		timeout:  timeout,
		pending:  make(map[string]chan result),
		events:   make(chan Event, eventsBuffer),
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c, err := cl.dial(dialCtx)
	if err != nil {
		return nil, err
	}
	cl.setConn(c)
	return cl, nil
}

type result struct {
	resp Response
	err  error
}

// client implements Client over a Firecracker vsock UDS.
type client struct {
	sockPath string
	guestCID uint32
	timeout  time.Duration

	mu       sync.Mutex
	conn     *hostConn
	pending  map[string]chan result
	closed   bool
	closeOne sync.Once
	dialMu   sync.Mutex

	events  chan Event
	nextID  atomic.Uint64
	readers sync.WaitGroup
}

type hostConn struct {
	nc        net.Conn
	br        *bufio.Reader
	ready     chan struct{}
	readyOnce sync.Once
	done      chan struct{}
}

func (c *hostConn) markReady() { c.readyOnce.Do(func() { close(c.ready) }) }

func (c *hostConn) isDone() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

func (c *hostConn) close() { _ = c.nc.Close() }

// dial retries connecting until ctx is done.
func (cl *client) dial(ctx context.Context) (*hostConn, error) {
	var lastErr error
	for {
		c, err := cl.dialOnce(ctx)
		if err == nil {
			return c, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("agent: connect %s: %w (last error: %v)", cl.sockPath, ctx.Err(), lastErr)
		case <-time.After(dialRetryInterval):
		}
	}
}

func (cl *client) dialOnce(ctx context.Context) (*hostConn, error) {
	var d net.Dialer
	nc, err := d.DialContext(ctx, "unix", cl.sockPath)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(cl.timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = nc.SetDeadline(deadline)
	if _, err := io.WriteString(nc, fmt.Sprintf("CONNECT %d\n", VsockPort)); err != nil {
		_ = nc.Close()
		return nil, fmt.Errorf("send CONNECT: %w", err)
	}
	br := bufio.NewReader(nc)
	line, err := br.ReadString('\n')
	if err != nil {
		_ = nc.Close()
		return nil, fmt.Errorf("read CONNECT reply: %w", err)
	}
	if reply := strings.TrimSpace(line); !strings.HasPrefix(reply, "OK") {
		_ = nc.Close()
		return nil, fmt.Errorf("vsock connect rejected: %q", reply)
	}
	_ = nc.SetDeadline(time.Time{})
	return &hostConn{nc: nc, br: br, ready: make(chan struct{}), done: make(chan struct{})}, nil
}

// setConn installs c as the current connection and starts its reader.
func (cl *client) setConn(c *hostConn) {
	cl.mu.Lock()
	if cl.closed {
		cl.mu.Unlock()
		c.close()
		return
	}
	cl.conn = c
	cl.readers.Add(1)
	cl.mu.Unlock()
	go cl.readLoop(c)
}

func (cl *client) readLoop(c *hostConn) {
	defer cl.readers.Done()
	defer func() {
		// Detach the dead connection so reconnect() knows to dial again.
		cl.mu.Lock()
		if cl.conn == c {
			cl.conn = nil
		}
		cl.mu.Unlock()
		close(c.done)
	}()
	for {
		line, err := c.br.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			cl.handleFrame(c, line)
		}
		if err != nil {
			cl.failPending(err)
			return
		}
	}
}

func (cl *client) handleFrame(c *hostConn, line []byte) {
	var header struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &header); err != nil {
		return
	}
	if header.Type != "" {
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return
		}
		if ev.Type == EventReady {
			c.markReady()
		}
		cl.pushEvent(ev)
		return
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return
	}
	cl.mu.Lock()
	ch := cl.pending[resp.ID]
	delete(cl.pending, resp.ID)
	cl.mu.Unlock()
	if ch != nil {
		select {
		case ch <- result{resp: resp}:
		default:
		}
	}
}

func (cl *client) failPending(err error) {
	cl.mu.Lock()
	pending := cl.pending
	cl.pending = make(map[string]chan result)
	cl.mu.Unlock()
	for id, ch := range pending {
		select {
		case ch <- result{err: fmt.Errorf("agent: connection lost (request %s): %w", id, err)}:
		default:
		}
	}
}

func (cl *client) pushEvent(ev Event) {
	select {
	case cl.events <- ev:
	default:
		// Best-effort: drop the oldest event to make room.
		select {
		case <-cl.events:
		default:
		}
		select {
		case cl.events <- ev:
		default:
		}
	}
}

// ensureConn returns a live connection, reconnecting once if the previous one
// was reset (for example after a Firecracker snapshot restore).
func (cl *client) ensureConn(ctx context.Context) (*hostConn, error) {
	cl.mu.Lock()
	if cl.closed {
		cl.mu.Unlock()
		return nil, errors.New("agent: client closed")
	}
	c := cl.conn
	cl.mu.Unlock()
	if c != nil && !c.isDone() {
		return c, nil
	}
	return cl.reconnect(ctx, false)
}

// reconnect dials a new vsock connection. With force it drops the current
// connection even if its reader has not noticed the reset yet (a write failure
// can precede the reader's EOF).
func (cl *client) reconnect(ctx context.Context, force bool) (*hostConn, error) {
	cl.dialMu.Lock()
	defer cl.dialMu.Unlock()

	cl.mu.Lock()
	if cl.closed {
		cl.mu.Unlock()
		return nil, errors.New("agent: client closed")
	}
	current := cl.conn
	if current != nil && !current.isDone() && !force {
		cl.mu.Unlock()
		return current, nil
	}
	cl.conn = nil
	cl.mu.Unlock()
	if current != nil {
		current.close()
	}

	dctx, cancel := context.WithTimeout(ctx, cl.timeout)
	defer cancel()
	c, err := cl.dial(dctx)
	if err != nil {
		return nil, err
	}
	cl.setConn(c)
	return c, nil
}

func (cl *client) roundTrip(ctx context.Context, req Request) (Response, error) {
	if req.Op == "" {
		req.Op = OpExec
	}
	if req.ID == "" {
		req.ID = fmt.Sprintf("r%d", cl.nextID.Add(1))
	}
	ch := make(chan result, 1)

	c, err := cl.ensureConn(ctx)
	if err != nil {
		return Response{}, err
	}

	cl.mu.Lock()
	if cl.closed {
		cl.mu.Unlock()
		return Response{}, errors.New("agent: client closed")
	}
	cl.pending[req.ID] = ch
	cl.mu.Unlock()

	line, err := WriteLine(req)
	if err != nil {
		cl.dropPending(req.ID)
		return Response{}, fmt.Errorf("agent: encode request: %w", err)
	}
	if _, err := c.nc.Write(line); err != nil {
		cl.dropPending(req.ID)
		return Response{}, fmt.Errorf("agent: write request: %w", err)
	}

	select {
	case r := <-ch:
		return r.resp, r.err
	case <-ctx.Done():
		cl.dropPending(req.ID)
		return Response{}, fmt.Errorf("agent: request %q: %w", req.Op, ctx.Err())
	}
}

func (cl *client) dropPending(id string) {
	cl.mu.Lock()
	delete(cl.pending, id)
	cl.mu.Unlock()
}

// request sends req, retrying once over a fresh connection if the previous
// connection was reset. The retry is best-effort: if the guest executed the
// first copy but the response was lost, it may run twice.
func (cl *client) request(ctx context.Context, req Request) (Response, error) {
	resp, err := cl.roundTrip(ctx, req)
	if err == nil || ctx.Err() != nil || cl.isClosed() {
		return resp, err
	}
	if _, rerr := cl.reconnect(ctx, true); rerr != nil {
		return resp, err
	}
	return cl.roundTrip(ctx, req)
}

func (cl *client) isClosed() bool {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	return cl.closed
}

// WaitReady blocks until the guest agent reports ready on a live connection.
func (cl *client) WaitReady(ctx context.Context) error {
	for {
		c, err := cl.ensureConn(ctx)
		if err != nil {
			return err
		}
		select {
		case <-c.ready:
			return nil
		case <-c.done:
			if err := ctx.Err(); err != nil {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(dialRetryInterval):
			}
		case <-ctx.Done():
			return fmt.Errorf("agent: wait ready: %w", ctx.Err())
		}
	}
}

// Exec runs a command in the guest.
func (cl *client) Exec(ctx context.Context, req Request) (Response, error) {
	if req.Op == "" {
		req.Op = OpExec
	}
	return cl.request(ctx, req)
}

// Touch reports explicit guest activity.
func (cl *client) Touch(ctx context.Context) error {
	resp, err := cl.request(ctx, Request{Op: OpTouch})
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("agent: touch: %s", resp.Error)
	}
	return nil
}

// Events streams agent events until the channel closes.
func (cl *client) Events() <-chan Event { return cl.events }

// Close closes the connection and the Events channel.
func (cl *client) Close() error {
	cl.closeOne.Do(func() {
		cl.mu.Lock()
		cl.closed = true
		c := cl.conn
		cl.conn = nil
		cl.mu.Unlock()
		if c != nil {
			c.close()
		}
		cl.failPending(errors.New("agent: client closed"))
		cl.readers.Wait()
		close(cl.events)
	})
	return nil
}
