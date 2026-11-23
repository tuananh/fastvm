import { z } from "zod";

/**
 * Control messages sent by fastvmd over the shell WebSocket (text frames).
 * All other server→client frames are binary PTY output.
 */
export const shellServerMessageSchema = z.discriminatedUnion("type", [
  z.object({ type: z.literal("exit"), code: z.number().int() }),
  z.object({ type: z.literal("error"), message: z.string() }),
]);
export type ShellServerMessage = z.infer<typeof shellServerMessageSchema>;

/** Resize control message sent by the client (text frames). */
export const shellResizeMessageSchema = z.object({
  type: z.literal("resize"),
  cols: z.number().int().positive(),
  rows: z.number().int().positive(),
});
export type ShellResizeMessage = z.infer<typeof shellResizeMessageSchema>;

/** Terminal size and optional argv for a shell session. */
export interface ShellOptions {
  cols: number;
  rows: number;
  /** Command to run; fastvmd defaults to `/bin/sh` when omitted. */
  argv?: string[];
}

/** Callbacks invoked by {@link connectShell}. */
export interface ShellCallbacks {
  /** The WebSocket handshake completed. */
  onOpen?: () => void;
  /** Raw PTY output bytes (server→client binary frames). */
  onOutput?: (data: Uint8Array) => void;
  /** The process exited. `code` is `null` when the daemon closed without an exit frame. */
  onExit?: (code: number | null) => void;
  /** Protocol error frame or unexpected close. */
  onError?: (message: string) => void;
}

export type ShellState = "connecting" | "open" | "closed";

/** Handle returned by {@link connectShell}. */
export interface ShellHandle {
  /** Current connection state; updated in place. */
  readonly state: ShellState;
  /** Send raw terminal input (encoded as a UTF-8 binary frame). */
  sendInput(data: string): void;
  /** Send a resize control frame; invalid sizes are ignored. */
  resize(cols: number, rows: number): void;
  /** Close the socket; no exit/error callback fires for a client-initiated close. */
  close(): void;
}

/** The minimal `window.location` shape needed to build a shell URL. */
export interface ShellLocation {
  protocol: string;
  host: string;
}

const SHELL_PATH_PREFIX = "/v1/sandboxes";

/**
 * Build the shell WebSocket URL from an explicit location (test seam for
 * `buildShellUrl`).
 */
export function buildShellUrlFromLocation(
  location: ShellLocation,
  ref: string,
  options: ShellOptions,
): string {
  const protocol = location.protocol === "https:" ? "wss:" : "ws:";
  const params = new URLSearchParams();
  params.set("cols", String(Math.trunc(options.cols)));
  params.set("rows", String(Math.trunc(options.rows)));
  for (const arg of options.argv ?? []) {
    params.append("argv", arg);
  }
  return `${protocol}//${location.host}${SHELL_PATH_PREFIX}/${encodeURIComponent(ref)}/shell/ws?${params.toString()}`;
}

/**
 * Build the same-origin shell WebSocket URL for a sandbox:
 * `ws(s)://<console-origin>/v1/sandboxes/<ref>/shell/ws?cols&rows&argv...`.
 */
export function buildShellUrl(ref: string, options: ShellOptions): string {
  return buildShellUrlFromLocation(window.location, ref, options);
}

function parseJsonText(text: string): unknown {
  try {
    return JSON.parse(text) as unknown;
  } catch {
    return undefined;
  }
}

/**
 * Open a shell session with fastvmd and wire up the binary/text protocol:
 *
 * - client→server binary frames: raw PTY input (`sendInput`)
 * - client→server text frames: `{"type":"resize","cols":N,"rows":N}` (`resize`)
 * - server→client binary frames: raw PTY output (`onOutput`)
 * - server→client text frames: `{"type":"exit","code":N}` (`onExit`) or
 *   `{"type":"error","message":...}` (`onError`)
 *
 * A close without an exit/error frame is reported as `onExit(null)` for a clean
 * close (code 1000) and `onError(...)` otherwise.
 */
export function connectShell(
  ref: string,
  options: ShellOptions,
  callbacks: ShellCallbacks = {},
): ShellHandle {
  const socket = new WebSocket(buildShellUrl(ref, options));
  socket.binaryType = "arraybuffer";

  let state: ShellState = "connecting";
  let settled = false;
  let clientClosed = false;

  socket.onopen = () => {
    state = "open";
    callbacks.onOpen?.();
  };

  socket.onmessage = (event: MessageEvent) => {
    if (typeof event.data === "string") {
      const message = shellServerMessageSchema.safeParse(parseJsonText(event.data));
      if (!message.success || settled) {
        return;
      }
      settled = true;
      state = "closed";
      if (message.data.type === "exit") {
        callbacks.onExit?.(message.data.code);
      } else {
        callbacks.onError?.(message.data.message);
      }
      return;
    }
    if (event.data instanceof ArrayBuffer) {
      callbacks.onOutput?.(new Uint8Array(event.data));
      return;
    }
    if (event.data instanceof Uint8Array) {
      callbacks.onOutput?.(new Uint8Array(event.data));
    }
  };

  // Browsers fire `error` before `close`; the close handler reports the failure.
  socket.onerror = () => {};

  socket.onclose = (event: CloseEvent) => {
    state = "closed";
    if (clientClosed || settled) {
      return;
    }
    settled = true;
    if (event.code === 1000) {
      callbacks.onExit?.(null);
    } else if (event.code === 1006) {
      // 1006 means the WebSocket handshake never completed (no close frame), e.g.
      // fastvmd is down, on an older build without the exec endpoint, or the
      // proxy cannot reach its socket. Give the user an actionable hint.
      callbacks.onError?.(
        "Could not open the shell session (WebSocket code 1006). Make sure fastvmd is running a current build and that the console proxy can reach the daemon API.",
      );
    } else {
      callbacks.onError?.(`shell connection closed unexpectedly (code ${event.code})`);
    }
  };

  const send = (payload: string | Uint8Array<ArrayBuffer>): void => {
    if (socket.readyState !== WebSocket.OPEN) {
      return;
    }
    socket.send(payload);
  };

  return {
    get state(): ShellState {
      return state;
    },
    sendInput(data: string): void {
      // Copy into an ArrayBuffer-backed view; TextEncoder may return a view over
      // a SharedArrayBuffer-like buffer, which WebSocket.send does not accept.
      send(new Uint8Array(new TextEncoder().encode(data)));
    },
    resize(cols: number, rows: number): void {
      const message = shellResizeMessageSchema.safeParse({ type: "resize", cols, rows });
      if (!message.success) {
        return;
      }
      send(JSON.stringify(message.data));
    },
    close(): void {
      if (clientClosed) {
        return;
      }
      clientClosed = true;
      state = "closed";
      socket.close();
    },
  };
}
