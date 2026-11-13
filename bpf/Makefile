# fastvm eBPF build (docs/DESIGN.md §6.6).
#
# The compiled object is committed under internal/switch/bpf/ so that the Go
# loader can //go:embed it (Go embeds cannot reach outside the package
# directory, hence the generated object lives next to the loader, not here).
#
#   make -C bpf          # regenerate the embedded object (always recompiles:
#                        # the output depends on the clang version)
#   make -C bpf clean    # remove the embedded object
#
# Requires clang with a bpf target and libbpf headers (/usr/include/bpf).

CLANG ?= clang
BPF_CFLAGS ?= -O2 -g -target bpf -D__TARGET_ARCH_x86 -Wall -Werror
OUT := ../internal/switch/bpf/l2switch_bpfel.o

.PHONY: all clean FORCE

all: $(OUT)

# FORCE makes 'make -C bpf' always recompile with the current clang. The object
# is committed (Go cannot embed outside the package dir), and its bytes depend
# on the toolchain, so a mtime-only dependency would silently leave a stale
# object after a checkout or clang upgrade (see TestObjectMatchesSource).
$(OUT): l2switch.c FORCE
	$(CLANG) $(BPF_CFLAGS) -c $< -o $@

FORCE:

clean:
	rm -f $(OUT)
