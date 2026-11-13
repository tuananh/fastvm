// Package template defines the baked-in fastvm templates for running coding
// agents inside a microVM.
//
// A template is a base-disk recipe plus a few defaults. The CLI builds the
// image on first use, then boots a sandbox with the agent already installed.
// Templates deliberately keep the VM alive with `sleep infinity`; commands are
// run later through `fastvm exec` (there is no interactive TTY yet).
package template

// Template is one baked-in coding-agent environment.
//
// Templates only reference a prebuilt fastvm image tag. The Dockerfiles live in
// hack/templates/ and are built by `make template-images` (or a CI pipeline);
// the daemon does not build them implicitly on `template start`.
type Template struct {
	Name        string
	Description string
	Agent       string // CLI name inside the guest
	AgentCmd    string // example command to show the user
	Image       string // fastvm image tag reference (fastvm/<name>:<version>)

	// EnvKeys are host environment variables forwarded into the workload when
	// present (API keys/tokens for the agent).
	EnvKeys []string

	// Default resources.
	VCPUs    int
	MemoryMB int
}

// ImageTag is the fastvm image tag referenced by this template.
func (t Template) ImageTag() string { return t.Image }

var builtins = []Template{
	{
		Name:        "claude",
		Description: "Anthropic Claude Code (Node 26 + @anthropic-ai/claude-code)",
		Agent:       "claude",
		AgentCmd:    "claude",
		Image:       "fastvm/claude:v6",
		EnvKeys:     []string{"ANTHROPIC_API_KEY"},
		VCPUs:       2,
		MemoryMB:    2048,
	},
	{
		Name:        "codex",
		Description: "OpenAI Codex CLI (Node 26 + @openai/codex)",
		Agent:       "codex",
		AgentCmd:    "codex",
		Image:       "fastvm/codex:v6",
		EnvKeys:     []string{"OPENAI_API_KEY"},
		VCPUs:       2,
		MemoryMB:    2048,
	},
	{
		Name:        "copilot",
		Description: "GitHub Copilot CLI (Node 26 + @github/copilot)",
		Agent:       "copilot",
		AgentCmd:    "copilot",
		Image:       "fastvm/copilot:v6",
		EnvKeys:     []string{"GH_TOKEN", "GITHUB_TOKEN"},
		VCPUs:       2,
		MemoryMB:    2048,
	},
}

// List returns a copy of the baked-in templates.
func List() []Template {
	out := make([]Template, len(builtins))
	copy(out, builtins)
	return out
}

// Get returns the named template.
func Get(name string) (Template, bool) {
	for _, t := range builtins {
		if t.Name == name {
			return t, true
		}
	}
	return Template{}, false
}
