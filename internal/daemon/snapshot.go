package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"fastvm/internal/manager"
	"fastvm/internal/network"
	"fastvm/internal/types"
	"fastvm/internal/vmm"
	"fastvm/internal/volume"
)

const snapshotVersion = 1

// SnapshotMeta is the portable snapshot manifest written next to the CH
// snapshot and writable disk image.
type SnapshotMeta struct {
	Version        int       `json:"version"`
	ImageDigest    string    `json:"image_digest"`
	VCPUs          int       `json:"vcpus"`
	MemoryMB       int       `json:"memory_mb"`
	Workdir        string    `json:"workdir,omitempty"`
	NetworkEnabled bool      `json:"network_enabled"`
	AllowHosts     []string  `json:"allow_hosts,omitempty"`
	VsockCID       uint32    `json:"vsock_cid"`
	CreatedAt      time.Time `json:"created_at"`
}

var hostnameArgRE = regexp.MustCompile(`fastvm\.hostname=[^ ]+`)

const snapshotsDirName = "snapshots"

var snapshotNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// snapshotsRoot is the only location where saved snapshots live. Keeping
// it inside the data directory means reflink-capable filesystems can CoW the
// runtime disk into the snapshot and back out on restore.
func (d *Daemon) snapshotsRoot() string {
	return filepath.Join(d.cfg.DataDir, snapshotsDirName)
}

// resolveSnapshotName validates a client-supplied snapshot name and returns
// its managed path. Absolute/path-like values are rejected so the API never
// writes outside the data directory.
func (d *Daemon) resolveSnapshotName(name string) (string, error) {
	if name == "" {
		return "", errors.New("snapshot: name is required")
	}
	if filepath.IsAbs(name) || strings.ContainsRune(name, filepath.Separator) || name == "." || name == ".." {
		return "", fmt.Errorf("snapshot: invalid name %q", name)
	}
	if !snapshotNameRE.MatchString(name) {
		return "", fmt.Errorf("snapshot: invalid name %q (use letters, digits, dot, dash, underscore)", name)
	}
	return filepath.Join(d.snapshotsRoot(), name), nil
}

func (d *Daemon) newSnapshotName(sb *types.Sandbox) (string, error) {
	base := sb.Name
	if base == "" {
		base = "snapshot"
	}
	base = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, base)
	base = strings.Trim(base, ".-_")
	if base == "" {
		base = "snapshot"
	}
	if len(base) > 80 {
		base = base[:80]
	}
	for i := 0; i < 100; i++ {
		name := fmt.Sprintf("%s-%s", base, d.now().UTC().Format("20060102-150405"))
		if i > 0 {
			name = fmt.Sprintf("%s-%d", name, i+1)
		}
		if _, err := os.Stat(filepath.Join(d.snapshotsRoot(), name)); errors.Is(err, os.ErrNotExist) {
			return name, nil
		}
	}
	return "", errors.New("snapshot: could not allocate a unique name")
}

// Snapshot exports a sandbox's memory/CPU snapshot and writable disk into a
// managed snapshot under the data directory, returning its name. Running
// sandboxes are paused only for the snapshot and resumed afterwards.
func (d *Daemon) Snapshot(ctx context.Context, ref, name string) (string, error) {
	rt, err := d.runtimeFor(ctx, ref)
	if err != nil {
		return "", err
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if name == "" {
		name, err = d.newSnapshotName(&rt.sb)
		if err != nil {
			return "", err
		}
	}
	dir, err := d.resolveSnapshotName(name)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(d.snapshotsRoot(), 0o700); err != nil {
		return "", err
	}
	if err := d.snapshotLocked(ctx, rt, dir); err != nil {
		return "", err
	}
	return name, nil
}

// snapshotLocked exports a snapshot; the caller must hold rt.mu.
func (d *Daemon) snapshotLocked(ctx context.Context, rt *runtime, outDir string) error {
	sb := &rt.sb
	if len(sb.Volumes) > 0 {
		return fmt.Errorf("%w: snapshots with host volumes are not supported yet", manager.ErrConflict)
	}
	if len(sb.DataVolumes) > 0 {
		return fmt.Errorf("%w: snapshots with data volumes are not supported yet", manager.ErrConflict)
	}
	if sb.VolumePath == "" {
		return fmt.Errorf("%w: sandbox has no writable disk", manager.ErrConflict)
	}
	abs, err := filepath.Abs(outDir)
	if err != nil {
		return err
	}
	if abs != filepath.Clean(abs) {
		return errors.New("snapshot: output path must be clean")
	}
	if entries, err := os.ReadDir(abs); err == nil && len(entries) > 0 {
		return fmt.Errorf("snapshot: output directory %s is not empty", abs)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return err
	}
	snapDir := filepath.Join(abs, "snapshot")
	if err := os.MkdirAll(snapDir, 0o700); err != nil {
		return err
	}

	resume := false
	switch sb.State {
	case types.StateRunning, types.StateIdle:
		if rt.vm == nil {
			return fmt.Errorf("%w: no VMM to snapshot", manager.ErrConflict)
		}
		if err := rt.vm.Pause(ctx); err != nil {
			return fmt.Errorf("pause for snapshot: %w", err)
		}
		resume = true
	case types.StatePaused:
		if rt.vm == nil {
			return fmt.Errorf("%w: no VMM to snapshot", manager.ErrConflict)
		}
	case types.StateDormant:
		// Memory snapshot is already on disk.
	default:
		return fmt.Errorf("%w: cannot snapshot a %s sandbox", manager.ErrConflict, sb.State)
	}
	if resume {
		defer func() {
			resumeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := rt.vm.Resume(resumeCtx); err != nil {
				d.log.Warn("resume after snapshot failed", "sandbox", sb.ID, "err", err)
			}
		}()
	}

	if sb.State == types.StateDormant {
		if err := copyDirContents(sb.SnapshotDir, snapDir); err != nil {
			return fmt.Errorf("copy snapshot: %w", err)
		}
	} else {
		if _, err := rt.vm.Snapshot(ctx, snapDir); err != nil {
			return fmt.Errorf("snapshot: %w", err)
		}
	}
	if err := volume.CopyFile(ctx, sb.VolumePath, filepath.Join(abs, "disk.img")); err != nil {
		return fmt.Errorf("copy disk: %w", err)
	}
	meta := SnapshotMeta{
		Version: snapshotVersion, ImageDigest: sb.BaseDisk,
		VCPUs: sb.VCPUs, MemoryMB: sb.MemoryMB, Workdir: sb.Workdir,
		NetworkEnabled: sb.NetworkEnabled, AllowHosts: sb.AllowHosts,
		VsockCID: guestCID(sb.IP), CreatedAt: d.now(),
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(abs, "snapshot.json"), append(data, '\n'), 0o600); err != nil {
		return err
	}
	d.event(ctx, sb.ID, "state", "snapshot written: "+abs)
	return nil
}

// SnapshotCopy creates a live child from a running sandbox. The parent is
// paused only while the snapshot is captured, then resumed.
func (d *Daemon) SnapshotCopy(ctx context.Context, from, name string) (types.Sandbox, error) {
	rt, err := d.runtimeFor(ctx, from)
	if err != nil {
		return types.Sandbox{}, err
	}
	parentID := rt.sb.ID
	tmp := filepath.Join(d.cfg.DataDir, "snapshots", fmt.Sprintf("copy-%s-%d", parentID, time.Now().UnixNano()))
	defer os.RemoveAll(tmp)
	// Capture the parent under the runtime lock without treating the temporary
	// copy directory as a saved snapshot.
	rt.mu.Lock()
	cpErr := d.snapshotLocked(ctx, rt, tmp)
	rt.mu.Unlock()
	if cpErr != nil {
		return types.Sandbox{}, cpErr
	}
	return d.createFromSnapshot(ctx, manager.CreateOptions{
		Name: name, FromSnapshot: tmp, Start: boolPtr(true),
	})
}

func (d *Daemon) createFromSnapshot(ctx context.Context, opts manager.CreateOptions) (types.Sandbox, error) {
	data, err := os.ReadFile(filepath.Join(opts.FromSnapshot, "snapshot.json"))
	if err != nil {
		return types.Sandbox{}, fmt.Errorf("snapshot: read manifest: %w", err)
	}
	var meta SnapshotMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return types.Sandbox{}, fmt.Errorf("snapshot: parse manifest: %w", err)
	}
	if meta.Version != snapshotVersion {
		return types.Sandbox{}, fmt.Errorf("snapshot: unsupported version %d", meta.Version)
	}
	base, err := d.img.Get(ctx, meta.ImageDigest)
	if err != nil {
		return types.Sandbox{}, fmt.Errorf("%w: snapshot image %q: %v", manager.ErrNotFound, meta.ImageDigest, err)
	}
	id, err := newID()
	if err != nil {
		return types.Sandbox{}, err
	}
	name := opts.Name
	if name == "" {
		name, err = d.newSandboxName(ctx)
		if err != nil {
			return types.Sandbox{}, err
		}
	}
	dir := filepath.Join(d.cfg.DataDir, "sandboxes", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return types.Sandbox{}, err
	}
	sb := types.Sandbox{
		ID: id, Name: name, BaseDisk: base.Digest,
		VCPUs: meta.VCPUs, MemoryMB: meta.MemoryMB, Workdir: meta.Workdir,
		State: types.StateStarting, NetworkEnabled: meta.NetworkEnabled,
		AllowHosts:  meta.AllowHosts,
		VMMAPISock:  filepath.Join(dir, "vmm-api.sock"),
		VsockUDPath: filepath.Join(dir, "vsock.sock"),
		SerialPath:  filepath.Join(dir, "serial.log"),
		SnapshotDir: filepath.Join(dir, "memory"),
		CreatedAt:   d.now(), UpdatedAt: d.now(), LastActivityAt: d.now(),
	}
	volPath := filepath.Join(d.cfg.DataDir, "volumes", id+".img")
	if err := os.MkdirAll(filepath.Dir(volPath), 0o700); err != nil {
		return types.Sandbox{}, err
	}
	if err := volume.CopyFile(ctx, filepath.Join(opts.FromSnapshot, "disk.img"), volPath); err != nil {
		return types.Sandbox{}, fmt.Errorf("snapshot: import disk: %w", err)
	}
	sb.VolumePath = volPath
	if err := copyDirContents(filepath.Join(opts.FromSnapshot, "snapshot"), sb.SnapshotDir); err != nil {
		return types.Sandbox{}, fmt.Errorf("snapshot: import snapshot: %w", err)
	}
	if sb.NetworkEnabled {
		port, err := d.net.Create(ctx, network.CreateRequest{SandboxID: sb.ID, Ports: sb.Ports, AllowHosts: sb.AllowHosts})
		if err != nil {
			_ = os.RemoveAll(dir)
			_ = os.Remove(volPath)
			return types.Sandbox{}, fmt.Errorf("snapshot restore network: %w", err)
		}
		sb.NetnsPath, sb.VethHost, sb.VethIn = port.NetnsPath, port.VethHost, port.VethIn
		sb.TAPName, sb.MAC, sb.IP, sb.Gateway = port.TAPName, port.MAC, port.IP, port.Gateway
	}
	if err := patchSnapshotConfig(sb.SnapshotDir, &sb); err != nil {
		_ = d.net.Delete(context.Background(), id)
		_ = os.RemoveAll(dir)
		_ = os.Remove(volPath)
		return types.Sandbox{}, err
	}
	rt := d.newRuntime(id)
	rt.sb = sb
	if err := d.persistLocked(ctx, rt, &sb); err != nil {
		_ = d.net.Delete(context.Background(), id)
		_ = os.RemoveAll(dir)
		_ = os.Remove(volPath)
		return types.Sandbox{}, err
	}
	d.names.Store(id, sb.Name)
	d.mu.Lock()
	d.runtimes[id] = rt
	d.mu.Unlock()
	if err := d.restoreLocked(ctx, rt, meta.VsockCID); err != nil {
		d.cleanupFailed(context.Background(), &sb)
		return types.Sandbox{}, err
	}
	d.wg.Add(1)
	go d.monitor(rt)
	return rt.sb, nil
}

func (d *Daemon) restoreLocked(ctx context.Context, rt *runtime, cid uint32) error {
	sb := &rt.sb
	_ = d.persistLocked(ctx, rt, sb)
	vm, err := d.vmmD.Restore(ctx, vmm.RestoreConfig{
		SandboxID: sb.ID, Snapshot: vmm.Snapshot{Dir: sb.SnapshotDir},
		TAPName: sb.TAPName, VsockUDPath: sb.VsockUDPath,
		APISocket: sb.VMMAPISock, SerialPath: sb.SerialPath,
		Resume: true,
	})
	if err != nil {
		d.event(ctx, sb.ID, "error", fmt.Sprintf("vmm restore: %v", err))
		sb.State = types.StateFailed
		sb.Error = err.Error()
		return err
	}
	rt.vm = vm
	if cid == 0 {
		cid = guestCID(sb.IP)
	}
	ac, err := d.dial(ctx, sb.VsockUDPath, cid)
	if err != nil {
		_ = vm.Stop(context.Background())
		sb.State = types.StateFailed
		sb.Error = err.Error()
		return err
	}
	rt.ac = ac
	if err := ac.WaitReady(ctx); err != nil {
		_ = vm.Stop(context.Background())
		sb.State = types.StateFailed
		sb.Error = err.Error()
		return err
	}
	if err := ac.SetHostname(ctx, sb.ID[:min(12, len(sb.ID))]); err != nil {
		d.log.Warn("rename restored guest failed", "sandbox", sb.ID, "err", err)
	}
	if sb.NetworkEnabled && sb.IP != "" {
		if err := ac.SetNetwork(ctx, sb.IP+"/30", sb.Gateway); err != nil {
			d.log.Warn("reconfigure restored guest network failed", "sandbox", sb.ID, "err", err)
		}
	}
	now := d.now()
	sb.State = types.StateRunning
	sb.LastActivityAt = now
	sb.WakeCount = 0
	rt.warmupUntil = now.Add(d.cfg.Lifecycle.IdleDetection.Warmup.D())
	rt.lastTick = now
	rt.lastCPU = 0
	rt.lastBlock = 0
	rt.lastNetRx, rt.lastNetTx = 0, 0
	_ = d.persistLocked(ctx, rt, sb)
	d.event(ctx, sb.ID, "state", "sandbox restored")
	return nil
}

var snapshotHostnameRE = hostnameArgRE // keep regexp compiled once

func patchSnapshotConfig(snapshotDir string, sb *types.Sandbox) error {
	path := filepath.Join(snapshotDir, "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("snapshot: read CH config: %w", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("snapshot: parse CH config: %w", err)
	}
	if disks, ok := cfg["disks"].([]any); ok && len(disks) > 0 {
		if disk, ok := disks[0].(map[string]any); ok {
			disk["path"] = sb.VolumePath
		}
	}
	if serial, ok := cfg["serial"].(map[string]any); ok {
		serial["file"] = sb.SerialPath
	}
	if vsock, ok := cfg["vsock"].(map[string]any); ok {
		vsock["socket"] = sb.VsockUDPath
	}
	if sb.TAPName == "" {
		delete(cfg, "net")
	} else if nets, ok := cfg["net"].([]any); ok && len(nets) > 0 {
		if netcfg, ok := nets[0].(map[string]any); ok {
			netcfg["tap"] = sb.TAPName
			netcfg["mac"] = sb.MAC
		}
	} else {
		return errors.New("snapshot: snapshot has no NIC but the restored sandbox requested networking")
	}
	if payload, ok := cfg["payload"].(map[string]any); ok {
		if cmdline, ok := payload["cmdline"].(string); ok {
			cmdline = snapshotHostnameRE.ReplaceAllString(cmdline, "fastvm.hostname="+sb.ID)
			payload["cmdline"] = cmdline
		}
	}
	out, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func copyDirContents(src, dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return fmt.Errorf("snapshot: unexpected directory %s", entry.Name())
		}
		if err := copyFile(filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func boolPtr(v bool) *bool { return &v }

// ListSnapshots enumerates saved snapshots under the data directory.
func (d *Daemon) ListSnapshots(ctx context.Context) ([]types.SnapshotInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(d.snapshotsRoot())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []types.SnapshotInfo{}, nil
		}
		return nil, fmt.Errorf("snapshot: list: %w", err)
	}
	out := make([]types.SnapshotInfo, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !snapshotNameRE.MatchString(entry.Name()) {
			continue
		}
		dir := filepath.Join(d.snapshotsRoot(), entry.Name())
		data, err := os.ReadFile(filepath.Join(dir, "snapshot.json"))
		if err != nil {
			continue // not a complete snapshot
		}
		var meta SnapshotMeta
		if err := json.Unmarshal(data, &meta); err != nil {
			continue
		}
		created := meta.CreatedAt
		if created.IsZero() {
			if info, err := entry.Info(); err == nil {
				created = info.ModTime()
			}
		}
		out = append(out, types.SnapshotInfo{
			Name: entry.Name(), CreatedAt: created, ImageDigest: meta.ImageDigest,
			VCPUs: meta.VCPUs, MemoryMB: meta.MemoryMB, NetworkEnabled: meta.NetworkEnabled,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// RemoveSnapshot deletes one saved snapshot directory.
func (d *Daemon) RemoveSnapshot(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := d.resolveSnapshotName(name)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: snapshot %q", manager.ErrNotFound, name)
		}
		return err
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("snapshot: remove %q: %w", name, err)
	}
	return nil
}
