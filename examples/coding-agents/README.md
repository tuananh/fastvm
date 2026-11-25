# Coding-agent sandboxes

Disposable coding-agent environments built from `node:26-slim`. Each example
installs its CLI on first boot and keeps the sandbox awake.

The agent installs and Codex session data need more than the demo's old 1 GiB
image. Keep `storage.root_size_gb: 8` (or pull the image with `--size-gb 8`)
and re-pull the image after changing the size.

```sh
# OpenAI Codex CLI with the DeepSeek API
export DEEPSEEK_API_KEY=...
fastvmctl run -f examples/coding-agents/codex.fvmfile -it

# Anthropic Claude Code
export ANTHROPIC_API_KEY=...
fastvmctl run -f examples/coding-agents/claude.fvmfile -it

# GitHub Copilot CLI
export GITHUB_TOKEN=...
fastvmctl run -f examples/coding-agents/copilot.fvmfile -it
```

The Codex image installs `procps`, `git`, and `ripgrep` because the Node slim
image does not include the tools Codex's app-server expects.

The Codex example mounts [`codex-home/config.toml`](codex-home/config.toml),
trusts `/workspace` by default, and selects DeepSeek's OpenAI-compatible
endpoint:

```toml
[projects."/workspace"]
trust_level = "trusted"

model = "deepseek-flash"
model_provider = "deepseek"
preferred_auth_method = "apikey"
forced_login_method = "api"

[model_providers.deepseek]
name = "deepseek"
base_url = "https://api.deepseek.com/"
env_key = "DEEPSEEK_API_KEY"
wire_api = "responses"
```

Newer Codex versions may also expect DeepSeek's model catalog in
`~/.codex/models.json`; the DeepSeek setup script generates it.

Mount a project directory with `-v` or add `volumes` to the fvmfile:

```sh
fastvmctl run -f examples/coding-agents/codex.fvmfile \
  -v "$PWD:/workspace" -w /workspace -it
```

The `allow_hosts` entries cover CLI installation and API access. Add hosts if
your agent or tools require more.
