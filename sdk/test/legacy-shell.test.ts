import { describe, expect, it } from "vite-plus/test";
import http from "node:http";
import type { AddressInfo } from "node:net";
import { FastVM } from "../src/client.js";
import type { ShellEvent } from "../src/types.js";

function frame(type: string, payload: Buffer): Buffer {
  const out = Buffer.alloc(5 + payload.byteLength);
  out.write(type, 0, "latin1");
  out.writeUInt32BE(payload.byteLength, 1);
  payload.copy(out, 5);
  return out;
}

describe("legacy shell upgrade", () => {
  it("speaks the framed shell protocol", async () => {
    const server = http.createServer();
    let upgraded = false;
    server.on("upgrade", (request, socket) => {
      upgraded = true;
      expect(request.method).toBe("POST");
      expect(request.url).toBe("/v1/sandboxes/sandbox%201/shell");
      socket.write(
        "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: fastvm-shell\r\n\r\n",
      );
      socket.write(frame("o", Buffer.from("hello")));
      socket.on("data", (data) => {
        let offset = 0;
        while (offset + 5 <= data.byteLength) {
          const type = String.fromCharCode(data[offset] ?? 0);
          const length = data.readUInt32BE(offset + 1);
          const payload = data.subarray(offset + 5, offset + 5 + length);
          offset += 5 + length;
          if (type === "i" && payload.toString() === "exit") {
            socket.write(frame("e", Buffer.from([0, 0, 0, 3])));
          }
        }
      });
    });
    await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
    expect(upgraded).toBe(false);
    const address = server.address() as AddressInfo;
    const vm = await FastVM.connect({ host: `http://127.0.0.1:${address.port}` });
    try {
      const shell = await vm.legacyShell("sandbox 1", { argv: ["/bin/sh"], rows: 30, cols: 100 });
      const events: ShellEvent[] = [];
      for await (const event of shell.events()) {
        events.push(event);
        if (event.type === "output") {
          shell.write("exit");
          shell.resize(40, 120);
        }
      }
      expect(
        new TextDecoder().decode(events[0]?.type === "output" ? events[0].data : undefined),
      ).toBe("hello");
      expect(events.at(-1)).toEqual({ type: "exit", code: 3 });
    } finally {
      await vm.close();
      server.closeAllConnections();
      server.close();
    }
  });
});
