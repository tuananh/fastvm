package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"fastvm/internal/types"
)

// FVMFile is a declarative sandbox description, loaded by `fastvmctl run -f`.
//
// The file is strict YAML (unknown keys are rejected). Host environment
// references of the form ${NAME} are expanded in env values.
type FVMFile struct {
	Version      int               `yaml:"version"`
	Name         string            `yaml:"name"`
	Image        string            `yaml:"image"`
	Entrypoint   []string          `yaml:"entrypoint"`
	Command      []string          `yaml:"command"`
	Workdir      string            `yaml:"workdir"`
	Env          map[string]string `yaml:"env"`
	Volumes      []FVMVolume       `yaml:"volumes"`
	Ports        []FVMPort         `yaml:"ports"`
	Network      *FVMNetwork       `yaml:"network"`
	Resources    *FVMResources     `yaml:"resources"`
	NoAutoSleep  bool              `yaml:"no_auto_sleep"`
	UserData     string            `yaml:"user_data"`
	UserDataFile string            `yaml:"user_data_file"`
}

type FVMVolume struct {
	Host     string `yaml:"host"`
	Guest    string `yaml:"guest"`
	Size     string `yaml:"size"`
	Mount    string `yaml:"mount"`
	Name     string `yaml:"name"`
	ReadOnly bool   `yaml:"read_only"`
}

type FVMPort struct {
	Host     int    `yaml:"host"`
	Guest    int    `yaml:"guest"`
	Protocol string `yaml:"protocol"`
}

type FVMNetwork struct {
	Enabled     *bool    `yaml:"enabled"`
	AllowHosts  []string `yaml:"allow_hosts"`
	AutoPublish bool     `yaml:"auto_publish"`
}

type FVMResources struct {
	VCPUs    int `yaml:"vcpus"`
	MemoryMB int `yaml:"memory_mb"`
}

// FVMSpec is the resolved, CLI-ready form of an FVMFile.
type FVMSpec struct {
	Name           string
	Image          string
	VCPUs          int
	MemoryMB       int
	Entrypoint     []string
	Command        []string
	Workdir        string
	Env            map[string]string
	Volumes        []types.Volume
	DataVolumes    []types.DataVolume
	Ports          []types.PortMapping
	NetworkEnabled bool
	AllowHosts     []string
	AutoPublish    bool
	NoAutoSleep    bool
	UserData       string
}

var fvmEnvRefRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// LoadFVMFile reads and validates a declarative sandbox file. Relative host
// volume paths are resolved against the file's directory.
func LoadFVMFile(path string) (FVMSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return FVMSpec{}, fmt.Errorf("read fvmfile: %w", err)
	}
	var doc FVMFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return FVMSpec{}, fmt.Errorf("parse fvmfile: %w", err)
	}
	if doc.Version != 0 && doc.Version != 1 {
		return FVMSpec{}, fmt.Errorf("unsupported fvmfile version %d", doc.Version)
	}
	if strings.TrimSpace(doc.Image) == "" {
		return FVMSpec{}, errors.New("fvmfile: image is required")
	}
	baseDir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return FVMSpec{}, err
	}
	name := doc.Name
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}

	if doc.Workdir != "" {
		if !filepath.IsAbs(doc.Workdir) || filepath.Clean(doc.Workdir) != doc.Workdir || doc.Workdir == "/" {
			return FVMSpec{}, fmt.Errorf("fvmfile: workdir %q must be an absolute clean path below /", doc.Workdir)
		}
	}
	spec := FVMSpec{
		Name: name, Image: doc.Image,
		Entrypoint: append([]string(nil), doc.Entrypoint...),
		Command:    append([]string(nil), doc.Command...),
		Workdir:    doc.Workdir,
	}
	if doc.Resources != nil {
		spec.VCPUs, spec.MemoryMB = doc.Resources.VCPUs, doc.Resources.MemoryMB
	}
	if doc.NoAutoSleep {
		spec.NoAutoSleep = true
	}
	if doc.UserData != "" && doc.UserDataFile != "" {
		return FVMSpec{}, errors.New("fvmfile: user_data and user_data_file are mutually exclusive")
	}
	if doc.UserDataFile != "" {
		p := doc.UserDataFile
		if !filepath.IsAbs(p) {
			p = filepath.Join(baseDir, p)
		}
		info, err := os.Stat(p)
		if err != nil {
			return FVMSpec{}, fmt.Errorf("fvmfile: read user_data_file: %w", err)
		}
		if info.Size() > types.MaxUserDataBytes {
			return FVMSpec{}, fmt.Errorf("fvmfile: user_data_file exceeds %d bytes", types.MaxUserDataBytes)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return FVMSpec{}, fmt.Errorf("fvmfile: read user_data_file: %w", err)
		}
		if strings.TrimSpace(string(data)) == "" {
			return FVMSpec{}, errors.New("fvmfile: user_data_file is empty")
		}
		spec.UserData = string(data)
	} else {
		spec.UserData = doc.UserData
	}
	if spec.UserData != "" {
		if strings.TrimSpace(spec.UserData) == "" {
			return FVMSpec{}, errors.New("fvmfile: user_data is empty")
		}
		if len(spec.UserData) > types.MaxUserDataBytes {
			return FVMSpec{}, fmt.Errorf("fvmfile: user_data exceeds %d bytes", types.MaxUserDataBytes)
		}
	}
	if len(doc.Env) > 0 {
		spec.Env = make(map[string]string, len(doc.Env))
		for key, val := range doc.Env {
			expanded, err := expandFVMEnv(val)
			if err != nil {
				return FVMSpec{}, fmt.Errorf("env %s: %w", key, err)
			}
			spec.Env[key] = expanded
		}
	}
	for _, vol := range doc.Volumes {
		if vol.Host != "" && vol.Size != "" {
			return FVMSpec{}, errors.New("fvmfile: volume cannot have both host and size")
		}
		if vol.Host == "" && vol.Size == "" {
			return FVMSpec{}, errors.New("fvmfile: volume needs host or size")
		}
		if vol.Size != "" {
			if vol.ReadOnly {
				return FVMSpec{}, errors.New("fvmfile: data volumes are always writable")
			}
			sizeGB, err := parseVolumeSizeGiB(vol.Size)
			if err != nil {
				return FVMSpec{}, fmt.Errorf("fvmfile: %w", err)
			}
			mount := vol.Mount
			if mount == "" {
				mount = "/data"
			}
			if !filepath.IsAbs(mount) || filepath.Clean(mount) != mount || mount == "/" {
				return FVMSpec{}, fmt.Errorf("fvmfile: volume mount %q must be absolute below /", mount)
			}
			spec.DataVolumes = append(spec.DataVolumes, types.DataVolume{
				Name: vol.Name, SizeGB: sizeGB, Mount: mount,
			})
			continue
		}
		if vol.Guest == "" {
			return FVMSpec{}, errors.New("fvmfile: each host volume needs guest")
		}
		host := vol.Host
		if !filepath.IsAbs(host) {
			host = filepath.Join(baseDir, host)
		}
		host, err = filepath.Abs(host)
		if err != nil {
			return FVMSpec{}, fmt.Errorf("volume %q: %w", vol.Host, err)
		}
		spec.Volumes = append(spec.Volumes, types.Volume{
			HostPath: host, GuestPath: vol.Guest, ReadOnly: vol.ReadOnly,
		})
	}
	for _, port := range doc.Ports {
		protocol := port.Protocol
		if protocol == "" {
			protocol = "tcp"
		}
		if port.Host < 1 || port.Host > 65535 || port.Guest < 1 || port.Guest > 65535 {
			return FVMSpec{}, fmt.Errorf("fvmfile: port %d:%d is out of range", port.Host, port.Guest)
		}
		if protocol != "tcp" && protocol != "udp" {
			return FVMSpec{}, fmt.Errorf("fvmfile: invalid port protocol %q", protocol)
		}
		spec.Ports = append(spec.Ports, types.PortMapping{
			HostPort: port.Host, GuestPort: port.Guest, Proto: protocol,
		})
	}
	if doc.Network != nil {
		spec.AllowHosts = append([]string(nil), doc.Network.AllowHosts...)
		spec.AutoPublish = doc.Network.AutoPublish
		spec.NetworkEnabled = len(spec.AllowHosts) > 0 || spec.AutoPublish
		if doc.Network.Enabled != nil {
			spec.NetworkEnabled = *doc.Network.Enabled
		}
		if !spec.NetworkEnabled && (len(spec.AllowHosts) > 0 || spec.AutoPublish) {
			return FVMSpec{}, errors.New("fvmfile: allow_hosts/auto_publish require network.enabled: true")
		}
		if !spec.NetworkEnabled && len(spec.Ports) > 0 {
			if doc.Network.Enabled != nil {
				return FVMSpec{}, errors.New("fvmfile: ports require network.enabled: true")
			}
			spec.NetworkEnabled = true
		}
	} else if len(spec.Ports) > 0 {
		// Published ports need a guest NIC.
		spec.NetworkEnabled = true
	}
	return spec, nil
}

// resolveWorkload combines the Docker-style entrypoint and command with
// CLI-supplied arguments. CLI args replace the default command and are
// appended to the entrypoint. Without an entrypoint, argv keeps the previous
// behavior: CLI args replace command entirely.
func resolveWorkload(entrypoint, command, cliArgs []string) []string {
	args := cliArgs
	if len(args) == 0 {
		args = command
	}
	out := make([]string, 0, len(entrypoint)+len(args))
	out = append(out, entrypoint...)
	out = append(out, args...)
	return out
}

func expandFVMEnv(value string) (string, error) {
	var missing []string
	out := fvmEnvRefRE.ReplaceAllStringFunc(value, func(match string) string {
		key := match[2 : len(match)-1]
		if v, ok := os.LookupEnv(key); ok {
			return v
		}
		missing = append(missing, key)
		return ""
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("unset environment variable(s): %s", strings.Join(missing, ", "))
	}
	return out, nil
}
