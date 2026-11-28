# @tuananh/fastvm

TypeScript SDK for the fastvm micro-VM runtime.

- Node.js 22.19+
- Full coverage of the daemon HTTP API
- Unix socket, plain HTTP and mTLS transports
- Typed `Sandbox`/`ExecResult` objects

## Install

```sh
pnpm add @tuananh/fastvm
```

The daemon must already be running (`fastvmd`).

## Connect

```ts
import { FastVM } from "@tuananh/fastvm";

const vm = await FastVM.connect({
  host: "https://127.0.0.1:8443",
  certPath: "/tmp/fastvm/certs/client",
});
```

Environment defaults match the CLI:

```text
FASTVM_HOST
FASTVM_CERT_PATH
FASTVM_SOCKET
```

```ts
const vm = await FastVM.connect(); // reads env
const local = await FastVM.connect({ socketPath: "/run/fastvm/fastvm.sock" });
```

## One-shot run

```ts
const result = await vm.run({
  image: "python:3.15.2-alpine",
  command: ["python", "-c", "import sys; print(sys.version.split()[0])"],
  cpu: 1,
  memoryMb: 256,
  timeoutMs: 120_000,
});

result.assertSuccess();
console.log(result.stdout);
```

`run()` creates the sandbox, executes the command and deletes the sandbox in
`finally`.

## Sandbox lifecycle

```ts
const sandbox = await vm.sandboxes.create({
  image: "python:3.15.2-alpine",
  name: "demo",
  cpu: 2,
  maxCpu: 4,
  memoryMb: 512,
  network: true,
  ports: [{ hostPort: 8080, guestPort: 80, proto: "tcp" }],
});

try {
  const result = await sandbox.exec(["python", "-c", "print('hello')"]);
  result.assertSuccess();
  console.log(result.stdout);

  console.log(await sandbox.stats());
  await sandbox.resize({ cpu: 3, memoryMb: 1024 });
  await sandbox.pause();
  await sandbox.resume();
} finally {
  await sandbox.delete();
}
```

## Files

```ts
await sandbox.writeFile("/tmp/hello.txt", "hello\n", { mode: 0o644 });
const file = await sandbox.readFile("/tmp/hello.txt");
console.log(new TextDecoder().decode(file.data));
```

## Console and shell

```ts
console.log(await sandbox.console({ offset: 0 }));

const shell = await sandbox.shell({ argv: ["/bin/sh"], rows: 30, cols: 100 });
shell.write("ls\n");
shell.resize(40, 120);
for await (const event of shell.events()) {
  if (event.type === "output") process.stdout.write(event.data);
  if (event.type === "exit") break;
}
```

`legacyShell()` is also available for the older HTTP upgrade endpoint.

## Images

```ts
await vm.images.list();
await vm.images.pull("alpine");
await vm.images.ingest({ rootfsDir: "/tmp/rootfs", tag: "demo:latest" });
await vm.images.remove("alpine", { force: true });
```

## Kernels

```ts
await vm.kernels.list();
await vm.kernels.remoteList();
await vm.kernels.download({ release: "ch-release-v7.2.8-20261009" });
await vm.kernels.use("ch-release-v7.2.8-20261009");
await vm.kernels.remove("ch-release-v7.2.8-20260901");
```

## Snapshots and events

```ts
await sandbox.snapshot("before-upgrade");
await sandbox.snapshotCopy("before-upgrade");
await vm.listSnapshots();
await vm.removeSnapshot("before-upgrade");
await vm.events(50);
```

## Development

```sh
vp install
vp check          # format + lint + typecheck
vp test           # bundled Vitest
vp pack --dts     # Vite+ library build with Oxc declarations
```

The SDK uses Vite+ end to end: `vp check` for format/lint/typecheck, `vp test`
for tests, and `vp pack` for the npm library. `isolatedDeclarations` lets Oxc
emit declarations without the TypeScript compiler or esbuild.

Tests cover every API method against a local mock daemon, plus Unix socket,
mTLS, HTTP, WebSocket shell and legacy framed-shell transports.
