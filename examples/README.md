# fastvm examples

Runnable `.fvmfile` examples for common workloads. See the
[FVMFILE format](../docs/FVMFILE.md) for all supported fields.

| Example | Use case |
|---|---|
| [hello.fvmfile](hello.fvmfile) | Minimal image + command |
| [nodejs.fvmfile](nodejs.fvmfile) | Node.js REPL |
| [docker.fvmfile](docker.fvmfile) | Docker-in-VM with host networking |
| [coding-agents](coding-agents/) | Codex with DeepSeek API, Claude Code, GitHub Copilot CLI |
| [chrome-headless](chrome-headless/) | Headless Chromium with CDP |
| [github-actions-runner](github-actions-runner/) | Self-hosted GitHub Actions runner |

Each example is a starting point. Pin image tags for reproducible use, and do
not commit API keys or runner tokens.
