package cloudhypervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"fastvm/internal/vmm"
)

// apiClient talks to Cloud Hypervisor's REST API over its Unix socket.
type apiClient struct {
	sock string
	hc   *http.Client
}

func newAPIClient(sock string, timeout time.Duration) *apiClient {
	dialer := &net.Dialer{Timeout: timeout}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", sock)
		},
	}
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	return &apiClient{
		sock: sock,
		hc:   &http.Client{Transport: tr, Timeout: timeout},
	}
}

// close releases idle API connections.
func (c *apiClient) close() { c.hc.CloseIdleConnections() }

// do performs one API request. Paths are relative to /api/v1/ (e.g. "vm.pause").
func (c *apiClient) do(ctx context.Context, method, endpoint string, body, out any) error {
	var rd io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(buf)
	}
	url := "http://localhost/api/v1/" + endpoint
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("cloud hypervisor API %s %s: HTTP %d: %s", method, endpoint, resp.StatusCode, string(msg))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil && err != io.EOF {
			return fmt.Errorf("cloud hypervisor API %s %s: decode response: %w", method, endpoint, err)
		}
	}
	return nil
}

// info checks that the VMM API is serving (GET /vm.info).
func (c *apiClient) info(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "vm.info", nil, nil)
}

// pause halts the vCPUs (PUT /vm.pause).
func (c *apiClient) pause(ctx context.Context) error {
	return c.do(ctx, http.MethodPut, "vm.pause", nil, nil)
}

// resume restarts the vCPUs (PUT /vm.resume).
func (c *apiClient) resume(ctx context.Context) error {
	return c.do(ctx, http.MethodPut, "vm.resume", nil, nil)
}

// shutdown asks the VMM to shut the VM down (PUT /vm.shutdown).
func (c *apiClient) shutdown(ctx context.Context) error {
	return c.do(ctx, http.MethodPut, "vm.shutdown", nil, nil)
}

// deviceCounters is one entry in /vm.counters. Cloud Hypervisor returns a
// device_type string plus type-specific uint64 counters.
type deviceCounters struct {
	DeviceType string `json:"device_type"`
	ReadBytes  uint64 `json:"read_bytes"`
	WriteBytes uint64 `json:"write_bytes"`
}

// counters reads per-device virtio counters (GET /vm.counters).
func (c *apiClient) counters(ctx context.Context) (map[string]deviceCounters, error) {
	var out map[string]deviceCounters
	if err := c.do(ctx, http.MethodGet, "vm.counters", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// resize changes live vCPU, RAM, and balloon targets (PUT /vm.resize).
func (c *apiClient) resize(ctx context.Context, cfg vmm.ResizeConfig) error {
	body := make(map[string]any)
	if cfg.VCPUs > 0 {
		body["desired_vcpus"] = cfg.VCPUs
	}
	if cfg.MemoryMB > 0 {
		body["desired_ram"] = uint64(cfg.MemoryMB) << 20
	}
	if cfg.ResizeBalloon {
		body["desired_balloon"] = uint64(cfg.BalloonMB) << 20
	}
	return c.do(ctx, http.MethodPut, "vm.resize", body, nil)
}

// snapshot writes a paused VM snapshot into dir (PUT /vm.snapshot).
func (c *apiClient) snapshot(ctx context.Context, dir string) error {
	return c.do(ctx, http.MethodPut, "vm.snapshot", map[string]string{
		"destination_url": "file://" + dir,
	}, nil)
}
