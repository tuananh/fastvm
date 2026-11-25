package main

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/spf13/cobra"

	"fastvm/internal/types"
)

func newModifyCmd() *cobra.Command {
	var cpus, memory int
	cmd := &cobra.Command{
		Use:   "modify <sandbox>",
		Short: "Resize a running sandbox",
		Example: `  fastvmctl modify web --cpu 4
  fastvmctl modify web --memory 1024
  fastvmctl modify web --cpu 4 --memory 1024`,
		Long: `Change the live CPU or memory allocation of a sandbox.

CPU can be raised or lowered within the sandbox's --max-cpu ceiling.
Memory can be grown up to --max-memory; shrinking below boot memory is rejected
by Cloud Hypervisor.
`,
		Args: exactArgsWithHelp(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			changedCPU := cmd.Flags().Changed("cpu")
			changedMemory := cmd.Flags().Changed("memory")
			if !changedCPU && !changedMemory {
				return fmt.Errorf("specify --cpu and/or --memory")
			}
			if changedCPU && cpus < 1 {
				return fmt.Errorf("--cpu must be at least 1")
			}
			if changedMemory && memory < 64 {
				return fmt.Errorf("--memory must be at least 64 MiB")
			}
			body := map[string]any{}
			if changedCPU {
				body["vcpus"] = cpus
			}
			if changedMemory {
				body["memory_mb"] = memory
			}
			var sb types.Sandbox
			if err := do(http.MethodPatch,
				"/v1/sandboxes/"+url.PathEscape(args[0])+"/resources", body, &sb); err != nil {
				return err
			}
			fmt.Printf("%s\t%s\t%d vCPU\t%d MB\n", sb.ID, sb.State, sb.VCPUs, sb.MemoryMB)
			return nil
		},
	}
	cmd.Flags().IntVar(&cpus, "cpu", 0, "desired live vCPU count (create-time --max-cpu caps it)")
	cmd.Flags().IntVar(&memory, "memory", 0, "desired live memory in MiB (create-time --max-memory caps it)")
	return cmd
}
