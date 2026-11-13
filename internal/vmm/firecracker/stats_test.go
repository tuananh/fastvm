package firecracker

import (
	"os/exec"
	"testing"
	"time"
)

func TestParseProcStat(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		utime   uint64
		stime   uint64
		wantErr bool
	}{
		{
			name:  "normal",
			line:  "1234 (firecracker) S 1 1234 1234 0 -1 4194304 100 0 0 0 42 7 0 0 20 0 1 0 1000 1000000 100 18446744073709551615",
			utime: 42,
			stime: 7,
		},
		{
			name:  "comm with spaces and parens",
			line:  "1234 (my vmm (v1)) R 1 1234 1234 0 -1 4194304 100 0 0 0 11 3 0 0 20 0 1 0 1000 1000000 100 0",
			utime: 11,
			stime: 3,
		},
		{name: "missing comm terminator", line: "1234 firecracker R 1", wantErr: true},
		{name: "too few fields", line: "1234 (fc) S 1 1234", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			utime, stime, err := parseProcStat(tc.line)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseProcStat: %v", err)
			}
			if utime != tc.utime || stime != tc.stime {
				t.Fatalf("got utime=%d stime=%d, want utime=%d stime=%d", utime, stime, tc.utime, tc.stime)
			}
		})
	}
}

func TestReadVMStatsBusyProcess(t *testing.T) {
	// A shell busy-loop must accumulate USER_HZ ticks within the timeout.
	cmd := exec.Command("/bin/sh", "-c", "i=0; while :; do i=$((i+1)); done")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start shell: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		st, err := readVMStats(cmd.Process.Pid)
		if err != nil {
			t.Fatalf("readVMStats: %v", err)
		}
		if st.VCPUTimeNS > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("busy process CPU time never advanced")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestReadVMStatsExitedProcess(t *testing.T) {
	cmd := exec.Command("/bin/true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("/bin/true: %v", err)
	}
	if _, err := readVMStats(cmd.Process.Pid); err == nil {
		t.Fatal("expected error reading stats of an exited process")
	}
}

func TestVMMBlockIOExitedProcess(t *testing.T) {
	cmd := exec.Command("/bin/true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("/bin/true: %v", err)
	}
	if _, _, err := vmmBlockIO(cmd.Process.Pid); err == nil {
		t.Fatal("expected error reading /proc/<pid>/io of an exited process")
	}
}
