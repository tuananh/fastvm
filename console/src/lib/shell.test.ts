import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import {
  buildShellUrl,
  buildShellUrlFromLocation,
  connectShell,
  shellResizeMessageSchema,
  shellServerMessageSchema,
} from "./shell";
import type { ShellCallbacks } from "./shell";

class FakeWebSocket {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSING = 2;
  static readonly CLOSED = 3;
  static instances: FakeWebSocket[] = [];

  readonly url: string;
  binaryType = "blob";
  readyState = FakeWebSocket.CONNECTING;
  sent: (string | Uint8Array)[] = [];
  closeCalls = 0;

  onopen: ((event: Event) => void) | null = null;
  onmessage: ((event: MessageEvent) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  onclose: ((event: CloseEvent) => void) | null = null;

  constructor(url: string | URL) {
    this.url = typeof url === "string" ? url : url.toString();
    FakeWebSocket.instances.push(this);
  }

  send(data: string | Uint8Array): void {
    this.sent.push(data);
  }

  close(): void {
    this.closeCalls += 1;
    this.readyState = FakeWebSocket.CLOSED;
  }

  open(): void {
    this.readyState = FakeWebSocket.OPEN;
    this.onopen?.(new Event("open"));
  }

  message(data: unknown): void {
    this.onmessage?.({ data } as unknown as MessageEvent);
  }

  serverClose(code: number): void {
    this.readyState = FakeWebSocket.CLOSED;
    this.onclose?.({ code } as unknown as CloseEvent);
  }
}

function lastSocket(): FakeWebSocket {
  const socket = FakeWebSocket.instances.at(-1);
  if (socket === undefined) {
    throw new Error("no WebSocket was created");
  }
  return socket;
}

function textDecoder(): TextDecoder {
  return new TextDecoder();
}

beforeEach(() => {
  FakeWebSocket.instances = [];
  vi.stubGlobal("WebSocket", FakeWebSocket);
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("shell URL builders", () => {
  it("builds a same-origin ws URL with encoded ref and repeatable argv", () => {
    const url = buildShellUrl("my vm", {
      cols: 120,
      rows: 40,
      argv: ["/bin/bash", "-l"],
    });
    expect(url).toBe(
      `ws://${window.location.host}/v1/sandboxes/my%20vm/shell/ws?cols=120&rows=40&argv=%2Fbin%2Fbash&argv=-l`,
    );
  });

  it("omits argv when not provided and truncates fractional sizes", () => {
    const url = buildShellUrl("web", { cols: 80.9, rows: 24.2 });
    expect(url).toBe(`ws://${window.location.host}/v1/sandboxes/web/shell/ws?cols=80&rows=24`);
  });

  it("uses wss for https origins", () => {
    const url = buildShellUrlFromLocation(
      { protocol: "https:", host: "console.example.com" },
      "sb 1",
      { cols: 100, rows: 30 },
    );
    expect(url).toBe("wss://console.example.com/v1/sandboxes/sb%201/shell/ws?cols=100&rows=30");
  });
});

describe("shell control schemas", () => {
  it("parses exit and error server messages", () => {
    expect(shellServerMessageSchema.parse({ type: "exit", code: 0 })).toEqual({
      type: "exit",
      code: 0,
    });
    expect(shellServerMessageSchema.parse({ type: "error", message: "pty failed" })).toEqual({
      type: "error",
      message: "pty failed",
    });
    expect(shellServerMessageSchema.safeParse({ type: "exit" }).success).toBe(false);
    expect(shellServerMessageSchema.safeParse({ type: "nope" }).success).toBe(false);
  });

  it("requires positive integer resize dimensions", () => {
    expect(shellResizeMessageSchema.safeParse({ type: "resize", cols: 80, rows: 24 }).success).toBe(
      true,
    );
    expect(shellResizeMessageSchema.safeParse({ type: "resize", cols: 0, rows: 24 }).success).toBe(
      false,
    );
    expect(
      shellResizeMessageSchema.safeParse({ type: "resize", cols: 80, rows: 2.5 }).success,
    ).toBe(false);
  });
});

describe("connectShell", () => {
  function connect(callbacks: ShellCallbacks = {}) {
    const handle = connectShell("web", { cols: 80, rows: 24 }, callbacks);
    const socket = lastSocket();
    return { handle, socket };
  }

  it("opens the URL with binaryType arraybuffer and reports open", () => {
    const opened: string[] = [];
    const { handle, socket } = connect({ onOpen: () => opened.push("open") });
    expect(socket.url).toBe(
      `ws://${window.location.host}/v1/sandboxes/web/shell/ws?cols=80&rows=24`,
    );
    expect(socket.binaryType).toBe("arraybuffer");
    expect(handle.state).toBe("connecting");

    socket.open();
    expect(handle.state).toBe("open");
    expect(opened).toEqual(["open"]);
  });

  it("encodes sendInput as a UTF-8 binary frame", () => {
    const { handle, socket } = connect();
    socket.open();
    handle.sendInput("echo héllo\r");

    expect(socket.sent).toHaveLength(1);
    const frame = socket.sent[0];
    expect(frame).toBeInstanceOf(Uint8Array);
    expect(textDecoder().decode(frame as Uint8Array)).toBe("echo héllo\r");
  });

  it("sends resize as a JSON text frame and ignores invalid sizes", () => {
    const { handle, socket } = connect();
    socket.open();
    handle.resize(100, 30);
    handle.resize(0, 30);
    handle.resize(100, -1);

    expect(socket.sent).toEqual([JSON.stringify({ type: "resize", cols: 100, rows: 30 })]);
  });

  it("passes binary output through as Uint8Array", () => {
    const chunks: Uint8Array[] = [];
    const { socket } = connect({ onOutput: (data) => chunks.push(data) });
    socket.open();

    socket.message(new Uint8Array([104, 105]).buffer);
    socket.message(new Uint8Array([33]));

    expect(chunks).toHaveLength(2);
    expect(chunks[0]).toEqual(new Uint8Array([104, 105]));
    expect(chunks[1]).toEqual(new Uint8Array([33]));
  });

  it("reports exit JSON and ignores the following close", () => {
    const exits: (number | null)[] = [];
    const errors: string[] = [];
    const { handle, socket } = connect({
      onExit: (code) => exits.push(code),
      onError: (message) => errors.push(message),
    });
    socket.open();

    socket.message(JSON.stringify({ type: "exit", code: 3 }));
    expect(exits).toEqual([3]);
    expect(handle.state).toBe("closed");

    socket.serverClose(1000);
    expect(exits).toEqual([3]);
    expect(errors).toEqual([]);
  });

  it("reports error JSON and ignores the following close", () => {
    const exits: (number | null)[] = [];
    const errors: string[] = [];
    const { handle } = connect({
      onExit: (code) => exits.push(code),
      onError: (message) => errors.push(message),
    });

    lastSocket().message(JSON.stringify({ type: "error", message: "pty failed" }));
    expect(errors).toEqual(["pty failed"]);
    expect(exits).toEqual([]);
    expect(handle.state).toBe("closed");
  });

  it("treats a clean close without exit as ended with a null code", () => {
    const exits: (number | null)[] = [];
    const errors: string[] = [];
    const { handle } = connect({
      onExit: (code) => exits.push(code),
      onError: (message) => errors.push(message),
    });
    lastSocket().open();
    lastSocket().serverClose(1000);

    expect(exits).toEqual([null]);
    expect(errors).toEqual([]);
    expect(handle.state).toBe("closed");
  });

  it("treats an abnormal close without exit as an error", () => {
    const exits: (number | null)[] = [];
    const errors: string[] = [];
    connect({
      onExit: (code) => exits.push(code),
      onError: (message) => errors.push(message),
    });
    lastSocket().open();
    lastSocket().serverClose(1006);

    expect(exits).toEqual([]);
    expect(errors).toEqual([
      "Could not open the shell session (WebSocket code 1006). Make sure fastvmd is running a current build and that the console proxy can reach the daemon API.",
    ]);
  });

  it("ignores unknown or malformed text frames", () => {
    const exits: (number | null)[] = [];
    const errors: string[] = [];
    const outputs: Uint8Array[] = [];
    connect({
      onExit: (code) => exits.push(code),
      onError: (message) => errors.push(message),
      onOutput: (data) => outputs.push(data),
    });
    lastSocket().open();
    lastSocket().message("not json");
    lastSocket().message(JSON.stringify({ type: "unknown" }));

    expect(exits).toEqual([]);
    expect(errors).toEqual([]);
    expect(outputs).toEqual([]);
  });

  it("sends nothing before the socket is open", () => {
    const { handle, socket } = connect();
    handle.sendInput("ignored");
    handle.resize(80, 24);
    expect(socket.sent).toEqual([]);
  });

  it("closes the socket and suppresses callbacks for client-initiated close", () => {
    const exits: (number | null)[] = [];
    const errors: string[] = [];
    const { handle, socket } = connect({
      onExit: (code) => exits.push(code),
      onError: (message) => errors.push(message),
    });
    socket.open();
    handle.close();

    expect(socket.closeCalls).toBe(1);
    expect(handle.state).toBe("closed");
    socket.serverClose(1000);
    expect(exits).toEqual([]);
    expect(errors).toEqual([]);
    handle.close();
    expect(socket.closeCalls).toBe(1);
    handle.sendInput("ignored");
    expect(socket.sent).toEqual([]);
  });
});
