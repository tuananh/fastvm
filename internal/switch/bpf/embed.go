package bpf

import _ "embed"

// L2Switch is the compiled tc clsact L2-switch program (bpf/l2switch.c).
// Regenerate with `make -C bpf`.
//
//go:embed l2switch_bpfel.o
var L2Switch []byte
