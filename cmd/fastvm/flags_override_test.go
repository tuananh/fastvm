package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fastvm/internal/types"
)

// TestRunFVMFileCLIFlagsWin verifies that explicit CLI flags override the
// corresponding fvmfile fields.
func TestRunFVMFileCLIFlagsWin(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "fastvm.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	create := make(chan map[string]any, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/images":
			_ = json.NewEncoder(w).Encode([]types.BaseDisk{{Tag: "busybox"}, {Tag: "alpine"}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			create <- body
			_ = json.NewEncoder(w).Encode(types.Sandbox{ID: "sb1", Name: "flags", State: types.StateRunning})
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	file := filepath.Join(dir, "flags.fvmfile")
	if err := os.WriteFile(file, []byte(`
version: 1
name: flags
image: busybox
entrypoint: ["sh", "-c"]
command: ["sleep 60"]
env:
  FROM_FILE: file
  BOTH: file
volumes:
  - host: /tmp
    guest: /work
ports:
  - host: 8080
    guest: 80
network:
  enabled: true
  allow_hosts: [example.com]
resources:
  vcpus: 2
  memory_mb: 512
`), 0o600); err != nil {
		t.Fatal(err)
	}

	root := newRootCmd()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{
		"--socket", sock, "run", "-d", "-f", file,
		"--image", "alpine",
		"--cpu", "4",
		"--memory", "2048",
		"--publish", "9090:90",
		"--env", "FROM_FILE=flag",
		"--volume", "/tmp:/data",
		"--no-net",
		"--no-auto-sleep",
	})
	_ = root.ExecuteContext(context.Background())

	select {
	case body := <-create:
		if body["image"] != "alpine" || body["vcpus"] != float64(4) || body["memory_mb"] != float64(2048) {
			t.Fatalf("create body = %v", body)
		}
		if body["network_enabled"] != false || body["no_auto_sleep"] != true {
			t.Fatalf("network/auto-sleep = %v/%v", body["network_enabled"], body["no_auto_sleep"])
		}
		if hosts, _ := body["allow_hosts"].([]any); len(hosts) != 0 {
			t.Fatalf("allow_hosts should be cleared by --no-net: %v", body["allow_hosts"])
		}
		ports, _ := body["ports"].([]any)
		if len(ports) != 1 {
			t.Fatalf("ports = %v", body["ports"])
		}
		env, _ := body["env"].(map[string]any)
		if env["FROM_FILE"] != "flag" || env["BOTH"] != "file" {
			t.Fatalf("env = %v", env)
		}
		vols, _ := body["volumes"].([]any)
		if len(vols) != 2 {
			t.Fatalf("volumes = %v", body["volumes"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("create request was not sent")
	}
}
