# Sandbox lifecycle

A sandbox moves through a small set of states. It can be awake, paused, or
dormant (memory offloaded to disk).

## States

| State | Meaning |
|---|---|
| `Created` | Provisioned but not booted (`run --no-start`) |
| `Running` | Workload active |
| `Idle` | Running, but no activity; auto-sleep countdown active |
| `Paused` | vCPUs halted; guest RAM resident; network queue stays live |
| `Dormant` | Paused and guest memory snapshotted to a file; VMM stays alive |
| `Failed` | Terminal error; inspect with `fastvmctl get` |

```text
Created -> Running -> Idle -> Paused -> Dormant
                ^        |       |         |
                +--------+-------+---------+   wake
```

## Commands

```sh
fastvmctl run -d --image alpine --name dev    # start
fastvmctl pause dev                           # sleep
fastvmctl pause dev --deep                    # sleep + offload memory
fastvmctl resume dev                          # wake
fastvmctl rm dev                              # destroy
```

- `resume` boots `Created`, wakes `Paused`, or restores `Dormant`.
- Inbound network traffic, `exec`, `shell`, `cp`, and `resume` wake a sleeping
  sandbox.
- Inspection calls (`ps`, `get`, `logs`) read daemon/guest state without waking.
- `--no-auto-sleep` disables automatic pause.
- Automatic sleep is controlled by `lifecycle.idle_window`, `pause_after`, and
  `offload_after`.

## Pause vs dormant vs snapshot

| | Paused | Dormant | Snapshot |
|---|---|---|---|
| Guest RAM | resident | file-backed, demand-paged | copied |
| Wake | fastest | fast | not a wake state |
| Survives VMM restart | no | yes | yes |
| Purpose | short idle | save host memory | save a restorable copy |

A snapshot is an explicit saved copy used by `run --from-snapshot`; the source
sandbox keeps running. See [SNAPSHOTS](SNAPSHOT.md).

## Idle

A sandbox becomes `Idle` when its workload has no qualifying activity for the
configured idle window. If it stays idle long enough it pauses, then goes
dormant. Activity, or an explicit `resume`, returns it to `Running`.

`events` shows state changes:

```sh
fastvmctl events -n 20
```
