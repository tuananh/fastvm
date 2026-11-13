// Package api exposes the fastvm HTTP/JSON API over a unix socket.
package api

import (
	"context"
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

func (s *Server) routes() {
	s.mux.HandleFunc("GET /v1/version", s.handleVersion)
	s.mux.HandleFunc("GET /v1/images", s.handleListImages)
	s.mux.HandleFunc("POST /v1/images/build", s.handleBuildImage)
	s.mux.HandleFunc("POST /v1/images/ingest", s.handleIngest)
	s.mux.HandleFunc("DELETE /v1/images", s.handleRemoveImage)
	s.mux.HandleFunc("GET /v1/sandboxes", s.handleList)
	s.mux.HandleFunc("POST /v1/sandboxes", s.handleCreate)
	s.mux.HandleFunc("GET /v1/sandboxes/{ref}", s.handleGet)
	s.mux.HandleFunc("DELETE /v1/sandboxes/{ref}", s.handleDelete)
	s.mux.HandleFunc("POST /v1/sandboxes/{ref}/exec", s.handleExec)
	s.mux.HandleFunc("POST /v1/sandboxes/{ref}/shell", s.handleShell)
	s.mux.HandleFunc("POST /v1/sandboxes/{ref}/sleep", s.handleSleep)
	s.mux.HandleFunc("POST /v1/sandboxes/{ref}/wake", s.handleWake)
	s.mux.HandleFunc("GET /v1/events", s.handleEvents)
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

func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": "0.1.0", "name": "fastvm"})
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

type buildReq struct {
	Dockerfile string `json:"dockerfile"`
	Context    string `json:"context"`
	Tag        string `json:"tag"`
	SizeGB     int    `json:"size_gb"`
}

func (s *Server) handleBuildImage(w http.ResponseWriter, r *http.Request) {
	var req buildReq
	if err := decode(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	disk, err := s.images.BuildImage(r.Context(), manager.BuildOptions{
		Dockerfile: req.Dockerfile, ContextDir: req.Context, Tag: req.Tag, SizeGB: req.SizeGB,
	})
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
	Name        string              `json:"name"`
	Image       string              `json:"image"`
	VCPUs       int                 `json:"vcpus"`
	MemoryMB    int                 `json:"memory_mb"`
	Ports       []types.PortMapping `json:"ports"`
	Cmd         []string            `json:"cmd"`
	Env         map[string]string   `json:"env"`
	NoAutoSleep bool                `json:"no_auto_sleep"`
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if err := decode(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	sb, err := s.rt.Create(r.Context(), manager.CreateOptions{
		Name: req.Name, Image: req.Image, VCPUs: req.VCPUs, MemoryMB: req.MemoryMB,
		Ports: req.Ports, Cmd: req.Cmd, Env: req.Env, NoAutoSleep: req.NoAutoSleep,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sb)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	sb, err := s.rt.Get(r.Context(), r.PathValue("ref"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sb)
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
