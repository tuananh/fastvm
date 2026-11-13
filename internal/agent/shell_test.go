package agent

import (
	"bytes"
	"testing"
)

func TestShellFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	payload := []byte("hello\x00world")
	if err := WriteShellFrame(&buf, ShellFrameOutput, payload); err != nil {
		t.Fatalf("WriteShellFrame: %v", err)
	}
	typ, got, err := ReadShellFrame(&buf)
	if err != nil {
		t.Fatalf("ReadShellFrame: %v", err)
	}
	if typ != ShellFrameOutput {
		t.Errorf("type = %q, want %q", typ, ShellFrameOutput)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("payload = %q, want %q", got, payload)
	}
}

func TestShellFrameTooLarge(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteShellFrame(&buf, ShellFrameInput, make([]byte, ShellMaxFrame+1)); err == nil {
		t.Fatal("expected oversize payload error")
	}
}
