package main

import (
	"fmt"
	"os"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"fastvm/internal/types"
)

type kernelDownloadResult struct {
	result types.KernelDownloadResult
	err    error
}

type kernelDownloadModel struct {
	release string
	spinner spinner.Model
	start   time.Time
	result  <-chan kernelDownloadResult
	done    bool
	out     types.KernelDownloadResult
	err     error
}

func newKernelDownloadModel(release string, result <-chan kernelDownloadResult) kernelDownloadModel {
	return kernelDownloadModel{
		release: release,
		spinner: spinner.New(spinner.WithSpinner(spinner.Dot)),
		start:   time.Now(),
		result:  result,
	}
}

func (m kernelDownloadModel) Init() tea.Cmd {
	return tea.Batch(
		func() tea.Msg { return m.spinner.Tick() },
		waitForKernelDownload(m.result),
	)
}

func waitForKernelDownload(ch <-chan kernelDownloadResult) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

func (m kernelDownloadModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case kernelDownloadResult:
		m.done = true
		m.out = msg.result
		m.err = msg.err
		return m, tea.Quit
	default:
		return m, nil
	}
}

func (m kernelDownloadModel) View() tea.View {
	if m.done {
		return tea.NewView("")
	}
	return tea.NewView(fmt.Sprintf(
		"%s Downloading kernel %s\n\n"+
			"  Fetching from cloud-hypervisor/linux and verifying SHA-256…\n\n"+
			"  elapsed %s",
		m.spinner.View(),
		m.release,
		time.Since(m.start).Round(time.Second),
	))
}

// kernelDownloadWithProgress runs the daemon download behind a Bubbles spinner
// when stderr is a terminal. Non-interactive callers get a status line instead,
// preserving the machine-readable stdout result.
func kernelDownloadWithProgress(
	release string,
	download func() (types.KernelDownloadResult, error),
) (types.KernelDownloadResult, error) {
	fmt.Fprintf(os.Stderr, "downloading kernel %s (verified by SHA-256)...\n", release)
	info, err := os.Stderr.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 || os.Getenv("TERM") == "dumb" {
		return download()
	}

	results := make(chan kernelDownloadResult, 1)
	go func() {
		result, downloadErr := download()
		results <- kernelDownloadResult{result: result, err: downloadErr}
	}()

	final, err := tea.NewProgram(newKernelDownloadModel(release, results),
		tea.WithInput(nil), tea.WithOutput(os.Stderr)).Run()
	if err != nil {
		return types.KernelDownloadResult{}, err
	}
	done, ok := final.(kernelDownloadModel)
	if !ok {
		return types.KernelDownloadResult{}, fmt.Errorf("kernel download progress: unexpected model %T", final)
	}
	if done.err != nil {
		return types.KernelDownloadResult{}, done.err
	}
	fmt.Fprintf(os.Stderr, "✓ downloaded kernel %s\n", done.out.Release)
	return done.out, nil
}
