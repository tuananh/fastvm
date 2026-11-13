package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// defaultRequestTimeout bounds one Firecracker API request.
const defaultRequestTimeout = 10 * time.Second

// APIError is a non-2xx response from the Firecracker API. Firecracker returns
// errors as {"fault_message": "..."}.
type APIError struct {
	Method     string
	Path       string
	StatusCode int
	Fault      string
	Body       string
}

func (e *APIError) Error() string {
	detail := e.Fault
	if detail == "" {
		detail = e.Body
	}
	if detail == "" {
		detail = http.StatusText(e.StatusCode)
	}
	return fmt.Sprintf("firecracker API: %s %s: HTTP %d: %s", e.Method, e.Path, e.StatusCode, detail)
}

// apiClient is a minimal Firecracker API client: HTTP/1.1 over a Unix socket.
type apiClient struct {
	sock    string
	hc      *http.Client
	timeout time.Duration
}

func newAPIClient(sock string, timeout time.Duration) *apiClient {
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
		MaxIdleConns:    2,
		IdleConnTimeout: 30 * time.Second,
	}
	return &apiClient{sock: sock, hc: &http.Client{Transport: tr}, timeout: timeout}
}

func (c *apiClient) close() { c.hc.CloseIdleConnections() }

// do performs one request. Successful responses are expected to be 204 or 200;
// resp may be nil when the response body is not needed.
func (c *apiClient) do(ctx context.Context, method, path string, req, resp any) error {
	var body io.Reader
	if req != nil {
		b, err := json.Marshal(req)
		if err != nil {
			return fmt.Errorf("encode %s %s: %w", method, path, err)
		}
		body = bytes.NewReader(b)
	}
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	hreq, err := http.NewRequestWithContext(reqCtx, method, "http://firecracker"+path, body)
	if err != nil {
		return fmt.Errorf("build %s %s: %w", method, path, err)
	}
	hreq.Header.Set("Content-Type", "application/json")

	hres, err := c.hc.Do(hreq)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer hres.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(hres.Body, 1<<20))
	if hres.StatusCode >= 400 {
		apiErr := &APIError{Method: method, Path: path, StatusCode: hres.StatusCode, Body: string(data)}
		var fault struct {
			FaultMessage string `json:"fault_message"`
		}
		if json.Unmarshal(data, &fault) == nil {
			apiErr.Fault = fault.FaultMessage
		}
		return apiErr
	}
	if readErr != nil {
		return fmt.Errorf("read %s %s response: %w", method, path, readErr)
	}
	if resp != nil && len(data) > 0 {
		if err := json.Unmarshal(data, resp); err != nil {
			return fmt.Errorf("decode %s %s response: %w", method, path, err)
		}
	}
	return nil
}

// ping checks the API is live. It doubles as the boot readiness probe.
func (c *apiClient) ping(ctx context.Context) error {
	var info struct {
		State string `json:"state"`
	}
	return c.do(ctx, http.MethodGet, "/", nil, &info)
}

func (c *apiClient) bootSource(ctx context.Context, kernelPath, cmdline string) error {
	req := struct {
		KernelImagePath string `json:"kernel_image_path"`
		BootArgs        string `json:"boot_args"`
	}{KernelImagePath: kernelPath, BootArgs: cmdline}
	return c.do(ctx, http.MethodPut, "/boot-source", &req, nil)
}

func (c *apiClient) rootDrive(ctx context.Context, driveID, path string, readOnly bool) error {
	req := struct {
		DriveID      string `json:"drive_id"`
		PathOnHost   string `json:"path_on_host"`
		IsRootDevice bool   `json:"is_root_device"`
		IsReadOnly   bool   `json:"is_read_only"`
	}{DriveID: driveID, PathOnHost: path, IsRootDevice: true, IsReadOnly: readOnly}
	return c.do(ctx, http.MethodPut, "/drives/"+driveID, &req, nil)
}

func (c *apiClient) machineConfig(ctx context.Context, vcpus, memMB int) error {
	req := struct {
		VCPUCount  int `json:"vcpu_count"`
		MemSizeMiB int `json:"mem_size_mib"`
	}{VCPUCount: vcpus, MemSizeMiB: memMB}
	return c.do(ctx, http.MethodPut, "/machine-config", &req, nil)
}

func (c *apiClient) networkInterface(ctx context.Context, ifaceID, hostDev, guestMAC string) error {
	req := struct {
		IfaceID     string `json:"iface_id"`
		HostDevName string `json:"host_dev_name"`
		GuestMAC    string `json:"guest_mac,omitempty"`
	}{IfaceID: ifaceID, HostDevName: hostDev, GuestMAC: guestMAC}
	return c.do(ctx, http.MethodPut, "/network-interfaces/"+ifaceID, &req, nil)
}

func (c *apiClient) serial(ctx context.Context, outPath string) error {
	req := struct {
		SerialOutPath string `json:"serial_out_path"`
	}{SerialOutPath: outPath}
	return c.do(ctx, http.MethodPut, "/serial", &req, nil)
}

func (c *apiClient) vsock(ctx context.Context, vsockID string, guestCID uint32, udsPath string) error {
	req := struct {
		VsockID  string `json:"vsock_id"`
		GuestCID uint32 `json:"guest_cid"`
		UDSPath  string `json:"uds_path"`
	}{VsockID: vsockID, GuestCID: guestCID, UDSPath: udsPath}
	return c.do(ctx, http.MethodPut, "/vsock", &req, nil)
}

func (c *apiClient) start(ctx context.Context) error {
	req := struct {
		ActionType string `json:"action_type"`
	}{ActionType: "InstanceStart"}
	return c.do(ctx, http.MethodPut, "/actions", &req, nil)
}

func (c *apiClient) setVMState(ctx context.Context, state string) error {
	req := struct {
		State string `json:"state"`
	}{State: state}
	return c.do(ctx, http.MethodPatch, "/vm", &req, nil)
}

func (c *apiClient) snapshotCreate(ctx context.Context, statePath, memPath string) error {
	req := struct {
		SnapshotPath string `json:"snapshot_path"`
		MemFilePath  string `json:"mem_file_path"`
		SnapshotType string `json:"snapshot_type"`
	}{SnapshotPath: statePath, MemFilePath: memPath, SnapshotType: "Full"}
	return c.do(ctx, http.MethodPut, "/snapshot/create", &req, nil)
}

type memBackend struct {
	BackendType string `json:"backend_type"`
	BackendPath string `json:"backend_path"`
}

type vsockOverride struct {
	VsockID string `json:"vsock_id"`
	UDSPath string `json:"uds_path"`
}

type networkOverride struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
}

type snapshotLoadRequest struct {
	SnapshotPath     string            `json:"snapshot_path"`
	MemBackend       memBackend        `json:"mem_backend"`
	ResumeVM         bool              `json:"resume_vm"`
	VsockOverride    *vsockOverride    `json:"vsock_override,omitempty"`
	NetworkOverrides []networkOverride `json:"network_overrides,omitempty"`
}

func (c *apiClient) snapshotLoad(ctx context.Context, req snapshotLoadRequest) error {
	return c.do(ctx, http.MethodPut, "/snapshot/load", &req, nil)
}

// isAPIError reports whether err is a Firecracker API error with status code.
func isAPIError(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr)
}
