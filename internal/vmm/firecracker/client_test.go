package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeAPI is a fake Firecracker API server on a Unix socket.
type fakeAPI struct {
	sock string

	mu       sync.Mutex
	requests []recordedRequest
	failPath string
	status   int
	fault    string
	delay    time.Duration
}

func startFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{sock: filepath.Join(t.TempDir(), "api.sock")}
	ln, err := net.Listen("unix", f.sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(f.handle)}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return f
}

func (f *fakeAPI) handle(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil {
		body = make([]byte, 0, 256)
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			body = append(body, buf[:n]...)
			if err != nil {
				break
			}
		}
	}

	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{Method: r.Method, Path: r.URL.Path, Body: string(body)})
	failPath, status, fault, delay := f.failPath, f.status, f.fault, f.delay
	f.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}
	// Simulate client timeout/cancellation while the handler sleeps.
	select {
	case <-r.Context().Done():
		return
	default:
	}

	if failPath != "" && r.URL.Path == failPath {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if fault != "" {
			_, _ = fmt.Fprintf(w, `{"fault_message":%q}`, fault)
		}
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"fake","state":"Running","vmm_version":"fake"}`))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeAPI) lastRequest(t *testing.T) recordedRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("fake API received no requests")
	}
	return f.requests[len(f.requests)-1]
}

func (f *fakeAPI) setFail(path string, status int, fault string) {
	f.mu.Lock()
	f.failPath, f.status, f.fault = path, status, fault
	f.mu.Unlock()
}

func TestAPIClientRequests(t *testing.T) {
	loadReq := snapshotLoadRequest{
		SnapshotPath:  "/snap/vmstate",
		MemBackend:    memBackend{BackendType: "File", BackendPath: "/snap/mem"},
		ResumeVM:      true,
		VsockOverride: &vsockOverride{VsockID: VsockID, UDSPath: "/run/vsock.sock"},
		NetworkOverrides: []networkOverride{
			{IfaceID: NetIfaceID, HostDevName: "tap-abc"},
		},
	}

	tests := []struct {
		name       string
		call       func(ctx context.Context, c *apiClient) error
		wantMethod string
		wantPath   string
		wantBody   map[string]any
	}{
		{
			name:       "ping",
			call:       func(ctx context.Context, c *apiClient) error { return c.ping(ctx) },
			wantMethod: http.MethodGet,
			wantPath:   "/",
		},
		{
			name: "boot-source",
			call: func(ctx context.Context, c *apiClient) error {
				return c.bootSource(ctx, "/boot/vmlinux", "console=ttyS0")
			},
			wantMethod: http.MethodPut,
			wantPath:   "/boot-source",
			wantBody:   map[string]any{"kernel_image_path": "/boot/vmlinux", "boot_args": "console=ttyS0"},
		},
		{
			name: "root drive",
			call: func(ctx context.Context, c *apiClient) error {
				return c.rootDrive(ctx, RootDriveID, "/volumes/disk.ext4", false)
			},
			wantMethod: http.MethodPut,
			wantPath:   "/drives/rootfs",
			wantBody: map[string]any{
				"drive_id": "rootfs", "path_on_host": "/volumes/disk.ext4",
				"is_root_device": true, "is_read_only": false,
			},
		},
		{
			name: "machine-config",
			call: func(ctx context.Context, c *apiClient) error {
				return c.machineConfig(ctx, 4, 512)
			},
			wantMethod: http.MethodPut,
			wantPath:   "/machine-config",
			wantBody:   map[string]any{"vcpu_count": float64(4), "mem_size_mib": float64(512)},
		},
		{
			name: "network-interface",
			call: func(ctx context.Context, c *apiClient) error {
				return c.networkInterface(ctx, NetIfaceID, "tap-abc", "06:00:ac:10:00:02")
			},
			wantMethod: http.MethodPut,
			wantPath:   "/network-interfaces/eth0",
			wantBody: map[string]any{
				"iface_id": "eth0", "host_dev_name": "tap-abc", "guest_mac": "06:00:ac:10:00:02",
			},
		},
		{
			name: "serial",
			call: func(ctx context.Context, c *apiClient) error {
				return c.serial(ctx, "/run/serial.log")
			},
			wantMethod: http.MethodPut,
			wantPath:   "/serial",
			wantBody:   map[string]any{"serial_out_path": "/run/serial.log"},
		},
		{
			name: "vsock",
			call: func(ctx context.Context, c *apiClient) error {
				return c.vsock(ctx, VsockID, 42, "/run/vsock.sock")
			},
			wantMethod: http.MethodPut,
			wantPath:   "/vsock",
			wantBody:   map[string]any{"vsock_id": "vsock0", "guest_cid": float64(42), "uds_path": "/run/vsock.sock"},
		},
		{
			name:       "start",
			call:       func(ctx context.Context, c *apiClient) error { return c.start(ctx) },
			wantMethod: http.MethodPut,
			wantPath:   "/actions",
			wantBody:   map[string]any{"action_type": "InstanceStart"},
		},
		{
			name:       "pause",
			call:       func(ctx context.Context, c *apiClient) error { return c.setVMState(ctx, "Paused") },
			wantMethod: http.MethodPatch,
			wantPath:   "/vm",
			wantBody:   map[string]any{"state": "Paused"},
		},
		{
			name:       "resume",
			call:       func(ctx context.Context, c *apiClient) error { return c.setVMState(ctx, "Resumed") },
			wantMethod: http.MethodPatch,
			wantPath:   "/vm",
			wantBody:   map[string]any{"state": "Resumed"},
		},
		{
			name: "snapshot-create",
			call: func(ctx context.Context, c *apiClient) error {
				return c.snapshotCreate(ctx, "/snap/vmstate", "/snap/mem")
			},
			wantMethod: http.MethodPut,
			wantPath:   "/snapshot/create",
			wantBody: map[string]any{
				"snapshot_path": "/snap/vmstate", "mem_file_path": "/snap/mem", "snapshot_type": "Full",
			},
		},
		{
			name: "snapshot-load",
			call: func(ctx context.Context, c *apiClient) error {
				return c.snapshotLoad(ctx, loadReq)
			},
			wantMethod: http.MethodPut,
			wantPath:   "/snapshot/load",
			wantBody: map[string]any{
				"snapshot_path": "/snap/vmstate",
				"resume_vm":     true,
				"mem_backend": map[string]any{
					"backend_type": "File", "backend_path": "/snap/mem",
				},
				"vsock_override": map[string]any{"vsock_id": "vsock0", "uds_path": "/run/vsock.sock"},
				"network_overrides": []any{
					map[string]any{"iface_id": "eth0", "host_dev_name": "tap-abc"},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := startFakeAPI(t)
			c := newAPIClient(f.sock, time.Second)
			defer c.close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := tc.call(ctx, c); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			got := f.lastRequest(t)
			if got.Method != tc.wantMethod || got.Path != tc.wantPath {
				t.Fatalf("request = %s %s, want %s %s", got.Method, got.Path, tc.wantMethod, tc.wantPath)
			}
			if tc.wantBody == nil {
				return
			}
			var body map[string]any
			if err := json.Unmarshal([]byte(got.Body), &body); err != nil {
				t.Fatalf("decode request body %q: %v", got.Body, err)
			}
			for k, want := range tc.wantBody {
				gotVal, ok := body[k]
				if !ok {
					t.Fatalf("request body missing %q: %s", k, got.Body)
				}
				if fmt.Sprintf("%v", gotVal) != fmt.Sprintf("%v", want) {
					t.Fatalf("request body[%q] = %v, want %v", k, gotVal, want)
				}
			}
		})
	}
}

func TestAPIClientErrorMapping(t *testing.T) {
	f := startFakeAPI(t)
	f.setFail("/boot-source", http.StatusBadRequest, "The kernel file cannot be opened")
	c := newAPIClient(f.sock, time.Second)
	defer c.close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := c.bootSource(ctx, "/missing", "console=ttyS0")
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is not *APIError: %v", err)
	}
	if apiErr.StatusCode != http.StatusBadRequest || apiErr.Path != "/boot-source" {
		t.Fatalf("unexpected APIError: %+v", apiErr)
	}
	if apiErr.Fault != "The kernel file cannot be opened" {
		t.Fatalf("unexpected fault: %q", apiErr.Fault)
	}
	if !isAPIError(err) {
		t.Fatal("isAPIError should be true")
	}
}

func TestAPIClientTimeout(t *testing.T) {
	f := startFakeAPI(t)
	f.mu.Lock()
	f.delay = 500 * time.Millisecond
	f.mu.Unlock()
	c := newAPIClient(f.sock, 50*time.Millisecond)
	defer c.close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := c.ping(ctx)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAPIClientUnreachableSocket(t *testing.T) {
	c := newAPIClient(filepath.Join(t.TempDir(), "missing.sock"), 100*time.Millisecond)
	defer c.close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.ping(ctx); err == nil {
		t.Fatal("expected connection error")
	}
}

func TestNewAPIClientDefaultTimeout(t *testing.T) {
	c := newAPIClient("/tmp/x.sock", 0)
	if c.timeout != defaultRequestTimeout {
		t.Fatalf("timeout = %s, want %s", c.timeout, defaultRequestTimeout)
	}
}
