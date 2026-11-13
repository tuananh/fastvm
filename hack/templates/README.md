# Coding-agent template images

One Dockerfile per baked-in template. `internal/template` references the
prebuilt fastvm image tags (`fastvm/<name>:v6`); it never builds Dockerfiles at
`template start` time.

Build them against a running daemon (Docker + network required):

```sh
make build
sudo make template:copilot     # one image
sudo make template:claude      # or template:codex
sudo make template-images      # all three
```

The individual targets and `make template-images` run
`fastvm image build -f hack/templates/<name>.Dockerfile -t fastvm/<name>:v6 hack/templates`,
and tags the resulting base disks. A CI pipeline can call the same commands (or
`make template-images` with `FASTVM_SOCKET=...`).

Keep the tag version here in sync with `internal/template/template.go` and the
`TEMPLATE_VERSION` variable in the root Makefile.
