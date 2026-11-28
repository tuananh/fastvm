import { afterEach, describe, expect, it } from "vite-plus/test";
import http from "node:http";
import type { AddressInfo } from "node:net";
import { WebSocketServer } from "ws";
import type WebSocket from "ws";
import { FastVM } from "../src/client.js";
import type { ShellEvent } from "../src/types.js";

function rawText(data: Buffer | ArrayBuffer | Buffer[]): string {
  if (Array.isArray(data)) return Buffer.concat(data).toString();
  return Buffer.from(data as ArrayBuffer).toString();
}

describe("shell websocket", () => {
  let server: http.Server | undefined;
  let vm: FastVM | undefined;
  let received: { binary: boolean; data: string }[] = [];
  let socket: WebSocket | undefined;

  afterEach(async () => {
    await vm?.close();
    socket?.terminate();
    if (server !== undefined) await new Promise<void>((resolve) => server?.close(() => resolve()));
    server = undefined;
  });

  it("opens a PTY session, sends input, resize and receives output/exit", async () => {
    received = [];
    const httpServer = http.createServer();
    server = httpServer;
    const wss = new WebSocketServer({ noServer: true });
    httpServer.on("upgrade", (request, rawSocket, head) => {
      wss.handleUpgrade(request, rawSocket, head, (websocket) => {
        socket = websocket;
        const url = new URL(request.url ?? "/", "http://localhost");
        expect(decodeURIComponent(url.pathname)).toBe("/v1/sandboxes/sandbox 1/shell/ws");
        expect(url.searchParams.get("rows")).toBe("30");
        expect(url.searchParams.get("cols")).toBe("100");
        expect(url.searchParams.getAll("argv")).toEqual(["/bin/sh", "-l"]);
        websocket.on("message", (data, isBinary) => {
          received.push({ binary: isBinary, data: rawText(data) });
          if (isBinary && rawText(data) === "exit") {
            websocket.send(JSON.stringify({ type: "exit", code: 7 }));
          }
        });
        websocket.send(Buffer.from("hello"));
      });
    });
    await new Promise<void>((resolve) => httpServer.listen(0, "127.0.0.1", resolve));
    const address = httpServer.address() as AddressInfo;
    vm = await FastVM.connect({ host: `http://127.0.0.1:${address.port}` });
    const shell = await vm.shell("sandbox 1", { argv: ["/bin/sh", "-l"], rows: 30, cols: 100 });
    const events: ShellEvent[] = [];
    for await (const event of shell.events()) {
      events.push(event);
      if (event.type === "output") {
        shell.write("ls\n");
        shell.resize(40, 120);
        shell.write("exit");
      }
    }
    expect(
      new TextDecoder().decode(events[0]?.type === "output" ? events[0].data : undefined),
    ).toBe("hello");
    expect(events.at(-1)).toEqual({ type: "exit", code: 7 });
    expect(received.some((message) => !message.binary && message.data.includes('"resize"'))).toBe(
      true,
    );
    expect(received.some((message) => message.binary && message.data === "ls\n")).toBe(true);
  });
});
