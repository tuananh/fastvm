package cloudhypervisor

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fastvm/internal/types"
	"fastvm/internal/vmm"
)

func TestBootArgs(t *testing.T) {
	cfg := vmm.BootConfig{
		SandboxID:   "sb-1",
		KernelPath:  "/kernels/vmlinux",
		DiskPath:    "/volumes/sb-1.img",
		DataDisks:   []string{"/volumes/sb-1-data-0.img", "/volumes/sb-1-data-1.img"},
		VCPUs:       4,
		MemoryMB:    1024,
		TAPName:     "tap-1",
		MAC:         "02:00:00:00:00:01",
		GuestCID:    7,
		VsockUDPath: "/run/sb-1/vsock.sock",
		APISocket:   "/run/sb-1/api.sock",
		SerialPath:  "/run/sb-1/serial.log",
		Cmdline:     "console=ttyS0 root=/dev/vda rw init=/init",
		Volumes: []types.Volume{
			{HostPath: "/shares/a", GuestPath: "/mnt/a"},
			{HostPath: "/shares/b", GuestPath: "/mnt/b", ReadOnly: true},
		},
	}
	args := bootArgs(cfg, vmPaths{apiSock: cfg.APISocket, serial: cfg.SerialPath, vmmLog: "/run/sb-1/vmm.log"},
		[]string{"/run/sb-1/fs-0.sock", "/run/sb-1/fs-1.sock"})
	joined := strings.Join(args, "\n")
	for _, want := range []string{
		"--kernel\n/kernels/vmlinux",
		"--cmdline\nconsole=ttyS0 root=/dev/vda rw init=/init",
		"--cpus\nboot=4",
		"--memory\nsize=1024M,shared=on",
		"--disk\npath=/volumes/sb-1.img,image_type=raw",
		"--disk\npath=/volumes/sb-1-data-0.img,image_type=raw",
		"--disk\npath=/volumes/sb-1-data-1.img,image_type=raw",
		"--net\ntap=tap-1,mac=02:00:00:00:00:01",
		"--vsock\ncid=7,socket=/run/sb-1/vsock.sock",
		"--serial\nfile=/run/sb-1/serial.log",
		"--fs\ntag=fastvm0,socket=/run/sb-1/fs-0.sock",
		"--fs\ntag=fastvm1,socket=/run/sb-1/fs-1.sock",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("boot args missing %q in:\n%s", want, joined)
		}
	}
}

func TestBootArgsDefaultsAndNoVolumes(t *testing.T) {
	cfg := vmm.BootConfig{
		KernelPath: "/k", DiskPath: "/d", APISocket: "/run/a.sock", SerialPath: "/run/s.log",
	}
	args := bootArgs(cfg, vmPaths{apiSock: "/run/a.sock", serial: "/run/s.log"}, nil)
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "shared=on") {
		t.Errorf("shared memory must only be enabled with volumes: %s", joined)
	}
	if strings.Contains(joined, "--fs") || strings.Contains(joined, "--net") || strings.Contains(joined, "--vsock") {
		t.Errorf("unexpected optional device args: %s", joined)
	}
	if !strings.Contains(joined, "size=512M") || !strings.Contains(joined, "boot=2") {
		t.Errorf("defaults not applied: %s", joined)
	}
	if !strings.Contains(joined, "console=ttyS0 root=/dev/vda rw init=/init") {
		t.Errorf("default cmdline not applied: %s", joined)
	}
}

func TestMemoryParam(t *testing.T) {
	if got := memoryParam(512, false); got != "size=512M" {
		t.Fatalf("memoryParam without volumes = %q", got)
	}
	if got := memoryParam(512, true); got != "size=512M,shared=on" {
		t.Fatalf("memoryParam with volumes = %q", got)
	}
}

func TestNewDriverFallsBackToStaticBinary(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"cloud-hypervisor-static", "virtiofsd"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	d, err := NewDriver(Options{Binary: DefaultBinary})
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	if d.binary != filepath.Join(dir, "cloud-hypervisor-static") {
		t.Fatalf("binary = %q", d.binary)
	}
	vf, err := d.resolveVirtiofsd()
	if err != nil || vf != filepath.Join(dir, "virtiofsd") {
		t.Fatalf("virtiofsd = %q, %v", vf, err)
	}
}

func TestResolveExecutableMissing(t *testing.T) {
	if _, err := resolveExecutable("definitely-not-a-real-binary-xyz", "also-missing-xyz"); err == nil {
		t.Fatal("expected error for a missing executable")
	}
}

// testAPIServer starts an HTTP server on a Unix socket and records requests.
func testAPIServer(t *testing.T, status int, response string) (sock string, got *[]string) {
	t.Helper()
	dir := t.TempDir()
	sock = dir + "/api.sock"
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	requests := &[]string{}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*requests = append(*requests, r.Method+" "+r.URL.Path+" "+string(body))
		w.WriteHeader(status)
		if response != "" {
			_, _ = w.Write([]byte(response))
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock, requests
}

func TestAPIClient(t *testing.T) {
	sock, requests := testAPIServer(t, http.StatusNoContent, "")
	cl := newAPIClient(sock, 5_000_000_000)
	defer cl.close()
	ctx := context.Background()
	if err := cl.pause(ctx); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := cl.resume(ctx); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if err := cl.shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := cl.snapshot(ctx, "/snap/dir"); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	want := []string{
		"PUT /api/v1/vm.pause ",
		"PUT /api/v1/vm.resume ",
		"PUT /api/v1/vm.shutdown ",
		`PUT /api/v1/vm.snapshot {"destination_url":"file:///snap/dir"}`,
	}
	got := *requests
	if len(got) != len(want) {
		t.Fatalf("requests = %#v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("request %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestAPIClientError(t *testing.T) {
	sock, _ := testAPIServer(t, http.StatusBadRequest, `{"error":"bad"}`)
	cl := newAPIClient(sock, 5_000_000_000)
	defer cl.close()
	err := cl.pause(context.Background())
	if err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("expected HTTP error, got %v", err)
	}
}

func TestAPIClientInfo(t *testing.T) {
	sock, requests := testAPIServer(t, http.StatusOK, `{"state":"Running"}`)
	cl := newAPIClient(sock, 5_000_000_000)
	defer cl.close()
	if err := cl.info(context.Background()); err != nil {
		t.Fatalf("info: %v", err)
	}
	if len(*requests) != 1 || (*requests)[0] != "GET /api/v1/vm.info " {
		t.Fatalf("requests = %#v", *requests)
	}
}

func TestAPIClientCounters(t *testing.T) {
	body := `{"block0":{"device_type":"block","read_bytes":123,"write_bytes":456},"net0":{"device_type":"net","rx_bytes":1,"tx_bytes":2}}`
	sock, requests := testAPIServer(t, http.StatusOK, body)
	cl := newAPIClient(sock, 5_000_000_000)
	defer cl.close()
	counters, err := cl.counters(context.Background())
	if err != nil {
		t.Fatalf("counters: %v", err)
	}
	if got := counters["block0"].ReadBytes; got != 123 {
		t.Errorf("read bytes = %d", got)
	}
	if got := counters["net0"].DeviceType; got != "net" {
		t.Errorf("device type = %q", got)
	}
	if len(*requests) != 1 || (*requests)[0] != "GET /api/v1/vm.counters " {
		t.Fatalf("requests = %#v", *requests)
	}
}

func TestBlockCounters(t *testing.T) {
	read, write, found := blockCounters(map[string]deviceCounters{
		"block0": {DeviceType: "block", ReadBytes: 10, WriteBytes: 20},
		"block1": {DeviceType: "block", ReadBytes: 5, WriteBytes: 6},
		"net0":   {DeviceType: "net", ReadBytes: 100, WriteBytes: 200},
		"_disk0": {ReadBytes: 2, WriteBytes: 3},
	})
	if !found || read != 17 || write != 29 {
		t.Fatalf("block counters = %d/%d found=%v", read, write, found)
	}
	if _, _, found := blockCounters(map[string]deviceCounters{
		"net0": {DeviceType: "net"},
	}); found {
		t.Fatal("net-only counters should not count as block")
	}
}
