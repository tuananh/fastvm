package main

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"fastvm/internal/types"
)

// parseVolumeSizeGiB parses "8GiB", "8G", "8GB", or "8".
func parseVolumeSizeGiB(s string) (int, error) {
	v := strings.ToUpper(strings.TrimSpace(s))
	switch {
	case strings.HasSuffix(v, "GIB"):
		v = strings.TrimSuffix(v, "GIB")
	case strings.HasSuffix(v, "GB"):
		v = strings.TrimSuffix(v, "GB")
	case strings.HasSuffix(v, "G"):
		v = strings.TrimSuffix(v, "G")
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("invalid volume size %q (use GiB, e.g. 8GiB)", s)
	}
	return n, nil
}

// parseDataVolumeSpec parses "size=8GiB,mount=/data[,name=data]".
func parseDataVolumeSpec(spec string) (types.DataVolume, error) {
	var out types.DataVolume
	seenSize := false
	for _, part := range strings.Split(spec, ",") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return types.DataVolume{}, fmt.Errorf("invalid volume %q: expected key=value entries", spec)
		}
		switch strings.TrimSpace(k) {
		case "size":
			n, err := parseVolumeSizeGiB(v)
			if err != nil {
				return types.DataVolume{}, err
			}
			out.SizeGB, seenSize = n, true
		case "mount":
			out.Mount = strings.TrimSpace(v)
		case "name":
			out.Name = strings.TrimSpace(v)
		default:
			return types.DataVolume{}, fmt.Errorf("invalid volume %q: unknown key %q", spec, k)
		}
	}
	if !seenSize {
		return types.DataVolume{}, fmt.Errorf("invalid volume %q: size is required", spec)
	}
	if out.Mount == "" {
		out.Mount = "/data"
	}
	if !filepath.IsAbs(out.Mount) || filepath.Clean(out.Mount) != out.Mount || out.Mount == "/" {
		return types.DataVolume{}, fmt.Errorf("volume mount %q must be an absolute clean path below /", out.Mount)
	}
	return out, nil
}

// parseVolumeFlags splits repeatable --volume values into host-directory
// volumes and block data volumes.
func parseVolumeFlags(raw []string) ([]types.Volume, []types.DataVolume, error) {
	var hostRaw []string
	var data []types.DataVolume
	for _, spec := range raw {
		if strings.HasPrefix(spec, "/") {
			hostRaw = append(hostRaw, spec)
			continue
		}
		if !strings.Contains(spec, "=") {
			return nil, nil, fmt.Errorf("volume %q: expected an absolute host path or size=...", spec)
		}
		dv, err := parseDataVolumeSpec(spec)
		if err != nil {
			return nil, nil, err
		}
		data = append(data, dv)
	}
	hosts, err := parseVolumes(hostRaw)
	if err != nil {
		return nil, nil, err
	}
	return hosts, data, nil
}

// parseVolumes parses repeatable --volume flags. Accepted forms:
//
//	/path                  same path inside the guest
//	/host:/guest           explicit guest path
//	/host[:/guest][:ro]    explicit path with read-only mode
func parseVolumes(raw []string) ([]types.Volume, error) {
	out := make([]types.Volume, 0, len(raw))
	for _, spec := range raw {
		readOnly := false
		if strings.HasSuffix(spec, ":ro") {
			readOnly = true
			spec = strings.TrimSuffix(spec, ":ro")
		} else {
			spec = strings.TrimSuffix(spec, ":rw")
		}
		host, guest, ok := strings.Cut(spec, ":")
		if !ok {
			if !filepath.IsAbs(spec) || filepath.Clean(spec) != spec {
				return nil, fmt.Errorf("volume %q: single-path form must be an absolute clean host path", spec)
			}
			host, guest = spec, spec
		} else if host == "" || guest == "" {
			return nil, fmt.Errorf("volume %q: expected host:guest[:ro]", spec)
		}
		out = append(out, types.Volume{HostPath: host, GuestPath: guest, ReadOnly: readOnly})
	}
	return out, nil
}

var imageRefRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]*$`)

// ensureImage pulls an OCI image into the daemon's image store when it is not
// already present. No Docker daemon or CLI is involved.
func ensureImage(ref string) error {
	var disks []types.BaseDisk
	if err := getJSON("/v1/images", &disks); err != nil {
		return err
	}
	for _, disk := range disks {
		if disk.Digest == ref || disk.Tag == ref || disk.Name == ref {
			return nil
		}
		for _, tag := range disk.Tags {
			if tag == ref {
				return nil
			}
		}
	}
	if !imageRefRE.MatchString(ref) {
		return fmt.Errorf("image %q is not a valid image reference", ref)
	}
	if _, err := pullImageWithProgress(ref, func() (types.BaseDisk, error) {
		var pulled types.BaseDisk
		if err := do(http.MethodPost, "/v1/images/pull", map[string]any{"ref": ref}, &pulled); err != nil {
			return types.BaseDisk{}, fmt.Errorf("pull image %q: %w", ref, err)
		}
		return pulled, nil
	}); err != nil {
		return err
	}
	return nil
}

func parsePublishes(vals []string) ([]types.PortMapping, error) {
	var out []types.PortMapping
	for _, v := range vals {
		proto := "tcp"
		if i := strings.LastIndexByte(v, '/'); i >= 0 {
			proto = v[i+1:]
			v = v[:i]
		}
		parts := strings.Split(v, ":")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid --publish %q (want host:guest)", v)
		}
		hp, err1 := strconv.Atoi(parts[0])
		gp, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil || hp < 1 || gp < 1 {
			return nil, fmt.Errorf("invalid --publish %q", v)
		}
		if proto != "tcp" && proto != "udp" {
			return nil, fmt.Errorf("invalid protocol %q", proto)
		}
		out = append(out, types.PortMapping{HostPort: hp, GuestPort: gp, Proto: proto})
	}
	return out, nil
}

func parseEnvs(vals []string) (map[string]string, error) {
	if len(vals) == 0 {
		return nil, nil
	}
	env := make(map[string]string, len(vals))
	for _, v := range vals {
		i := strings.IndexByte(v, '=')
		if i <= 0 {
			return nil, fmt.Errorf("invalid --env %q (want K=V)", v)
		}
		env[v[:i]] = v[i+1:]
	}
	return env, nil
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

var errMissingImage = errors.New("--image is required")
