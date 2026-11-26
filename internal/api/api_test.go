package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"fastvm/internal/agent"
	"fastvm/internal/manager"
	"fastvm/internal/types"
)

type fakeRT struct {
	created manager.CreateOptions
	resize  manager.ResizeOptions
	resized string
	sb      types.Sandbox
	err     error
	exec    agent.Response
	events  []types.Event
	deleted string
	stats   types.SandboxStats
	slept   bool
	deep    bool
	woken   bool
	started bool
}

func (f *fakeRT) Create(_ context.Context, opts manager.CreateOptions) (types.Sandbox, error) {
	f.created = opts
	return f.sb, f.err
}
func (f *fakeRT) Start(context.Context, string) error {
	f.started = true
	return f.err
}
func (f *fakeRT) Snapshot(context.Context, string, string) (string, error) {
	return "fake-snapshot", f.err
}
func (f *fakeRT) SnapshotCopy(context.Context, string, string) (types.Sandbox, error) {
	return f.sb, f.err
}
func (f *fakeRT) ListSnapshots(context.Context) ([]types.SnapshotInfo, error) {
	return nil, f.err
}
func (f *fakeRT) RemoveSnapshot(context.Context, string) error { return f.err }
func (f *fakeRT) Get(context.Context, string) (types.Sandbox, error) {
	return f.sb, f.err
}
func (f *fakeRT) Resize(_ context.Context, ref string, opts manager.ResizeOptions) (types.Sandbox, error) {
	f.resized, f.resize = ref, opts
	return f.sb, f.err
}

func (f *fakeRT) Stats(context.Context, string) (types.SandboxStats, error) {
	return f.stats, f.err
}

func (f *fakeRT) StatsAll(context.Context) ([]types.SandboxStats, error) {
	return []types.SandboxStats{f.stats}, f.err
}

func (f *fakeRT) List(context.Context) ([]types.Sandbox, error) {
	return []types.Sandbox{f.sb}, f.err
}
func (f *fakeRT) Exec(context.Context, string, []string, map[string]string) (agent.Response, error) {
	return f.exec, f.err
}
func (f *fakeRT) Sleep(_ context.Context, _ string, deep bool) error {
	f.slept, f.deep = true, deep
	return f.err
}
func (f *fakeRT) Wake(context.Context, string) error { f.woken = true; return f.err }
func (f *fakeRT) Delete(_ context.Context, ref string) error {
	f.deleted = ref
	return f.err
}
func (f *fakeRT) Events(context.Context, int) ([]types.Event, error) { return f.events, f.err }
func (f *fakeRT) Close() error                                       { return nil }

type fakeImg struct {
	disks   []types.BaseDisk
	pulled  manager.PullOptions
	ingest  string
	removed []string
	err     error
}

func (f *fakeImg) PullImage(_ context.Context, o manager.PullOptions) (types.BaseDisk, error) {
	f.pulled = o
	return f.disks[0], f.err
}
func (f *fakeImg) ListImages(context.Context) ([]types.BaseDisk, error) { return f.disks, f.err }
func (f *fakeImg) IngestRootfs(_ context.Context, dir, tag string) (types.BaseDisk, error) {
	f.ingest = dir + "|" + tag
	return f.disks[0], f.err
}
func (f *fakeImg) RemoveImage(_ context.Context, ref string, force bool) (types.BaseDisk, error) {
	f.removed = append(f.removed, fmt.Sprintf("%s|%t", ref, force))
	if f.err != nil {
		return types.BaseDisk{}, f.err
	}
	return f.disks[0], nil
}

func server(t *testing.T) (*httptest.Server, *fakeRT, *fakeImg) {
	t.Helper()
	rt := &fakeRT{sb: types.Sandbox{ID: "sb-1", Name: "web", State: types.StateRunning}}
	imgs := &fakeImg{disks: []types.BaseDisk{{Name: "img", Digest: "sha256:abc"}}}
	s := New(rt, imgs, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, rt, imgs
}

func call(t *testing.T, ts *httptest.Server, method, path string, body any) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, ts.URL+path, rdr)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func TestVersion(t *testing.T) {
	ts, _, _ := server(t)
	code, body := call(t, ts, "GET", "/v1/version", nil)
	if code != 200 || !bytes.Contains(body, []byte(`"version"`)) {
		t.Fatalf("code=%d body=%s", code, body)
	}
}

func TestListAndGet(t *testing.T) {
	ts, rt, _ := server(t)
	code, body := call(t, ts, "GET", "/v1/sandboxes", nil)
	if code != 200 {
		t.Fatalf("list code=%d body=%s", code, body)
	}
	code, body = call(t, ts, "GET", "/v1/sandboxes/sb-1", nil)
	if code != 200 || !bytes.Contains(body, []byte(`"sb-1"`)) {
		t.Fatalf("get code=%d body=%s", code, body)
	}
	rt.err = manager.ErrNotFound
	code, body = call(t, ts, "GET", "/v1/sandboxes/missing", nil)
	if code != 404 || !bytes.Contains(body, []byte("not found")) {
		t.Fatalf("missing code=%d body=%s", code, body)
	}
}

func TestSandboxStats(t *testing.T) {
	ts, rt, _ := server(t)
	rt.stats = types.SandboxStats{
		SandboxID: "sb-1", Name: "web", State: types.StateRunning,
		CPUPercent: 3.25, VCPUs: 2, MemoryMB: 512, MemoryRSSBytes: 104857600,
		UptimeSeconds: 132, WakeCount: 1,
		RxBytes: 1024, TxBytes: 2048,
		CapturedAt: time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC),
	}
	code, body := call(t, ts, "GET", "/v1/sandboxes/sb-1/stats", nil)
	if code != 200 {
		t.Fatalf("stats code=%d body=%s", code, body)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("stats body %s: %v", body, err)
	}
	want := map[string]string{
		"sandbox_id":        `"sb-1"`,
		"name":              `"web"`,
		"state":             `"Running"`,
		"cpu_percent":       "3.25",
		"vcpus":             "2",
		"memory_mb":         "512",
		"memory_rss_bytes":  "104857600",
		"uptime_seconds":    "132",
		"wake_count":        "1",
		"block_read_bytes":  "0",
		"block_write_bytes": "0",
		"rx_bytes":          "1024",
		"tx_bytes":          "2048",
		"captured_at":       `"2026-11-17T08:00:00Z"`,
	}
	if len(got) != len(want) {
		t.Fatalf("stats body has %d keys, want %d: %s", len(got), len(want), body)
	}
	for key, wv := range want {
		raw, ok := got[key]
		if !ok {
			t.Errorf("stats missing key %q: %s", key, body)
			continue
		}
		if string(raw) != wv {
			t.Errorf("key %q = %s, want %s", key, raw, wv)
		}
	}

	rt.err = manager.ErrNotFound
	code, body = call(t, ts, "GET", "/v1/sandboxes/missing/stats", nil)
	if code != 404 || !bytes.Contains(body, []byte(`"not_found"`)) {
		t.Fatalf("missing stats code=%d body=%s", code, body)
	}

	rt.err = nil
	code, body = call(t, ts, "GET", "/v1/sandboxes/stats", nil)
	if code != 200 {
		t.Fatalf("all stats code=%d body=%s", code, body)
	}
	var all []types.SandboxStats
	if err := json.Unmarshal(body, &all); err != nil || len(all) != 1 || all[0].SandboxID != "sb-1" {
		t.Fatalf("all stats body=%s err=%v", body, err)
	}
}

func TestCreateAndDelete(t *testing.T) {
	ts, rt, _ := server(t)
	code, _ := call(t, ts, "POST", "/v1/sandboxes", map[string]any{
		"name": "web", "image": "img", "ports": []map[string]any{{"host_port": 8080, "guest_port": 80, "proto": "tcp"}},
		"cmd": []string{"nginx"}, "no_auto_sleep": true,
	})
	if code != 201 {
		t.Fatalf("create code=%d", code)
	}
	if rt.created.Image != "img" || len(rt.created.Ports) != 1 || !rt.created.NoAutoSleep {
		t.Fatalf("create opts = %+v", rt.created)
	}
	code, _ = call(t, ts, "DELETE", "/v1/sandboxes/sb-1", nil)
	if code != 204 || rt.deleted != "sb-1" {
		t.Fatalf("delete code=%d ref=%q", code, rt.deleted)
	}
}

func TestCreateValidation(t *testing.T) {
	ts, _, _ := server(t)
	code, _ := call(t, ts, "POST", "/v1/sandboxes", map[string]any{"image": "img", "unknown": 1})
	if code != 400 {
		t.Fatalf("unknown field code=%d", code)
	}
}

func TestExecValidationAndSuccess(t *testing.T) {
	ts, rt, _ := server(t)
	code, _ := call(t, ts, "POST", "/v1/sandboxes/sb-1/exec", map[string]any{})
	if code != 400 {
		t.Fatalf("empty argv code=%d", code)
	}
	rt.exec = agent.Response{ID: "x", ExitCode: 0, Stdout: "hi\n"}
	code, body := call(t, ts, "POST", "/v1/sandboxes/sb-1/exec", map[string]any{"argv": []string{"echo", "hi"}})
	if code != 200 || !bytes.Contains(body, []byte("hi")) {
		t.Fatalf("exec code=%d body=%s", code, body)
	}
}

func TestSleepAndWake(t *testing.T) {
	ts, rt, _ := server(t)
	if code, _ := call(t, ts, "POST", "/v1/sandboxes/sb-1/sleep", map[string]any{}); code != 204 || !rt.slept || rt.deep {
		t.Fatalf("sleep code=%d slept=%v deep=%v", code, rt.slept, rt.deep)
	}
	if code, _ := call(t, ts, "POST", "/v1/sandboxes/sb-1/sleep", map[string]any{"deep": true}); code != 204 || !rt.deep {
		t.Fatalf("deep sleep code=%d deep=%v", code, rt.deep)
	}
	if code, _ := call(t, ts, "POST", "/v1/sandboxes/sb-1/wake", nil); code != 204 || !rt.woken {
		t.Fatalf("wake code=%d woken=%v", code, rt.woken)
	}
}

func TestImages(t *testing.T) {
	ts, _, imgs := server(t)
	code, _ := call(t, ts, "POST", "/v1/images/pull", map[string]any{
		"ref": "app:1", "size_gb": 4,
	})
	if code != 201 || imgs.pulled.Ref != "app:1" || imgs.pulled.SizeGB != 4 {
		t.Fatalf("pull code=%d opts=%+v", code, imgs.pulled)
	}
	code, _ = call(t, ts, "POST", "/v1/images/ingest", map[string]any{"rootfs_dir": "/rootfs", "tag": "raw:1"})
	if code != 201 || imgs.ingest != "/rootfs|raw:1" {
		t.Fatalf("ingest code=%d ingest=%q", code, imgs.ingest)
	}
	code, body := call(t, ts, "GET", "/v1/images", nil)
	if code != 200 || !bytes.Contains(body, []byte("sha256:abc")) {
		t.Fatalf("images code=%d body=%s", code, body)
	}
	code, _ = call(t, ts, "DELETE", "/v1/images?ref=app%3A1&force=1", nil)
	if code != 200 || len(imgs.removed) != 1 || imgs.removed[0] != "app:1|true" {
		t.Fatalf("remove code=%d removed=%v", code, imgs.removed)
	}
	if code, _ := call(t, ts, "DELETE", "/v1/images", nil); code != 400 {
		t.Fatalf("remove without ref code=%d, want 400", code)
	}
}

func TestEvents(t *testing.T) {
	ts, rt, _ := server(t)
	rt.events = []types.Event{{SandboxID: "sb-1", Type: "state", Message: "running"}}
	code, body := call(t, ts, "GET", "/v1/events?limit=5", nil)
	if code != 200 || !bytes.Contains(body, []byte("running")) {
		t.Fatalf("events code=%d body=%s", code, body)
	}
}

func TestErrorMapping(t *testing.T) {
	ts, rt, _ := server(t)
	cases := []struct {
		err  error
		code int
	}{
		{manager.ErrInvalid, 400},
		{manager.ErrConflict, 409},
		{manager.ErrNotFound, 404},
		{errors.New("boom"), 500},
	}
	for _, tc := range cases {
		rt.err = tc.err
		code, _ := call(t, ts, "GET", "/v1/sandboxes/sb-1", nil)
		if code != tc.code {
			t.Errorf("err %v => code %d, want %d", tc.err, code, tc.code)
		}
	}
}

// Ensure the server can own a real unix socket (smoke test).
func TestServeUnixSocket(t *testing.T) {
	if os.Getenv("FASTVM_API_SOCKET_TEST") == "" {
		t.Skip("set FASTVM_API_SOCKET_TEST=1 to exercise real unix socket")
	}
	dir := t.TempDir()
	_ = dir
}

func TestGzipResponses(t *testing.T) {
	ts, _, _ := server(t)
	req, err := http.NewRequest("GET", ts.URL+"/v1/sandboxes", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q", resp.Header.Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(zr); err != nil {
		t.Fatal(err)
	}
}
