// Package api exposes the fastvm REST/JSON API. The default transport is a unix
// socket; an optional TCP listener requires mutual TLS.
package api

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"fastvm/internal/agent"
	"fastvm/internal/manager"
	"fastvm/internal/types"
	"fastvm/internal/version"
)

// Server is the HTTP API server. It is transport-agnostic (tests use
// httptest); Serve binds the configured unix socket.
type Server struct {
	rt     manager.Runtime
	images manager.Images
	log    *slog.Logger
	mux    *http.ServeMux
}

// New builds the API server and registers routes.
func New(rt manager.Runtime, images manager.Images, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{rt: rt, images: images, log: log, mux: http.NewServeMux()}
	s.routes()
	return s
}

// Handler returns the HTTP handler (used by tests).
func (s *Server) Handler() http.Handler { return s.mux }

// MetricsProvider is implemented by daemons that expose Prometheus metrics.
type MetricsProvider interface {
	PrometheusMetrics(ctx context.Context) ([]byte, error)
}

// KernelManager is implemented by daemons that manage guest kernels.
type KernelManager interface {
	ListKernels(ctx context.Context) (types.KernelList, error)
	DownloadKernel(ctx context.Context, req types.KernelDownloadRequest) (types.KernelDownloadResult, error)
	UseKernel(ctx context.Context, release string) (types.KernelUseResult, error)
}

// ConsoleLogProvider is implemented by daemons that expose the host-captured
// guest serial console.
type ConsoleLogProvider interface {
	ConsoleLog(ctx context.Context, ref string) ([]byte, error)
}

// FileCopier is implemented by daemons that support guest file transfer.
type FileCopier interface {
	CopyToGuest(ctx context.Context, ref, path string, mode os.FileMode, r io.Reader) error
	CopyFromGuest(ctx context.Context, ref, path string, w io.Writer) (os.FileMode, int64, error)
	StatGuest(ctx context.Context, ref, path string) (os.FileMode, int64, error)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /metrics", s.handleMetrics)
	s.mux.HandleFunc("GET /v1/version", s.handleVersion)
	s.mux.HandleFunc("GET /v1/kernels", s.handleListKernels)
	s.mux.HandleFunc("POST /v1/kernels/download", s.handleDownloadKernel)
	s.mux.HandleFunc("POST /v1/kernels/use", s.handleUseKernel)
	s.mux.HandleFunc("GET /v1/images", s.handleListImages)
	s.mux.HandleFunc("POST /v1/images/pull", s.handlePullImage)
	s.mux.HandleFunc("POST /v1/images/ingest", s.handleIngest)
	s.mux.HandleFunc("DELETE /v1/images", s.handleRemoveImage)
	s.mux.HandleFunc("GET /v1/sandboxes", s.handleList)
	s.mux.HandleFunc("POST /v1/sandboxes", s.handleCreate)
	s.mux.HandleFunc("GET /v1/sandboxes/{ref}", s.handleGet)
	s.mux.HandleFunc("GET /v1/sandboxes/{ref}/stats", s.handleStats)
	s.mux.HandleFunc("PATCH /v1/sandboxes/{ref}/resources", s.handleResize)
	s.mux.HandleFunc("PUT /v1/sandboxes/{ref}/file", s.handleCopyTo)
	s.mux.HandleFunc("GET /v1/sandboxes/{ref}/file", s.handleCopyFrom)
	s.mux.HandleFunc("GET /v1/sandboxes/{ref}/console", s.handleConsole)
	s.mux.HandleFunc("DELETE /v1/sandboxes/{ref}", s.handleDelete)
	s.mux.HandleFunc("POST /v1/sandboxes/{ref}/exec", s.handleExec)
	s.mux.HandleFunc("POST /v1/sandboxes/{ref}/shell", s.handleShell)
	s.mux.HandleFunc("GET /v1/sandboxes/{ref}/shell/ws", s.handleShellWS)
	s.mux.HandleFunc("POST /v1/sandboxes/{ref}/sleep", s.handleSleep)
	s.mux.HandleFunc("POST /v1/sandboxes/{ref}/start", s.handleStart)
	s.mux.HandleFunc("GET /v1/snapshots", s.handleListSnapshots)
	s.mux.HandleFunc("DELETE /v1/snapshots/{name}", s.handleRemoveSnapshot)
	s.mux.HandleFunc("POST /v1/sandboxes/{ref}/snapshot", s.handleSnapshot)
	s.mux.HandleFunc("POST /v1/sandboxes/{ref}/snapshot/copy", s.handleSnapshotCopy)
	s.mux.HandleFunc("POST /v1/sandboxes/{ref}/wake", s.handleWake)
	s.mux.HandleFunc("GET /v1/events", s.handleEvents)
}

// ServeTLS listens on a TCP address using the supplied mTLS config and serves
// until ctx is cancelled.
func (s *Server) ServeTLS(ctx context.Context, addr string, tlsConfig *tls.Config) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen tcp %s: %w", addr, err)
	}
	tlsLn := tls.NewListener(ln, tlsConfig)
	srv := &http.Server{
		Handler:           s.mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.Serve(tlsLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Serve listens on a unix socket and serves until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, socketPath string) error {
	if err := os.MkdirAll(dirOf(socketPath), 0o755); err != nil {
		return err
	}
	_ = os.Remove(socketPath) // stale socket from a previous run
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen unix %s: %w", socketPath, err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		ln.Close()
		return err
	}
	srv := &http.Server{Handler: s.mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func dirOf(path string) string {
	if i := strings.LastIndexByte(path, '/'); i > 0 {
		return path[:i]
	}
	return "."
}

func (s *Server) handleCopyTo(w http.ResponseWriter, r *http.Request) {
	c, ok := s.rt.(FileCopier)
	if !ok {
		http.NotFound(w, r)
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path query parameter is required"})
		return
	}
	mode := os.FileMode(0o644)
	if raw := r.URL.Query().Get("mode"); raw != "" {
		v, err := strconv.ParseUint(raw, 8, 32)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "mode must be an octal file mode"})
			return
		}
		mode = os.FileMode(v)
	}
	if err := c.CopyToGuest(r.Context(), r.PathValue("ref"), path, mode, r.Body); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCopyFrom(w http.ResponseWriter, r *http.Request) {
	c, ok := s.rt.(FileCopier)
	if !ok {
		http.NotFound(w, r)
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path query parameter is required"})
		return
	}
	mode, size, err := c.StatGuest(r.Context(), r.PathValue("ref"), path)
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("X-Fastvm-Mode", strconv.FormatUint(uint64(mode.Perm()), 8))
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	if _, _, err := c.CopyFromGuest(r.Context(), r.PathValue("ref"), path, w); err != nil {
		// The response has already started; log-only is the best we can do here.
		s.log.Warn("copy from guest failed", "sandbox", r.PathValue("ref"), "path", path, "err", err)
	}
}

func (s *Server) handleConsole(w http.ResponseWriter, r *http.Request) {
	c, ok := s.rt.(ConsoleLogProvider)
	if !ok {
		http.NotFound(w, r)
		return
	}
	data, err := c.ConsoleLog(r.Context(), r.PathValue("ref"))
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) handleListKernels(w http.ResponseWriter, r *http.Request) {
	k, ok := s.rt.(KernelManager)
	if !ok {
		http.NotFound(w, r)
		return
	}
	list, err := k.ListKernels(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	if list.Kernels == nil {
		list.Kernels = []types.KernelInfo{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleDownloadKernel(w http.ResponseWriter, r *http.Request) {
	k, ok := s.rt.(KernelManager)
	if !ok {
		http.NotFound(w, r)
		return
	}
	var req types.KernelDownloadRequest
	if err := decode(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorEnvelope{Error: err.Error()})
		return
	}
	result, err := k.DownloadKernel(r.Context(), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleUseKernel(w http.ResponseWriter, r *http.Request) {
	k, ok := s.rt.(KernelManager)
	if !ok {
		http.NotFound(w, r)
		return
	}
	var req struct {
		Release string `json:"release"`
	}
	if err := decode(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorEnvelope{Error: err.Error()})
		return
	}
	if strings.TrimSpace(req.Release) == "" {
		writeJSON(w, http.StatusBadRequest, ErrorEnvelope{Error: "release is required"})
		return
	}
	result, err := k.UseKernel(r.Context(), req.Release)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	p, ok := s.rt.(MetricsProvider)
	if !ok {
		http.NotFound(w, r)
		return
	}
	data, err := p.PrometheusMetrics(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": version.String(), "name": "fastvm"})
}

func (s *Server) handleListImages(w http.ResponseWriter, r *http.Request) {
	imgs, err := s.images.ListImages(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	if imgs == nil {
		imgs = []types.BaseDisk{}
	}
	writeJSON(w, http.StatusOK, imgs)
}

type pullReq struct {
	Ref    string `json:"ref"`
	SizeGB int    `json:"size_gb"`
}

func (s *Server) handlePullImage(w http.ResponseWriter, r *http.Request) {
	var req pullReq
	if err := decode(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	disk, err := s.images.PullImage(r.Context(), manager.PullOptions{Ref: req.Ref, SizeGB: req.SizeGB})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, disk)
}

type ingestReq struct {
	RootfsDir string `json:"rootfs_dir"`
	Tag       string `json:"tag"`
}

func (s *Server) handleRemoveImage(w http.ResponseWriter, r *http.Request) {
	ref := strings.TrimSpace(r.URL.Query().Get("ref"))
	if ref == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ref query parameter is required"})
		return
	}
	force, _ := strconv.ParseBool(r.URL.Query().Get("force"))
	disk, err := s.images.RemoveImage(r.Context(), ref, force)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, disk)
}

func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	var req ingestReq
	if err := decode(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	disk, err := s.images.IngestRootfs(r.Context(), req.RootfsDir, req.Tag)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, disk)
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	list, err := s.rt.List(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	if list == nil {
		list = []types.Sandbox{}
	}
	writeJSON(w, http.StatusOK, list)
}

type createReq struct {
	Name           string              `json:"name"`
	Image          string              `json:"image"`
	Kernel         string              `json:"kernel"`
	VCPUs          int                 `json:"vcpus"`
	MaxVCPUs       int                 `json:"max_vcpus"`
	MemoryMB       int                 `json:"memory_mb"`
	MaxMemoryMB    int                 `json:"max_memory_mb"`
	Ports          []types.PortMapping `json:"ports"`
	Cmd            []string            `json:"cmd"`
	Env            map[string]string   `json:"env"`
	Workdir        string              `json:"workdir"`
	AutoPublish    bool                `json:"auto_publish"`
	NoAutoSleep    bool                `json:"no_auto_sleep"`
	Volumes        []types.Volume      `json:"volumes"`
	DataVolumes    []types.DataVolume  `json:"data_volumes"`
	Start          *bool               `json:"start"`
	NetworkEnabled *bool               `json:"network_enabled"`
	AllowHosts     []string            `json:"allow_hosts"`
	FromSnapshot   string              `json:"from_snapshot"`
	UserData       string              `json:"user_data"`
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if err := decode(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	sb, err := s.rt.Create(r.Context(), manager.CreateOptions{
		Name: req.Name, Image: req.Image, Kernel: req.Kernel,
		VCPUs: req.VCPUs, MaxVCPUs: req.MaxVCPUs, MemoryMB: req.MemoryMB, MaxMemoryMB: req.MaxMemoryMB,
		Ports: req.Ports, Volumes: req.Volumes, DataVolumes: req.DataVolumes, Cmd: req.Cmd, Env: req.Env, Workdir: req.Workdir, AutoPublish: req.AutoPublish, NoAutoSleep: req.NoAutoSleep,
		Start: req.Start, NetworkEnabled: req.NetworkEnabled, AllowHosts: req.AllowHosts,
		FromSnapshot: req.FromSnapshot, UserData: req.UserData,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sb)
}

func (s *Server) handleResize(w http.ResponseWriter, r *http.Request) {
	var req struct {
		VCPUs    int `json:"vcpus"`
		MemoryMB int `json:"memory_mb"`
	}
	if err := decode(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorEnvelope{Error: err.Error()})
		return
	}
	sb, err := s.rt.Resize(r.Context(), r.PathValue("ref"), manager.ResizeOptions{
		VCPUs: req.VCPUs, MemoryMB: req.MemoryMB,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sb)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	sb, err := s.rt.Get(r.Context(), r.PathValue("ref"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sb)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.rt.Stats(r.Context(), r.PathValue("ref"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.rt.Delete(r.Context(), r.PathValue("ref")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// shellRuntime is implemented by runtimes that support interactive PTY shells.
type shellRuntime interface {
	Shell(ctx context.Context, ref string, req agent.ShellRequest) (*agent.ShellSession, error)
}

func (s *Server) handleShell(w http.ResponseWriter, r *http.Request) {
	sr, ok := s.rt.(shellRuntime)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "shell not supported by this runtime"})
		return
	}
	var req agent.ShellRequest
	if err := decode(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	sess, err := sr.Shell(r.Context(), r.PathValue("ref"), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer sess.Close()

	hj, ok := w.(http.Hijacker)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming unsupported"})
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		s.log.Error("shell hijack", "err", err)
		return
	}
	if _, err := io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: fastvm-shell\r\n\r\n"); err != nil {
		_ = conn.Close()
		return
	}

	// Client -> guest (input/resize frames). On disconnect, close the session so
	// the guest shell gets SIGHUP instead of lingering.
	go func() {
		for {
			typ, payload, err := agent.ReadShellFrame(brw.Reader)
			if err != nil {
				_ = sess.Close()
				return
			}
			switch typ {
			case agent.ShellFrameInput:
				if _, err := sess.Write(payload); err != nil {
					_ = sess.Close()
					return
				}
			case agent.ShellFrameResize:
				var sz struct {
					Rows uint16 `json:"rows"`
					Cols uint16 `json:"cols"`
				}
				if err := json.Unmarshal(payload, &sz); err == nil {
					_ = sess.Resize(sz.Rows, sz.Cols)
				}
			}
		}
	}()

	// Guest -> client (output frames, then exit).
	buf := make([]byte, 32<<10)
	for {
		n, err := sess.Read(buf)
		if n > 0 {
			if werr := agent.WriteShellFrame(conn, agent.ShellFrameOutput, buf[:n]); werr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	var code [4]byte
	binary.BigEndian.PutUint32(code[:], uint32(int32(sess.ExitCode())))
	_ = agent.WriteShellFrame(conn, agent.ShellFrameExit, code[:])
	_ = conn.Close()
}

type execReq struct {
	Argv []string          `json:"argv"`
	Env  map[string]string `json:"env"`
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	var req execReq
	if err := decode(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if len(req.Argv) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "argv is required"})
		return
	}
	resp, err := s.rt.Exec(r.Context(), r.PathValue("ref"), req.Argv, req.Env)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

type sleepReq struct {
	Deep bool `json:"deep"`
}

func (s *Server) handleSleep(w http.ResponseWriter, r *http.Request) {
	var req sleepReq
	// Empty body is allowed (defaults to shallow sleep).
	if r.ContentLength > 0 {
		if err := decode(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	if err := s.rt.Sleep(r.Context(), r.PathValue("ref"), req.Deep); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListSnapshots(w http.ResponseWriter, r *http.Request) {
	snapshots, err := s.rt.ListSnapshots(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	if snapshots == nil {
		snapshots = []types.SnapshotInfo{}
	}
	writeJSON(w, http.StatusOK, snapshots)
}

func (s *Server) handleRemoveSnapshot(w http.ResponseWriter, r *http.Request) {
	if err := s.rt.RemoveSnapshot(r.Context(), r.PathValue("name")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if r.ContentLength > 0 {
		if err := decode(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	name, err := s.rt.Snapshot(r.Context(), r.PathValue("ref"), req.Name)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"snapshot": name})
}

func (s *Server) handleSnapshotCopy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decode(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	sb, err := s.rt.SnapshotCopy(r.Context(), r.PathValue("ref"), req.Name)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sb)
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	if err := s.rt.Start(r.Context(), r.PathValue("ref")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleWake(w http.ResponseWriter, r *http.Request) {
	if err := s.rt.Wake(r.Context(), r.PathValue("ref")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	events, err := s.rt.Events(r.Context(), limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	if events == nil {
		events = []types.Event{}
	}
	writeJSON(w, http.StatusOK, events)
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 8<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// ErrorEnvelope is returned by handlers on failure.
type ErrorEnvelope struct {
	Error string `json:"error"`
	Kind  string `json:"kind,omitempty"`
}

func writeErr(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	kind := ""
	switch {
	case errors.Is(err, manager.ErrNotFound):
		code, kind = http.StatusNotFound, "not_found"
	case errors.Is(err, manager.ErrInvalid):
		code, kind = http.StatusBadRequest, "invalid"
	case errors.Is(err, manager.ErrConflict):
		code, kind = http.StatusConflict, "conflict"
	}
	writeJSON(w, code, ErrorEnvelope{Error: err.Error(), Kind: kind})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ExecResponse mirrors agent.Response but keeps the API package decoupled.
type ExecResponse = agent.Response
