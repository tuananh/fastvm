// Command fastvmctl is the fastvm CLI client.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/fang"
	"github.com/spf13/cobra"

	"fastvm/internal/version"
)

func main() {
	root := newRootCmd()
	if err := fang.Execute(context.Background(), root,
		fang.WithVersion(version.String()), fang.WithErrorHandler(cliErrorHandler)); err != nil {
		os.Exit(1)
	}
}

// cliErrorHandler preserves multi-line guidance blocks from daemon errors.
// fang's default handler reflows them into one paragraph.
func cliErrorHandler(w io.Writer, styles fang.Styles, err error) {
	text := err.Error()
	if !strings.Contains(text, "\n") {
		fang.DefaultErrorHandler(w, styles, err)
		return
	}
	text = strings.TrimSpace(text)
	if text != "" {
		text = strings.ToUpper(text[:1]) + text[1:]
	}
	fmt.Fprintln(w, styles.ErrorHeader.String())
	fmt.Fprintln(w, text)
	fmt.Fprintln(w)
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "fastvmctl",
		Short: "Micro-VM sandbox runtime",
		Long: `fastvmctl controls fastvmd and its Cloud Hypervisor sandboxes.

Create sandboxes from OCI images on demand, choose networking and egress
policy, attach a PTY, or snapshot a running sandbox and restore copies.

Run without a command to see the full command reference.

Environment:
  FASTVM_HOST       HTTPS API endpoint (for example https://127.0.0.1:8443);
                    unset uses the local unix socket
  FASTVM_CERT_PATH  directory containing ca.pem, cert.pem, key.pem for mTLS
  FASTVM_SOCKET     unix socket path (default /run/fastvm/fastvm.sock)`,
		Version: version.String(),
	}
	root.PersistentFlags().StringVar(&socketFlag, "socket", "",
		"fastvmd unix socket (overrides FASTVM_HOST; default: $FASTVM_SOCKET or "+defaultSocket+")")
	root.AddCommand(
		newImageCmd(),
		newKernelCmd(),
		newRunCmd(),
		newSnapshotCmd(),
		newListCmd(),
		newGetCmd(),
		newCpCmd(),
		newLogsCmd(),
		newExecCmd(),
		newShellCmd(),
		newPauseCmd(),
		newResumeCmd(),
		newDeleteCmd(),
		newEventsCmd(),
		newVersionCmd(),
	)
	return root
}
