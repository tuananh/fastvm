// Package initramfs builds a minimal Linux initramfs containing the static
// fastvm-agent as /init.
package initramfs

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	newcMagic = "070701"
	trailer   = "TRAILER!!!"
)

type entry struct {
	name string
	mode uint32
	data []byte
}

// Build writes a gzip-compressed newc CPIO initramfs containing agentPath as
// /init. It returns the output path.
func Build(agentPath, outDir string) (string, error) {
	agent, err := os.ReadFile(agentPath)
	if err != nil {
		return "", fmt.Errorf("initramfs: read agent: %w", err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	sum := sha256.Sum256(agent)
	name := "agent-" + hex.EncodeToString(sum[:]) + ".cpio.gz"
	dst := filepath.Join(outDir, name)
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}

	entries := []entry{
		{name: ".", mode: 0o040755 | 0o755},
		{name: "init", mode: 0o100000 | 0o755, data: agent},
		{name: "newroot", mode: 0o040000 | 0o755},
		{name: "proc", mode: 0o040000 | 0o755},
		{name: "sys", mode: 0o040000 | 0o755},
		{name: "dev", mode: 0o040000 | 0o755},
		{name: "run", mode: 0o040000 | 0o755},
	}
	var raw bytes.Buffer
	if err := writeNewc(&raw, entries); err != nil {
		return "", err
	}
	var gz bytes.Buffer
	zw, err := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	if err != nil {
		return "", err
	}
	if _, err := zw.Write(raw.Bytes()); err != nil {
		return "", err
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(outDir, ".initramfs-*.tmp")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(gz.Bytes()); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return "", err
	}
	ok = true
	return dst, nil
}

func writeNewc(w *bytes.Buffer, entries []entry) error {
	ino := uint32(1)
	for _, e := range entries {
		if err := writeEntry(w, e, ino); err != nil {
			return err
		}
		ino++
	}
	return writeEntry(w, entry{name: trailer}, ino)
}

func writeEntry(w *bytes.Buffer, e entry, ino uint32) error {
	if e.name == "" {
		return fmt.Errorf("initramfs: empty entry name")
	}
	name := e.name + "\x00"
	header := fmt.Sprintf("%s%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X",
		newcMagic,
		ino, e.mode, 0, 0, 1, 0, uint32(len(e.data)),
		0, 0, 0, 0, uint32(len(name)), 0)
	if len(header) != 110 {
		return fmt.Errorf("initramfs: bad header length %d", len(header))
	}
	if _, err := w.WriteString(header); err != nil {
		return err
	}
	if _, err := w.WriteString(name); err != nil {
		return err
	}
	pad := (4 - (len(header)+len(name))%4) % 4
	if _, err := w.Write(make([]byte, pad)); err != nil {
		return err
	}
	if _, err := w.Write(e.data); err != nil {
		return err
	}
	dataPad := (4 - len(e.data)%4) % 4
	if _, err := w.Write(make([]byte, dataPad)); err != nil {
		return err
	}
	return nil
}

// IsInitramfsMode reports whether the kernel cmdline asks the agent to mount
// the OCI root disk and enter it.
func IsInitramfsMode(cmdline []byte) bool {
	for _, field := range strings.Fields(string(cmdline)) {
		if field == "fastvm.initramfs=1" {
			return true
		}
	}
	return false
}
