import { readFileSync } from "node:fs";
import net from "node:net";
import tls from "node:tls";
import { Agent, fetch as undiciFetch } from "undici";
import type { Dispatcher } from "undici";
import {
  ApiError,
  ConflictError,
  ConnectionError,
  InvalidError,
  NotFoundError,
  TimeoutError,
} from "./errors.js";

export interface FastVMOptions {
  /** e.g. https://127.0.0.1:8443 (FASTVM_HOST) */
  host?: string;
  /** Unix socket path (FASTVM_SOCKET) */
  socketPath?: string;
  /** Directory containing ca.pem, cert.pem and key.pem (FASTVM_CERT_PATH) */
  certPath?: string;
  ca?: string | Buffer;
  cert?: string | Buffer;
  key?: string | Buffer;
  timeoutMs?: number;
  dispatcher?: Dispatcher;
}

interface TLSMaterial {
  ca?: string | Buffer;
  cert?: string | Buffer;
  key?: string | Buffer;
}

interface ErrorEnvelope {
  error?: string;
  kind?: string;
}

export class Transport {
  readonly baseUrl: string;
  readonly socketPath?: string;
  private readonly dispatcher: Dispatcher | undefined;
  private readonly timeoutMs: number;
  private readonly tls: TLSMaterial;

  constructor(options: FastVMOptions = {}) {
    const socketPath = options.socketPath ?? process.env.FASTVM_SOCKET;
    const host = options.host ?? process.env.FASTVM_HOST;
    this.timeoutMs = options.timeoutMs ?? 30_000;

    if (socketPath !== undefined && socketPath !== "") {
      this.socketPath = socketPath;
      this.baseUrl = "http://localhost";
      this.tls = {};
      this.dispatcher = options.dispatcher ?? new Agent({ connect: { socketPath } });
      return;
    }

    const raw = host === undefined || host === "" ? "https://127.0.0.1:8443" : host;
    const url = new URL(raw.includes("://") ? raw : `https://${raw}`);
    this.baseUrl = `${url.protocol}//${url.host}${url.pathname.replace(/\/$/, "")}`;
    this.tls =
      url.protocol !== "https:" || options.certPath === undefined
        ? {
            ...(options.ca === undefined ? {} : { ca: options.ca }),
            ...(options.cert === undefined ? {} : { cert: options.cert }),
            ...(options.key === undefined ? {} : { key: options.key }),
          }
        : {
            ...readTLSMaterial(options.certPath),
            ...(options.ca === undefined ? {} : { ca: options.ca }),
            ...(options.cert === undefined ? {} : { cert: options.cert }),
            ...(options.key === undefined ? {} : { key: options.key }),
          };
    this.dispatcher = options.dispatcher ?? buildAgent(url, options);
  }

  async request<T>(
    method: string,
    path: string,
    body?: unknown,
    options: { signal?: AbortSignal; timeoutMs?: number; raw?: "text" | "json" } = {},
  ): Promise<T> {
    const signals: AbortSignal[] = [];
    if (options.signal !== undefined) signals.push(options.signal);
    const timeout = options.timeoutMs ?? this.timeoutMs;
    if (timeout > 0) signals.push(AbortSignal.timeout(timeout));
    const signal =
      signals.length === 0
        ? undefined
        : signals.length === 1
          ? signals[0]
          : AbortSignal.any(signals);

    const headers: Record<string, string> = { Accept: "application/json" };
    let payload: string | undefined;
    if (body !== undefined) {
      headers["Content-Type"] = "application/json";
      payload = JSON.stringify(body);
    }

    let response: Awaited<ReturnType<typeof undiciFetch>>;
    try {
      response = await undiciFetch(`${this.baseUrl}${path}`, {
        method,
        headers,
        ...(payload === undefined ? {} : { body: payload }),
        ...(signal === undefined ? {} : { signal }),
        ...(this.dispatcher === undefined ? {} : { dispatcher: this.dispatcher }),
      });
    } catch (error) {
      if (
        error instanceof Error &&
        (error.name === "TimeoutError" || error.name === "AbortError")
      ) {
        throw new TimeoutError(`request timed out after ${timeout}ms`, { cause: error });
      }
      throw new ConnectionError(
        `cannot reach fastvmd at ${this.baseUrl}: ${(error as Error).message}`,
        { cause: error },
      );
    }

    if (response.status === 204) {
      return undefined as T;
    }
    const text = await response.text();
    if (!response.ok) {
      throw apiError(response.status, text);
    }
    if (options.raw === "text") {
      return text as T;
    }
    if (text === "") {
      return undefined as T;
    }
    try {
      return JSON.parse(text) as T;
    } catch {
      throw new ApiError(response.status, "fastvmd returned invalid JSON", undefined, text);
    }
  }

  async binary(
    method: string,
    path: string,
    body: Uint8Array | undefined,
    options: { signal?: AbortSignal; timeoutMs?: number } = {},
  ): Promise<{ data: Uint8Array; headers: { get(name: string): string | null } }> {
    const signal = options.signal ?? AbortSignal.timeout(options.timeoutMs ?? this.timeoutMs);
    const headers: Record<string, string> = { Accept: "application/octet-stream" };
    if (body !== undefined) headers["Content-Type"] = "application/octet-stream";
    let response: Awaited<ReturnType<typeof undiciFetch>>;
    try {
      response = await undiciFetch(`${this.baseUrl}${path}`, {
        method,
        headers,
        ...(body === undefined ? {} : { body }),
        signal,
        ...(this.dispatcher === undefined ? {} : { dispatcher: this.dispatcher }),
      });
    } catch (error) {
      if (
        error instanceof Error &&
        (error.name === "TimeoutError" || error.name === "AbortError")
      ) {
        throw new TimeoutError("request timed out", { cause: error });
      }
      throw new ConnectionError(
        `cannot reach fastvmd at ${this.baseUrl}: ${(error as Error).message}`,
        { cause: error },
      );
    }
    if (!response.ok) throw apiError(response.status, await response.text());
    return { data: new Uint8Array(await response.arrayBuffer()), headers: response.headers };
  }

  async text(
    path: string,
    options: { signal?: AbortSignal; offset?: number; timeoutMs?: number } = {},
  ): Promise<{ text: string; size?: number }> {
    const headers: Record<string, string> = { Accept: "text/plain" };
    const url = new URL(`${this.baseUrl}${path}`);
    if (options.offset !== undefined && options.offset > 0)
      url.searchParams.set("offset", String(options.offset));
    const signal = options.signal ?? AbortSignal.timeout(options.timeoutMs ?? this.timeoutMs);
    let response: Awaited<ReturnType<typeof undiciFetch>>;
    try {
      response = await undiciFetch(url, {
        headers,
        signal,
        ...(this.dispatcher === undefined ? {} : { dispatcher: this.dispatcher }),
      });
    } catch (error) {
      if (
        error instanceof Error &&
        (error.name === "TimeoutError" || error.name === "AbortError")
      ) {
        throw new TimeoutError("request timed out", { cause: error });
      }
      throw new ConnectionError(
        `cannot reach fastvmd at ${this.baseUrl}: ${(error as Error).message}`,
        { cause: error },
      );
    }
    const body = await response.text();
    if (!response.ok) throw apiError(response.status, body);
    const sizeHeader = response.headers.get("x-fastvm-size");
    return {
      text: body,
      ...(sizeHeader === null ? {} : { size: Number(sizeHeader) }),
    };
  }

  /** Open the legacy raw `Upgrade: fastvm-shell` endpoint. */
  upgradeShell(path: string, body: unknown): Promise<{ socket: net.Socket; head: Buffer }> {
    const url = new URL(`${this.baseUrl}${path}`);
    const socket =
      this.socketPath !== undefined
        ? net.connect(this.socketPath)
        : url.protocol === "https:"
          ? tls.connect({
              host: url.hostname,
              port: Number(url.port === "" ? 443 : url.port),
              ...this.tls,
            })
          : net.connect(Number(url.port === "" ? 80 : url.port), url.hostname);

    const payload = Buffer.from(JSON.stringify(body));
    const head = [
      `POST ${url.pathname} HTTP/1.1`,
      `Host: ${url.host}`,
      "Content-Type: application/json",
      `Content-Length: ${payload.byteLength}`,
      "Connection: Upgrade",
      "Upgrade: fastvm-shell",
      "",
      "",
    ].join("\r\n");

    return new Promise((resolve, reject) => {
      let buffer = Buffer.alloc(0);
      let upgraded = false;
      const onError = (error: Error) => {
        socket.destroy();
        reject(error);
      };
      const onData = (data: Buffer) => {
        buffer = Buffer.concat([buffer, data]);
        const end = buffer.indexOf("\r\n\r\n");
        if (end < 0) return;
        if (!upgraded) {
          upgraded = true;
          const header = buffer.subarray(0, end).toString("latin1");
          buffer = buffer.subarray(end + 4);
          socket.pause();
          socket.off("data", onData);
          socket.off("error", onError);
          if (!header.startsWith("HTTP/1.1 101")) {
            socket.destroy();
            reject(new Error(header.split("\r\n")[0] ?? "shell upgrade failed"));
            return;
          }
          resolve({ socket, head: buffer });
          return;
        }
      };
      socket.once("connect", () => {
        socket.write(head);
        socket.write(payload);
        socket.on("data", onData);
      });
      socket.once("error", onError);
    });
  }

  /** Browser-style WebSocket URL and Node `ws` connection options. */
  shellURL(path: string): string {
    return `${this.baseUrl.replace(/^http/, "ws")}${path}`;
  }

  shellConnectOptions(): {
    createConnection?: () => net.Socket;
    ca?: string | Buffer;
    cert?: string | Buffer;
    key?: string | Buffer;
    rejectUnauthorized?: boolean;
  } {
    if (this.socketPath !== undefined) {
      return { createConnection: () => net.connect(this.socketPath as string) };
    }
    return {
      ...(this.tls.ca === undefined ? {} : { ca: this.tls.ca }),
      ...(this.tls.cert === undefined ? {} : { cert: this.tls.cert }),
      ...(this.tls.key === undefined ? {} : { key: this.tls.key }),
    };
  }

  async close(): Promise<void> {
    if (this.dispatcher !== undefined && "close" in this.dispatcher) {
      await this.dispatcher.close();
    }
  }
}

function buildAgent(url: URL, options: FastVMOptions): Dispatcher | undefined {
  if (url.protocol !== "https:") return undefined;
  const material = options.certPath === undefined ? {} : readTLSMaterial(options.certPath);
  const ca = options.ca ?? material.ca;
  const cert = options.cert ?? material.cert;
  const key = options.key ?? material.key;
  if (ca === undefined && cert === undefined && key === undefined) return undefined;
  return new Agent({
    connect: {
      ...(ca === undefined ? {} : { ca }),
      ...(cert === undefined ? {} : { cert }),
      ...(key === undefined ? {} : { key }),
    },
  });
}

function readTLSMaterial(dir: string): TLSMaterial {
  return {
    ca: readFileSync(`${dir}/ca.pem`),
    cert: readFileSync(`${dir}/cert.pem`),
    key: readFileSync(`${dir}/key.pem`),
  };
}

function apiError(status: number, text: string): ApiError {
  let message = text.trim();
  let kind: string | undefined;
  try {
    const envelope = JSON.parse(text) as ErrorEnvelope;
    if (typeof envelope.error === "string") message = envelope.error;
    if (typeof envelope.kind === "string") kind = envelope.kind;
  } catch {
    // keep raw body
  }
  switch (status) {
    case 404:
      return new NotFoundError(status, message, kind, text);
    case 409:
      return new ConflictError(status, message, kind, text);
    case 400:
      return new InvalidError(status, message, kind, text);
    default:
      return new ApiError(status, message, kind, text);
  }
}
