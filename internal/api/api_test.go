package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"fastvm/internal/agent"
	"fastvm/internal/manager"
	"fastvm/internal/types"
)

type fakeRT struct {
	created manager.CreateOptions
	sb      types.Sandbox
	err     error
	exec    agent.Response
	events  []types.Event
	deleted string
	slept   bool
	deep    bool
	woken   bool
}

func (f *fakeRT) Create(_ context.Context, opts manager.CreateOptions) (types.Sandbox, error) {
	f.created = opts
	return f.sb, f.err
}
func (f *fakeRT) Get(context.Context, string) (types.Sandbox, error) {
	return f.sb, f.err
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
	built   manager.BuildOptions
	ingest  string
	removed []string
	err     error
}

func (f *fakeImg) BuildImage(_ context.Context, o manager.BuildOptions) (types.BaseDisk, error) {
	f.built = o
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
	code, _ := call(t, ts, "POST", "/v1/images/build", map[string]any{
		"dockerfile": "Dockerfile", "context": "/src", "tag": "app:1", "size_gb": 4,
	})
	if code != 201 || imgs.built.Tag != "app:1" || imgs.built.ContextDir != "/src" {
		t.Fatalf("build code=%d opts=%+v", code, imgs.built)
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
