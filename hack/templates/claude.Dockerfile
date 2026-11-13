# Anthropic Claude Code template.
# Referenced by internal/template as fastvm/claude:v6.
# Build with: sudo make template:<name> (or template-images)
FROM node:26-bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && npm install -g @anthropic-ai/claude-code
WORKDIR /workspace
