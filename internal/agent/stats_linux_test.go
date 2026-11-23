//go:build linux

package agent

import "testing"

func TestParseMeminfoUsedMatchesFree(t *testing.T) {
	data := `MemTotal:         485276 kB
MemFree:          453648 kB
MemAvailable:     451888 kB
Buffers:            1024 kB
Cached:             6000 kB
SReclaimable:       1288 kB
`
	stats, err := parseMeminfo(data)
	if err != nil {
		t.Fatal(err)
	}
	const k = 1024
	wantTotal := uint64(485276 * k)
	wantUsed := uint64((485276 - 453648 - 1024 - 6000 - 1288) * k)
	if stats.MemoryTotalBytes != wantTotal {
		t.Errorf("total = %d, want %d", stats.MemoryTotalBytes, wantTotal)
	}
	if stats.MemoryUsedBytes != wantUsed {
		t.Errorf("used = %d, want %d", stats.MemoryUsedBytes, wantUsed)
	}
}
