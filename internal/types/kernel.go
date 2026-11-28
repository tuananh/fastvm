package types

// KernelInfo describes one kernel file in the daemon kernel directory.
type KernelInfo struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256,omitempty"`
	Release   string `json:"release,omitempty"`
	Asset     string `json:"asset,omitempty"`
	Active    bool   `json:"active"`
}

// KernelList is the daemon's local kernel inventory.
type KernelList struct {
	Dir     string       `json:"dir"`
	Current string       `json:"current,omitempty"`
	Kernels []KernelInfo `json:"kernels"`
}

// KernelDownloadRequest asks the daemon to download one kernel asset.
type KernelDownloadRequest struct {
	Release string `json:"release"`
	Asset   string `json:"asset,omitempty"`
	Force   bool   `json:"force,omitempty"`
}

// KernelDownloadResult is returned after a download.
type KernelDownloadResult struct {
	Path    string `json:"path"`
	Release string `json:"release"`
	Asset   string `json:"asset"`
	SHA256  string `json:"sha256"`
}

// KernelUseResult is returned after switching the active kernel.
type KernelUseResult struct {
	Path    string `json:"path"`
	Current string `json:"current"`
}

// KernelRemoteInfo is one installable kernel asset from cloud-hypervisor/linux.
type KernelRemoteInfo struct {
	Release   string `json:"release"`
	Asset     string `json:"asset"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256,omitempty"`
}

// KernelRemoteList is the list of kernels available for download.
type KernelRemoteList struct {
	Kernels []KernelRemoteInfo `json:"kernels"`
}

// KernelRemoveResult describes a removed kernel.
type KernelRemoveResult struct {
	Release string `json:"release"`
	Path    string `json:"path"`
}
