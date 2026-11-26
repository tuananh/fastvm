package main

import (
	"fmt"
	"os"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"fastvm/internal/types"
)

type imagePullResult struct {
	disk types.BaseDisk
	err  error
}

type imagePullModel struct {
	ref     string
	spinner spinner.Model
	start   time.Time
	result  <-chan imagePullResult
	done    bool
	disk    types.BaseDisk
	err     error
}

func newImagePullModel(ref string, result <-chan imagePullResult) imagePullModel {
	return imagePullModel{
		ref:     ref,
		spinner: spinner.New(spinner.WithSpinner(spinner.Dot)),
		start:   time.Now(),
		result:  result,
	}
}

func (m imagePullModel) Init() tea.Cmd {
	return tea.Batch(
		func() tea.Msg { return m.spinner.Tick() },
		waitForImagePull(m.result),
	)
}

func waitForImagePull(ch <-chan imagePullResult) tea.Cmd {
	return func() tea.Msg {
		return <-ch
	}
}

func (m imagePullModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case imagePullResult:
		m.done = true
		m.disk = msg.disk
		m.err = msg.err
		return m, tea.Quit
	default:
		return m, nil
	}
}

func (m imagePullModel) View() tea.View {
	if m.done {
		return tea.NewView("")
	}
	return tea.NewView(fmt.Sprintf(
		"%s Pulling image %s\n\n"+
			"  This can take a while on the first pull:\n"+
			"  pull → unpack rootfs → create ext4 disk\n\n"+
			"  elapsed %s",
		m.spinner.View(),
		m.ref,
		time.Since(m.start).Round(time.Second),
	))
}

// pullImageWithProgress runs a synchronous image pull behind a Bubbles
// spinner when the CLI is attached to a terminal. Non-interactive callers get
// a single status line on stderr instead, preserving machine-readable stdout.
func pullImageWithProgress(ref string, pull func() (types.BaseDisk, error)) (types.BaseDisk, error) {
	// Print this before the spinner as well, so the first pull is visible even
	// when terminal progress rendering is unavailable.
	fmt.Fprintf(os.Stderr, "pulling image %s (the first pull can take a while)...\n", ref)
	info, err := os.Stderr.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 || os.Getenv("TERM") == "dumb" {
		return pull()
	}

	results := make(chan imagePullResult, 1)
	go func() {
		disk, pullErr := pull()
		results <- imagePullResult{disk: disk, err: pullErr}
	}()

	model := newImagePullModel(ref, results)
	final, err := tea.NewProgram(model, tea.WithInput(nil), tea.WithOutput(os.Stderr)).Run()
	if err != nil {
		return types.BaseDisk{}, err
	}
	done, ok := final.(imagePullModel)
	if !ok {
		return types.BaseDisk{}, fmt.Errorf("image pull progress: unexpected model %T", final)
	}
	if done.err != nil {
		return types.BaseDisk{}, done.err
	}
	fmt.Fprintf(os.Stderr, "✓ pulled image %s (%s)\n", done.ref, short(done.disk.Digest))
	return done.disk, nil
}
