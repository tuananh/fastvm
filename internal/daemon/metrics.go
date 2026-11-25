package daemon

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"fastvm/internal/types"
)

// PrometheusMetrics renders the daemon and per-sandbox counters in Prometheus
// text exposition format.
func (d *Daemon) PrometheusMetrics(ctx context.Context) ([]byte, error) {
	sandboxes, err := d.List(ctx)
	if err != nil {
		return nil, err
	}
	images, err := d.img.List(ctx)
	if err != nil {
		return nil, err
	}
	volumeCount, volumeBytes := volumeCache(sandboxes)

	// Snapshot runtimes once; live counters come from the cached sampler.
	d.mu.Lock()
	runtimes := make(map[string]*runtime, len(d.runtimes))
	for id, rt := range d.runtimes {
		runtimes[id] = rt
	}
	d.mu.Unlock()

	// Prefer live runtime records over persisted state, which can lag between
	// samples and state transitions.
	for i := range sandboxes {
		rt := runtimes[sandboxes[i].ID]
		if rt == nil {
			continue
		}
		rt.mu.Lock()
		if rt.sb.ID != "" {
			sandboxes[i] = rt.sb
		}
		rt.mu.Unlock()
	}

	var b strings.Builder
	writeHelpType(&b, "fastvm_up", "1 if fastvmd is serving metrics.", "gauge")
	fmt.Fprintln(&b, "fastvm_up 1")

	states := []types.State{
		types.StateCreated, types.StateStarting, types.StateRunning,
		types.StateIdle, types.StatePaused, types.StateDormant,
		types.StateStopping, types.StateStopped, types.StateFailed,
	}
	counts := make(map[types.State]int, len(states))
	for _, sb := range sandboxes {
		counts[sb.State]++
	}
	writeHelpType(&b, "fastvm_sandboxes", "Number of sandboxes by state.", "gauge")
	for _, state := range states {
		fmt.Fprintf(&b, "fastvm_sandboxes{state=%q} %d\n", string(state), counts[state])
	}

	writeHelpType(&b, "fastvm_images", "Number of base images.", "gauge")
	fmt.Fprintf(&b, "fastvm_images %d\n", len(images))
	var imageBytes int64
	for _, disk := range images {
		imageBytes += disk.SizeBytes
	}
	writeHelpType(&b, "fastvm_image_cache_bytes", "Logical size of base images.", "gauge")
	fmt.Fprintf(&b, "fastvm_image_cache_bytes %d\n", imageBytes)
	writeHelpType(&b, "fastvm_volumes", "Number of writable sandbox volumes.", "gauge")
	fmt.Fprintf(&b, "fastvm_volumes %d\n", volumeCount)
	writeHelpType(&b, "fastvm_volume_cache_bytes", "Logical size of writable sandbox volumes.", "gauge")
	fmt.Fprintf(&b, "fastvm_volume_cache_bytes %d\n", volumeBytes)

	sort.Slice(sandboxes, func(i, j int) bool { return sandboxes[i].ID < sandboxes[j].ID })
	writeHelpType(&b, "fastvm_sandbox_info", "Sandbox identity and configuration.", "gauge")
	for _, sb := range sandboxes {
		fmt.Fprintf(&b, "fastvm_sandbox_info{sandbox_id=%q,sandbox_name=%q,state=%q,memory_mb=%q,vcpus=%q} 1\n",
			sb.ID, sb.Name, string(sb.State), strconv.Itoa(sb.MemoryMB), strconv.Itoa(sb.VCPUs))
	}
	writeHelpType(&b, "fastvm_sandbox_cpu_percent", "Cached sandbox CPU utilization percent.", "gauge")
	writeHelpType(&b, "fastvm_sandbox_memory_rss_bytes", "VMM resident set size in bytes.", "gauge")
	writeHelpType(&b, "fastvm_sandbox_guest_memory_used_bytes", "Guest-used memory in bytes (free semantics).", "gauge")
	writeHelpType(&b, "fastvm_sandbox_guest_memory_total_bytes", "Guest-visible total memory in bytes.", "gauge")
	writeHelpType(&b, "fastvm_sandbox_guest_memory_ballooned_bytes", "Memory currently inflated into the virtio balloon.", "gauge")
	writeHelpType(&b, "fastvm_sandbox_network_rx_bytes_total", "Guest network receive bytes.", "counter")
	writeHelpType(&b, "fastvm_sandbox_network_tx_bytes_total", "Guest network transmit bytes.", "counter")
	writeHelpType(&b, "fastvm_sandbox_wakes_total", "Sandbox wake count.", "counter")
	writeHelpType(&b, "fastvm_sandbox_sleeps_total", "Sandbox sleep count.", "counter")
	writeHelpType(&b, "fastvm_sandbox_execs_total", "Guest exec attempt count.", "counter")
	for _, sb := range sandboxes {
		var cpuPct float64
		var rss, rx, tx, guestUsed, guestTotal, ballooned uint64
		if rt := runtimes[sb.ID]; rt != nil {
			rt.mu.Lock()
			cpuPct = rt.cpuPct
			rss, rx, tx = rt.memoryRSSBytes, rt.rxBytes, rt.txBytes
			guestUsed, guestTotal, ballooned = rt.guestMemoryUsedBytes, rt.guestMemoryTotalBytes, rt.guestMemoryBalloonedBytes
			rt.mu.Unlock()
		}
		labels := fmt.Sprintf("sandbox_id=%q", sb.ID)
		fmt.Fprintf(&b, "fastvm_sandbox_cpu_percent{%s} %g\n", labels, cpuPct)
		fmt.Fprintf(&b, "fastvm_sandbox_memory_rss_bytes{%s} %d\n", labels, rss)
		fmt.Fprintf(&b, "fastvm_sandbox_guest_memory_used_bytes{%s} %d\n", labels, guestUsed)
		fmt.Fprintf(&b, "fastvm_sandbox_guest_memory_total_bytes{%s} %d\n", labels, guestTotal)
		fmt.Fprintf(&b, "fastvm_sandbox_guest_memory_ballooned_bytes{%s} %d\n", labels, ballooned)
		fmt.Fprintf(&b, "fastvm_sandbox_network_rx_bytes_total{%s} %d\n", labels, rx)
		fmt.Fprintf(&b, "fastvm_sandbox_network_tx_bytes_total{%s} %d\n", labels, tx)
		fmt.Fprintf(&b, "fastvm_sandbox_wakes_total{%s} %d\n", labels, sb.WakeCount)
		fmt.Fprintf(&b, "fastvm_sandbox_sleeps_total{%s} %d\n", labels, sb.SleepCount)
		fmt.Fprintf(&b, "fastvm_sandbox_execs_total{%s} %d\n", labels, sb.ExecCount)
	}
	return []byte(b.String()), nil
}

func writeHelpType(b *strings.Builder, name, help, typ string) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s %s\n", name, typ)
}

func volumeCache(sandboxes []types.Sandbox) (count int, bytes int64) {
	for _, sb := range sandboxes {
		if sb.VolumePath == "" {
			continue
		}
		st, err := os.Stat(sb.VolumePath)
		if err != nil || st.IsDir() {
			continue
		}
		count++
		bytes += st.Size()
	}
	return count, bytes
}
