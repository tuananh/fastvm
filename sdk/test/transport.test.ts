import { afterEach, describe, expect, it } from "vite-plus/test";
import { readFileSync } from "node:fs";
import http from "node:http";
import https from "node:https";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { mkdtemp, rm } from "node:fs/promises";
import { FastVM } from "../src/client.js";
import { json } from "./helpers.js";

const fixture = path.resolve("test/fixtures/tls");

describe("transport", () => {
  let server: http.Server | https.Server | undefined;
  let vm: FastVM | undefined;
  let cleanup: (() => Promise<void>) | undefined;

  afterEach(async () => {
    await vm?.close();
    vm = undefined;
    if (server !== undefined) {
      await new Promise<void>((resolve) => server?.close(() => resolve()));
      server = undefined;
    }
    await cleanup?.();
    cleanup = undefined;
  });

  it("uses a Unix domain socket", async () => {
    const dir = await mkdtemp(path.join(os.tmpdir(), "fastvm-sdk-"));
    const socketPath = path.join(dir, "api.sock");
    const unixServer = http.createServer((_req, res) =>
      json(res, { version: "1.0.0", name: "fastvm" }),
    );
    server = unixServer;
    await new Promise<void>((resolve) => unixServer.listen(socketPath, resolve));
    cleanup = async () => {
      await rm(dir, { recursive: true, force: true });
    };
    vm = await FastVM.connect({ socketPath });
    await expect(vm.version()).resolves.toEqual({ version: "1.0.0", name: "fastvm" });
    expect((await import("node:fs")).statSync(socketPath).isSocket()).toBe(true);
  });

  it("uses mTLS with a client certificate", async () => {
    const tlsServer = https.createServer(
      {
        key: readFileSync(path.join(fixture, "server.key")),
        cert: readFileSync(path.join(fixture, "server.pem")),
        ca: readFileSync(path.join(fixture, "ca.pem")),
        requestCert: true,
        rejectUnauthorized: true,
      },
      (_req, res) => json(res, { version: "2.0.0", name: "fastvm" }),
    );
    server = tlsServer;
    await new Promise<void>((resolve) => tlsServer.listen(0, "127.0.0.1", resolve));
    const address = tlsServer.address() as net.AddressInfo;
    vm = await FastVM.connect({ host: `https://127.0.0.1:${address.port}`, certPath: fixture });
    await expect(vm.version()).resolves.toEqual({ version: "2.0.0", name: "fastvm" });
  });

  it("supports HTTP hosts without TLS", async () => {
    const plain = http.createServer((_req, res) => json(res, { version: "3.0.0", name: "fastvm" }));
    server = plain;
    await new Promise<void>((resolve) => plain.listen(0, "127.0.0.1", resolve));
    const address = plain.address() as net.AddressInfo;
    vm = await FastVM.connect({ host: `http://127.0.0.1:${address.port}` });
    await expect(vm.version()).resolves.toEqual({ version: "3.0.0", name: "fastvm" });
  });
});
