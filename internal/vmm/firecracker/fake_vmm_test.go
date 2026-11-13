package firecracker

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
)

// TestFakeVMMProcess is the child-process fake Firecracker used by driver
// tests. The test binary re-execs itself with -test.run=TestFakeVMMProcess and
// passes the real Firecracker CLI (--api-sock ...). Modes are selected with
// environment variables:
//
//	FASTVM_FAKE_VMM=1              (required; marks the helper process)
//	FASTVM_FAKE_VMM_LOG=path       append one JSON request record per line
//	FASTVM_FAKE_VMM_MODE=exit      print a boot error to stderr and exit 3
//	FASTVM_FAKE_VMM_FAIL_PATH=/x   answer that API path with HTTP 400
func TestFakeVMMProcess(t *testing.T) {
	if os.Getenv("FASTVM_FAKE_VMM") != "1" {
		t.Skip("helper process for firecracker driver tests")
	}

	apiSock := ""
	args := flag.Args() // arguments after "--"
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--api-sock" {
			apiSock = args[i+1]
		}
	}
	if apiSock == "" {
		fmt.Fprintln(os.Stderr, "fake-vmm: missing --api-sock")
		os.Exit(2)
	}
	if os.Getenv("FASTVM_FAKE_VMM_MODE") == "exit" {
		fmt.Fprintln(os.Stderr, "fake-vmm: boot failed: kernel rejected by fake VMM")
		os.Exit(3)
	}

	ln, err := net.Listen("unix", apiSock)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake-vmm: listen %s: %v\n", apiSock, err)
		os.Exit(2)
	}
	defer ln.Close()

	logPath := os.Getenv("FASTVM_FAKE_VMM_LOG")
	var logFile *os.File
	if logPath != "" {
		logFile, err = os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fake-vmm: open request log: %v\n", err)
			os.Exit(2)
		}
		defer logFile.Close()
	}
	failPath := os.Getenv("FASTVM_FAKE_VMM_FAIL_PATH")

	var mu sync.Mutex
	record := func(r *http.Request, body []byte) {
		if logFile == nil {
			return
		}
		line, err := json.Marshal(struct {
			Method string `json:"method"`
			Path   string `json:"path"`
			Body   string `json:"body"`
		}{r.Method, r.URL.Path, string(body)})
		if err != nil {
			return
		}
		mu.Lock()
		_, _ = logFile.Write(append(line, '\n'))
		mu.Unlock()
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		record(r, body)
		if failPath != "" && r.URL.Path == failPath {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"fault_message":"injected failure"}`)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"fake","state":"Running","vmm_version":"fake"}`)
			return
		}
		if r.Method == http.MethodPut && r.URL.Path == "/snapshot/create" {
			var req struct {
				SnapshotPath string `json:"snapshot_path"`
				MemFilePath  string `json:"mem_file_path"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"fault_message":"bad snapshot request"}`)
				return
			}
			for _, p := range []string{req.SnapshotPath, req.MemFilePath} {
				if err := os.WriteFile(p, []byte("fake-snapshot"), 0o600); err != nil {
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprintf(w, `{"fault_message":%q}`, err.Error())
					return
				}
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := &http.Server{Handler: handler}
	if err := srv.Serve(ln); err != nil {
		fmt.Fprintf(os.Stderr, "fake-vmm: serve: %v\n", err)
		os.Exit(2)
	}
}

// recordedRequest is one request the fake VMM received.
type recordedRequest struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Body   string `json:"body"`
}

// readRequests reads the fake VMM request log.
func readRequests(t *testing.T, path string) []recordedRequest {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fake VMM request log: %v", err)
	}
	var reqs []recordedRequest
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var r recordedRequest
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("decode fake VMM request log line %q: %v", line, err)
		}
		reqs = append(reqs, r)
	}
	return reqs
}

// nonPingRequests drops the GET / readiness probes.
func nonPingRequests(reqs []recordedRequest) []recordedRequest {
	out := reqs[:0:0]
	for _, r := range reqs {
		if r.Method == http.MethodGet && r.Path == "/" {
			continue
		}
		out = append(out, r)
	}
	return out
}
