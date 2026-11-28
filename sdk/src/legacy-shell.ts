import type { Socket } from "node:net";
import type { ShellEvent, ShellHandle, ShellOptions } from "./types.js";
import type { Transport } from "./transport.js";

type Waiter = (result: IteratorResult<ShellEvent>) => void;

const FRAME_INPUT = 0x69; // i
const FRAME_RESIZE = 0x72; // r
const FRAME_OUTPUT = 0x6f; // o
const FRAME_EXIT = 0x65; // e
const FRAME_ERROR = 0x78; // x
const MAX_FRAME = 256 * 1024;

function frame(type: number, payload: Uint8Array): Buffer {
  const out = Buffer.allocUnsafe(5 + payload.byteLength);
  out[0] = type;
  out.writeUInt32BE(payload.byteLength, 1);
  Buffer.from(payload).copy(out, 5);
  return out;
}

/** Interactive PTY over the legacy `POST /shell` upgrade endpoint. */
export class LegacyShellSession implements ShellHandle {
  private readonly socket: Socket;
  private buffer: Buffer;
  private readonly queue: ShellEvent[] = [];
  private readonly waiters: Waiter[] = [];
  private done = false;

  private constructor(socket: Socket, head: Buffer) {
    this.socket = socket;
    this.buffer = head;
    socket.on("data", (data) => {
      this.buffer = Buffer.concat([
        this.buffer,
        typeof data === "string" ? Buffer.from(data) : data,
      ]);
      this.parse();
    });
    socket.on("error", (error) => {
      this.push({ type: "error", message: error.message });
      this.finish();
    });
    socket.on("close", () => this.finish());
    this.parse();
    socket.resume();
  }

  static async open(
    transport: Transport,
    ref: string,
    options: ShellOptions = {},
  ): Promise<LegacyShellSession> {
    const { socket, head } = await transport.upgradeShell(
      `/v1/sandboxes/${encodeURIComponent(ref)}/shell`,
      {
        ...(options.argv === undefined ? {} : { argv: options.argv }),
        rows: options.rows ?? 24,
        cols: options.cols ?? 80,
      },
    );
    return new LegacyShellSession(socket, head);
  }

  write(data: string | Uint8Array): void {
    const bytes = typeof data === "string" ? new TextEncoder().encode(data) : data;
    this.socket.write(frame(FRAME_INPUT, bytes));
  }

  resize(rows: number, cols: number): void {
    this.socket.write(
      frame(FRAME_RESIZE, new TextEncoder().encode(JSON.stringify({ rows, cols }))),
    );
  }

  close(): void {
    this.socket.destroy();
    this.finish();
  }

  events(): AsyncIterable<ShellEvent> {
    return {
      [Symbol.asyncIterator]: () => ({
        next: () => this.next(),
      }),
    };
  }

  private parse(): void {
    while (this.buffer.byteLength >= 5) {
      const type = this.buffer[0];
      const length = this.buffer.readUInt32BE(1);
      if (length > MAX_FRAME) {
        this.push({ type: "error", message: "shell frame too large" });
        this.close();
        return;
      }
      if (this.buffer.byteLength < 5 + length) return;
      const payload = this.buffer.subarray(5, 5 + length);
      this.buffer = this.buffer.subarray(5 + length);
      if (type === FRAME_OUTPUT) {
        this.push({ type: "output", data: new Uint8Array(payload) });
      } else if (type === FRAME_EXIT) {
        const code = payload.byteLength >= 4 ? payload.readUInt32BE(0) : 0;
        this.push({ type: "exit", code });
        this.finish();
      } else if (type === FRAME_ERROR) {
        this.push({ type: "error", message: payload.toString("utf8") });
        this.finish();
      }
    }
  }

  private next(): Promise<IteratorResult<ShellEvent>> {
    const item = this.queue.shift();
    if (item !== undefined) return Promise.resolve({ value: item, done: false });
    if (this.done) return Promise.resolve({ value: undefined, done: true });
    return new Promise((resolve) => this.waiters.push(resolve));
  }

  private push(event: ShellEvent): void {
    const waiter = this.waiters.shift();
    if (waiter !== undefined) waiter({ value: event, done: false });
    else this.queue.push(event);
  }

  private finish(): void {
    if (this.done) return;
    this.done = true;
    this.socket.destroy();
    for (const waiter of this.waiters.splice(0)) waiter({ value: undefined, done: true });
  }
}
