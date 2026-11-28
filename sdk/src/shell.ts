import WebSocket from "ws";
import type { ShellEvent, ShellHandle, ShellOptions } from "./types.js";
import type { Transport } from "./transport.js";

type Waiter = (result: IteratorResult<ShellEvent>) => void;

/** Interactive PTY session over the daemon's shell WebSocket endpoint. */
export class ShellSession implements ShellHandle {
  private readonly socket: WebSocket;
  private readonly opened: Promise<void>;
  private readonly queue: ShellEvent[] = [];
  private readonly waiters: Waiter[] = [];
  private done = false;

  constructor(socket: WebSocket) {
    this.socket = socket;
    this.opened = new Promise<void>((resolve, reject) => {
      const onOpen = () => {
        socket.off("error", onError);
        resolve();
      };
      const onError = (error: Error) => {
        socket.off("open", onOpen);
        reject(error);
      };
      socket.once("open", onOpen);
      socket.once("error", onError);
    });
    socket.on("message", (data, isBinary) => {
      if (isBinary) {
        this.push({ type: "output", data: new Uint8Array(data as Buffer) });
        return;
      }
      try {
        const text =
          typeof data === "string"
            ? data
            : Array.isArray(data)
              ? Buffer.concat(data).toString()
              : Buffer.from(data as ArrayBuffer).toString();
        const message = JSON.parse(text) as {
          type?: string;
          code?: number;
          message?: string;
        };
        if (message.type === "exit") {
          this.push({ type: "exit", code: message.code ?? 0 });
          this.finish();
        } else if (message.type === "error") {
          this.push({ type: "error", message: message.message ?? "shell failed" });
          this.finish();
        }
      } catch {
        // Ignore malformed control frames.
      }
    });
    socket.on("error", (error) => {
      this.push({ type: "error", message: error.message });
      this.finish();
    });
    socket.on("close", () => this.finish());
  }

  static async open(
    transport: Transport,
    ref: string,
    options: ShellOptions = {},
  ): Promise<ShellSession> {
    const params = new URLSearchParams();
    params.set("rows", String(options.rows ?? 24));
    params.set("cols", String(options.cols ?? 80));
    for (const arg of options.argv ?? ["/bin/sh"]) params.append("argv", arg);
    const url = `${transport.shellURL(`/v1/sandboxes/${encodeURIComponent(ref)}/shell/ws`)}?${params.toString()}`;
    const socket = new WebSocket(url, transport.shellConnectOptions());
    const session = new ShellSession(socket);
    await session.opened;
    return session;
  }

  write(data: string | Uint8Array): void {
    if (this.socket.readyState === WebSocket.OPEN) {
      this.socket.send(typeof data === "string" ? new TextEncoder().encode(data) : data);
    }
  }

  resize(rows: number, cols: number): void {
    if (this.socket.readyState === WebSocket.OPEN) {
      this.socket.send(JSON.stringify({ type: "resize", rows, cols }));
    }
  }

  close(): void {
    this.socket.close();
    this.finish();
  }

  events(): AsyncIterable<ShellEvent> {
    return {
      [Symbol.asyncIterator]: () => ({
        next: () => this.next(),
      }),
    };
  }

  private next(): Promise<IteratorResult<ShellEvent>> {
    const item = this.queue.shift();
    if (item !== undefined) return Promise.resolve({ value: item, done: false });
    if (this.done) return Promise.resolve({ value: undefined, done: true });
    return new Promise((resolve) => this.waiters.push(resolve));
  }

  private push(event: ShellEvent): void {
    const waiter = this.waiters.shift();
    if (waiter !== undefined) {
      waiter({ value: event, done: false });
    } else {
      this.queue.push(event);
    }
  }

  private finish(): void {
    if (this.done) return;
    this.done = true;
    for (const waiter of this.waiters.splice(0)) {
      waiter({ value: undefined, done: true });
    }
  }
}
