// Package agent defines the guest-agent wire protocol (newline-delimited JSON
// over virtio-vsock) and the host-side client contract.
package agent

import (
	"context"
	"encoding/json"
)

// VsockPort is the guest agent's vsock port.
const VsockPort uint32 = 1024

// Op names.
const (
	OpReady     = "ready"
	OpExec      = "exec"
	OpShell     = "shell"
	OpSignal    = "signal"
	OpTouch     = "touch"
	OpBusy      = "busy"
	OpWorkload  = "workload" // guest -> host event
	OpHeartbeat = "heartbeat"
)

// Event type names pushed guest -> host. They are carried in Event.Type.
const (
	EventReady           = "ready"
	EventWorkloadStarted = "workload_started"
	EventWorkloadExit    = "workload_exit"
	EventWorkloadBusy    = "workload_busy"
	EventTouch           = "touch"
)

// Request is sent host -> guest.
type Request struct {
	ID        string            `json:"id"`
	Op        string            `json:"op"`
	Argv      []string          `json:"argv,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Stdin     string            `json:"stdin,omitempty"`
	Signal    string            `json:"signal,omitempty"`
	TimeoutMS int               `json:"timeout_ms,omitempty"`
	// Detach starts Argv as the sandbox workload in the background and returns
	// immediately; workload_started/workload_exit events follow on Events.
	Detach bool `json:"detach,omitempty"`
	// Busy is the advisory workload-busy hint carried by OpBusy.
	Busy bool `json:"busy,omitempty"`
	// Rows/Cols are the initial PTY size for OpShell.
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
}

// Response is sent guest -> host for a Request.
type Response struct {
	ID       string `json:"id"`
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Event is pushed guest -> host asynchronously.
type Event struct {
	Type    string `json:"type"` // ready, workload_started, workload_exit, workload_busy, touch
	Message string `json:"message,omitempty"`
	Code    int    `json:"code,omitempty"`
}

// Client is the host-side agent connection.
type Client interface {
	// WaitReady blocks until the guest agent reports ready.
	WaitReady(ctx context.Context) error
	// Exec runs a command in the guest.
	Exec(ctx context.Context, req Request) (Response, error)
	// Touch reports explicit guest activity.
	Touch(ctx context.Context) error
	// Events streams agent events until the channel closes.
	Events() <-chan Event
	// Close closes the connection.
	Close() error
}

// WriteLine encodes v as one JSON line.
func WriteLine(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
