import { afterEach, describe, expect, it } from "vite-plus/test";
import { FastVM } from "../src/client.js";
import {
  ApiError,
  ConflictError,
  ConnectionError,
  InvalidError,
  NotFoundError,
  CommandFailedError,
} from "../src/errors.js";
import { DISK, KERNEL, SANDBOX, STATS, empty, json, mockDaemon } from "./helpers.js";
import type { MockDaemon } from "./helpers.js";

let client: FastVM | undefined;
let daemon: MockDaemon | undefined;

async function setup(): Promise<{ vm: FastVM; daemon: MockDaemon }> {
  daemon = await mockDaemon();
  client = await FastVM.connect({ host: daemon.url, timeoutMs: 5000 });
  return { vm: client, daemon };
}

afterEach(async () => {
  await client?.close();
  await daemon?.close();
  client = undefined;
  daemon = undefined;
});

describe("client basics", () => {
  it("reads version, metrics and events", async () => {
    const { vm, daemon } = await setup();
    daemon.handle("GET", "/v1/version", (_req, res) =>
      json(res, { version: "1.2.3", name: "fastvm" }),
    );
    daemon.handle("GET", "/metrics", (_req, res) => {
      res.statusCode = 200;
      res.setHeader("Content-Type", "text/plain");
      res.end("fastvm_up 1\n");
    });
    daemon.handle("GET", "/v1/events", (_req, res) =>
      json(res, [
        {
          time: "2026-01-01T00:00:00Z",
          sandbox_id: "sandbox-1",
          sandbox_name: "web",
          type: "state",
          message: "Running",
        },
      ]),
    );

    await expect(vm.version()).resolves.toEqual({ version: "1.2.3", name: "fastvm" });
    await expect(vm.metrics()).resolves.toBe("fastvm_up 1\n");
    await expect(vm.events(7)).resolves.toEqual([
      {
        time: "2026-01-01T00:00:00Z",
        sandboxId: "sandbox-1",
        sandboxName: "web",
        type: "state",
        message: "Running",
      },
    ]);
    expect(daemon.requests.find((r) => r.path === "/v1/events")?.query.get("limit")).toBe("7");
  });

  it("maps API errors to typed exceptions", async () => {
    const { vm, daemon } = await setup();
    daemon.handle("GET", "/v1/sandboxes/missing", (_req, res) =>
      json(res, { error: "not found", kind: "not_found" }, 404),
    );
    daemon.handle("GET", "/v1/sandboxes/conflict", (_req, res) =>
      json(res, { error: "conflict", kind: "conflict" }, 409),
    );
    daemon.handle("GET", "/v1/sandboxes/bad", (_req, res) =>
      json(res, { error: "bad", kind: "invalid" }, 400),
    );
    await expect(vm.sandboxes.get("missing")).rejects.toBeInstanceOf(NotFoundError);
    await expect(vm.sandboxes.get("conflict")).rejects.toBeInstanceOf(ConflictError);
    await expect(vm.sandboxes.get("bad")).rejects.toBeInstanceOf(InvalidError);
  });

  it("rejects malformed JSON responses", async () => {
    const { vm, daemon } = await setup();
    daemon.handle("GET", "/v1/version", (_req, res) => {
      res.statusCode = 200;
      res.setHeader("Content-Type", "application/json");
      res.end("not-json");
    });
    await expect(vm.version()).rejects.toMatchObject({ status: 200 });
  });

  it("maps server failures and connection failures", async () => {
    const { vm, daemon } = await setup();
    daemon.handle("GET", "/v1/version", (_req, res) => json(res, { error: "boom" }, 500));
    await expect(vm.version()).rejects.toBeInstanceOf(ApiError);

    const dead = await FastVM.connect({ host: "http://127.0.0.1:1", timeoutMs: 200 });
    await expect(dead.version()).rejects.toBeInstanceOf(ConnectionError);
    await dead.close();
  });
});

describe("sandboxes", () => {
  it("lists, creates, gets and maps camelCase fields", async () => {
    const { vm, daemon } = await setup();
    daemon.handle("GET", "/v1/sandboxes", (_req, res) => json(res, [SANDBOX]));
    daemon.handle("POST", "/v1/sandboxes", (_req, res) => json(res, SANDBOX, 201));
    daemon.handle("GET", "/v1/sandboxes/sandbox-1", (_req, res) => json(res, SANDBOX));

    const list = await vm.sandboxes.list();
    expect(list[0]?.id).toBe("sandbox-1");
    expect(list[0]?.baseDisk).toBe("sha256:abc");
    expect(list[0]?.ports?.[0]).toEqual({
      hostPort: 8080,
      guestPort: 80,
      proto: "tcp",
      loopbackOnly: true,
    });
    expect(list[0]?.volumes?.[0]?.readOnly).toBe(true);
    expect(list[0]?.dataVolumes?.[0]?.sizeGb).toBe(2);

    const created = await vm.sandboxes.create({
      image: "alpine",
      name: "web",
      cpu: 2,
      maxCpu: 4,
      memoryMb: 512,
      rootSizeGb: 8,
      network: true,
      ports: [{ hostPort: 8080, guestPort: 80, proto: "tcp", loopbackOnly: true }],
      volumes: [{ hostPath: "/host", guestPath: "/guest", readOnly: true }],
      dataVolumes: [{ sizeGb: 2, mount: "/data" }],
    });
    expect(created.name).toBe("web");
    const body = JSON.parse(
      daemon.requests.find((r) => r.method === "POST")?.body.toString() ?? "{}",
    );
    expect(body).toMatchObject({
      image: "alpine",
      vcpus: 2,
      max_vcpus: 4,
      root_size_gb: 8,
      network_enabled: true,
      ports: [{ host_port: 8080, guest_port: 80, proto: "tcp", loopback_only: true }],
      volumes: [{ host_path: "/host", guest_path: "/guest", read_only: true }],
      data_volumes: [{ size_gb: 2, mount: "/data" }],
    });
    await expect(vm.sandboxes.get("sandbox-1")).resolves.toMatchObject({ id: "sandbox-1" });
  });

  it("gets stats for one sandbox and all sandboxes", async () => {
    const { vm, daemon } = await setup();
    daemon.handle("GET", "/v1/sandboxes/sandbox-1/stats", (_req, res) => json(res, STATS));
    daemon.handle("GET", "/v1/sandboxes/stats", (_req, res) => json(res, [STATS]));
    expect(await vm.sandboxes.stats("sandbox-1")).toMatchObject({
      sandboxId: "sandbox-1",
      cpuPercent: 12.5,
    });
    expect(await vm.sandboxes.statsAll()).toHaveLength(1);
  });

  it("runs, resizes and manages lifecycle", async () => {
    const { vm, daemon } = await setup();
    daemon.handle("GET", "/v1/sandboxes/sandbox-1", (_req, res) => json(res, SANDBOX));
    daemon.handle("POST", "/v1/sandboxes/sandbox-1/exec", (_req, res) =>
      json(res, { id: "req", exit_code: 0, stdout: "hello\n", stderr: "" }),
    );
    daemon.handle("PATCH", "/v1/sandboxes/sandbox-1/resources", (_req, res) =>
      json(res, { ...SANDBOX, vcpus: 3 }),
    );
    daemon.handle("POST", "/v1/sandboxes/sandbox-1/sleep", (_req, res) => empty(res));
    daemon.handle("POST", "/v1/sandboxes/sandbox-1/start", (_req, res) => empty(res));
    daemon.handle("POST", "/v1/sandboxes/sandbox-1/wake", (_req, res) => empty(res));
    daemon.handle("DELETE", "/v1/sandboxes/sandbox-1", (_req, res) => empty(res));

    const sandbox = await vm.sandboxes.get("sandbox-1");
    const result = await sandbox.exec(["echo", "hello"], { env: { A: "B" } });
    expect(result.ok).toBe(true);
    expect(result.stdout).toBe("hello\n");
    expect(
      JSON.parse(daemon.requests.find((r) => r.path.endsWith("/exec"))?.body.toString() ?? "{}"),
    ).toEqual({
      argv: ["echo", "hello"],
      env: { A: "B" },
    });
    expect((await sandbox.resize({ cpu: 3, memoryMb: 1024 })).vcpus).toBe(3);
    await sandbox.start();
    await sandbox.sleep({ deep: true });
    await sandbox.wake();
    await sandbox.delete();
    expect(daemon.requests.filter((r) => r.path.endsWith("/sleep"))).toHaveLength(1);
  });

  it("asserts failed command results", async () => {
    const { vm, daemon } = await setup();
    daemon.handle("GET", "/v1/sandboxes/sandbox-1", (_req, res) => json(res, SANDBOX));
    daemon.handle("POST", "/v1/sandboxes/sandbox-1/exec", (_req, res) =>
      json(res, { id: "req", exit_code: 2, stderr: "bad" }),
    );
    const sandbox = await vm.sandboxes.get("sandbox-1");
    const result = await sandbox.run(["false"]);
    expect(result.ok).toBe(false);
    expect(() => result.assertSuccess()).toThrow(CommandFailedError);
  });

  it("snapshots, copies snapshots, lists and removes snapshots", async () => {
    const { vm, daemon } = await setup();
    daemon.handle("GET", "/v1/sandboxes/sandbox-1", (_req, res) => json(res, SANDBOX));
    daemon.handle("POST", "/v1/sandboxes/sandbox-1/snapshot", (_req, res) =>
      json(res, { snapshot: "snap-1" }, 201),
    );
    daemon.handle("POST", "/v1/sandboxes/sandbox-1/snapshot/copy", (_req, res) =>
      json(res, { ...SANDBOX, id: "copy" }, 201),
    );
    daemon.handle("GET", "/v1/snapshots", (_req, res) =>
      json(res, [
        {
          name: "snap-1",
          created_at: "2026-01-01T00:00:00Z",
          image_digest: "sha256:abc",
          vcpus: 2,
          memory_mb: 512,
          network_enabled: true,
        },
      ]),
    );
    daemon.handle("DELETE", "/v1/snapshots/snap-1", (_req, res) => empty(res));
    const sandbox = await vm.sandboxes.get("sandbox-1");
    await expect(sandbox.snapshot("snap-1")).resolves.toBe("snap-1");
    await expect(sandbox.snapshotCopy("snap-1")).resolves.toMatchObject({ id: "copy" });
    expect(await vm.listSnapshots()).toEqual([
      {
        name: "snap-1",
        createdAt: "2026-01-01T00:00:00Z",
        imageDigest: "sha256:abc",
        vcpus: 2,
        memoryMb: 512,
        networkEnabled: true,
      },
    ]);
    await vm.removeSnapshot("snap-1");
  });

  it("reads console output with an offset", async () => {
    const { vm, daemon } = await setup();
    daemon.handle("GET", "/v1/sandboxes/sandbox-1", (_req, res) => json(res, SANDBOX));
    daemon.handle("GET", "/v1/sandboxes/sandbox-1/console", (_req, res) => {
      res.setHeader("Content-Type", "text/plain");
      res.end("line 2\n");
    });
    const sandbox = await vm.sandboxes.get("sandbox-1");
    await expect(sandbox.console({ offset: 7 })).resolves.toBe("line 2\n");
    expect(daemon.requests.find((r) => r.path.endsWith("/console"))?.query.get("offset")).toBe("7");
  });

  it("copies files in both directions", async () => {
    const { vm, daemon } = await setup();
    daemon.handle("GET", "/v1/sandboxes/sandbox-1", (_req, res) => json(res, SANDBOX));
    daemon.handle("PUT", "/v1/sandboxes/sandbox-1/file", (_req, res) => empty(res));
    daemon.handle("GET", "/v1/sandboxes/sandbox-1/file", (_req, res) => {
      res.setHeader("X-Fastvm-Mode", "644");
      res.setHeader("X-Fastvm-Size", "5");
      res.end("hello");
    });
    const sandbox = await vm.sandboxes.get("sandbox-1");
    await sandbox.writeFile("/tmp/x", "hello", { mode: 0o644 });
    expect(daemon.requests.find((r) => r.method === "PUT")?.body.toString()).toBe("hello");
    const file = await sandbox.readFile("/tmp/x", { offset: 1 });
    expect(new TextDecoder().decode(file.data)).toBe("hello");
    expect(file.mode).toBe(0o644);
    expect(file.size).toBe(5);

    daemon.handle("GET", "/v1/sandboxes/sandbox-1/file", (_req, res) => {
      res.setHeader("X-Fastvm-Mode", "644");
      res.setHeader("X-Fastvm-Size", "6");
      res.end("logged");
    });
    await expect(sandbox.logs("stderr")).resolves.toBe("logged");
    expect(daemon.requests.at(-1)?.query.get("path")).toBe("/var/log/fastvm/stderr.log");
  });

  it("refreshes a sandbox record", async () => {
    const { vm, daemon } = await setup();
    let state = "Running";
    daemon.handle("GET", "/v1/sandboxes/sandbox-1", (_req, res) =>
      json(res, { ...SANDBOX, state }),
    );
    const sandbox = await vm.sandboxes.get("sandbox-1");
    expect(sandbox.state).toBe("Running");
    state = "Idle";
    await sandbox.refresh();
    expect(sandbox.state).toBe("Idle");
  });
});

describe("one-shot run", () => {
  it("creates, runs, deletes and cleans up on success", async () => {
    const { vm, daemon } = await setup();
    daemon.handle("POST", "/v1/sandboxes", (_req, res) => json(res, SANDBOX, 201));
    daemon.handle("POST", "/v1/sandboxes/sandbox-1/exec", (_req, res) =>
      json(res, { id: "req", exit_code: 0, stdout: "ok" }),
    );
    daemon.handle("DELETE", "/v1/sandboxes/sandbox-1", (_req, res) => empty(res));
    const result = await vm.run({ image: "alpine", command: ["echo", "ok"] });
    expect(result.stdout).toBe("ok");
    expect(daemon.requests.map((r) => `${r.method} ${r.path}`)).toContain(
      "DELETE /v1/sandboxes/sandbox-1",
    );
  });

  it("deletes the sandbox when the command fails", async () => {
    const { vm, daemon } = await setup();
    daemon.handle("POST", "/v1/sandboxes", (_req, res) => json(res, SANDBOX, 201));
    daemon.handle("POST", "/v1/sandboxes/sandbox-1/exec", (_req, res) =>
      json(res, { id: "req", exit_code: 1 }),
    );
    daemon.handle("DELETE", "/v1/sandboxes/sandbox-1", (_req, res) => empty(res));
    const result = await vm.run({ image: "alpine", command: ["false"] });
    expect(result.ok).toBe(false);
    expect(daemon.requests.some((r) => r.method === "DELETE")).toBe(true);
  });
});

describe("images and kernels", () => {
  it("lists, pulls, ingests and removes images", async () => {
    const { vm, daemon } = await setup();
    daemon.handle("GET", "/v1/images", (_req, res) => json(res, [DISK]));
    daemon.handle("POST", "/v1/images/pull", (_req, res) => json(res, DISK, 201));
    daemon.handle("POST", "/v1/images/ingest", (_req, res) => json(res, DISK, 201));
    daemon.handle("DELETE", "/v1/images", (_req, res) => json(res, DISK));
    expect(await vm.images.list()).toHaveLength(1);
    await expect(vm.images.pull("demo:latest", { sizeGb: 4 })).resolves.toMatchObject({
      digest: "sha256:abc",
    });
    await expect(
      vm.images.ingest({ rootfsDir: "/tmp/rootfs", tag: "demo:latest" }),
    ).resolves.toMatchObject({ digest: "sha256:abc" });
    await vm.images.remove("demo:latest", { force: true });
    expect(
      daemon.requests
        .find((r) => r.method === "DELETE" && r.path === "/v1/images")
        ?.query.get("force"),
    ).toBe("true");
  });

  it("lists, downloads, selects and removes kernels", async () => {
    const { vm, daemon } = await setup();
    daemon.handle("GET", "/v1/kernels", (_req, res) =>
      json(res, { dir: "/kernels", current: "/kernels/release-1", kernels: [KERNEL] }),
    );
    daemon.handle("GET", "/v1/kernels/remote", (_req, res) =>
      json(res, {
        kernels: [{ release: "release-2", asset: "vmlinux-x86_64", size_bytes: 10, sha256: "def" }],
      }),
    );
    daemon.handle("POST", "/v1/kernels/download", (_req, res) =>
      json(res, {
        path: "/kernels/x",
        release: "release-2",
        asset: "vmlinux-x86_64",
        sha256: "def",
      }),
    );
    daemon.handle("POST", "/v1/kernels/use", (_req, res) =>
      json(res, { path: "/kernels/x", current: "/kernels/current" }),
    );
    daemon.handle("DELETE", "/v1/kernels", (_req, res) =>
      json(res, { release: "release-2", path: "/kernels/x" }),
    );
    const list = await vm.kernels.list();
    expect(list.kernels[0]).toMatchObject({ release: "release-1", active: true });
    expect((await vm.kernels.remoteList()).kernels[0]?.asset).toBe("vmlinux-x86_64");
    await expect(vm.kernels.download({ release: "release-2" })).resolves.toMatchObject({
      release: "release-2",
    });
    await expect(vm.kernels.use("release-2")).resolves.toMatchObject({
      current: "/kernels/current",
    });
    await vm.kernels.remove("release-2");
  });
});
