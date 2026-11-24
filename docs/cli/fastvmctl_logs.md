# FASTVMCTL-LOGS(1)

## NAME

fastvmctl-logs - show captured sandbox logs

## SYNOPSIS

```text
fastvmctl logs [--source LIST] [-f|--follow] <sandbox>
```

## DESCRIPTION

Prints logs captured for a sandbox. Sources are printed separately with a
`==> source <==` header when more than one source is selected.

| Source | Contents |
|---|---|
| `stdout` | Non-interactive session stdout |
| `stderr` | Non-interactive session stderr |
| `console` | Raw serial console: boot, kernel, init, agent diagnostics |
| `system` | Daemon lifecycle events |

The default sources are `stdout,stderr`.

PTY sessions are intentionally not persisted. A PTY merges stdout and stderr and
echoes terminal input, so logging it would mix user keystrokes into the log. Use
the live shell for terminal output.

The sandbox is woken if it is paused or dormant.

## OPTIONS

| Option | Description |
|---|---|
| `--source LIST` | Comma-separated sources: `stdout`, `stderr`, `console`, `system` |
| `-f`, `--follow` | Stream new log output |

Global options: `--socket`, `--help`.

## EXAMPLES

```sh
fastvmctl logs dev
fastvmctl logs --source console dev
fastvmctl logs --source system dev
fastvmctl logs --source stdout,stderr -f dev
```

## SEE ALSO

[fastvmctl](fastvmctl.md), [fastvmctl run](fastvmctl_run.md),
[fastvmctl exec](fastvmctl_exec.md), [fastvmctl shell](fastvmctl_shell.md)
