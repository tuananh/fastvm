import fs from "node:fs";
import http from "node:http";
import https from "node:https";
import net from "node:net";
import tls from "node:tls";
import type { IncomingMessage, Server as NodeHttpServer, ServerResponse } from "node:http";
import type { Duplex } from "node:stream";
import type { HttpServer, Plugin } from "vite";

/** Demo defaults: daemon TCP API from hack/config.yaml and `make certs`. */
const DEFAULT_API_URL = "https://127.0.0.1:8443";
const DEFAULT_CERT_PATH = "/tmp/fastvm/certs/client";

/** Only requests/upgrades under this prefix are proxied; everything else is Vite's. */
const API_PREFIX = "/v1/";

type NextFunction = (err?: unknown) => void;

type ProxyTarget =
  | { kind: "unix"; socketPath: string }
  | {
      kind: "tcp";
      base: URL;
      ca?: Buffer;
      cert?: Buffer;
      key?: Buffer;
    };

type TLSMaterial = { ca?: Buffer; cert?: Buffer; key?: Buffer };

function readTLSMaterial(certPath: string | undefined): TLSMaterial {
  if (!certPath) {
    return {};
  }
  const read = (name: string): Buffer | undefined => {
    const path = `${certPath}/${name}`;
    try {
      return fs.readFileSync(path);
    } catch (err) {
      throw new Error(`cannot read ${path}: ${(err as Error).message}`);
    }
  };
  return {
    ca: read("ca.pem"),
    cert: read("cert.pem"),
    key: read("key.pem"),
  };
}

/**
 * Resolve the server-side upstream. FASTVM_HOST/FASTVM_CERT_PATH mirror the
 * CLI, so the console can talk to the same mTLS API. FASTVM_SOCKET remains a
 * fallback for local unix-socket development.
 */
function resolveTarget(): ProxyTarget {
  // FASTVM_SOCKET explicitly opts into the unix socket. Otherwise prefer the
  // mTLS API so the console works for an unprivileged user without socket
  // permissions.
  if (process.env.FASTVM_SOCKET) {
    return { kind: "unix", socketPath: process.env.FASTVM_SOCKET };
  }
  const host = (process.env.FASTVM_API_URL || process.env.FASTVM_HOST || DEFAULT_API_URL).trim();
  const base = new URL(host.includes("://") ? host : `https://${host}`);
  const certPath =
    process.env.FASTVM_CERT_PATH || (base.protocol === "https:" ? DEFAULT_CERT_PATH : undefined);
  return {
    kind: "tcp",
    base,
    ...readTLSMaterial(certPath),
  };
}

function targetLabel(target: ProxyTarget): string {
  return target.kind === "unix" ? target.socketPath : target.base.toString();
}

function sendUnavailable(res: ServerResponse, target: ProxyTarget, err: Error): void {
  if (res.writableEnded || res.destroyed) {
    return;
  }
  if (res.headersSent) {
    res.destroy(err);
    return;
  }
  const body = JSON.stringify({
    error: `fastvmd unavailable at ${targetLabel(target)}: ${err.message}`,
  });
  res.statusCode = 502;
  res.setHeader("Content-Type", "application/json");
  res.setHeader("Content-Length", Buffer.byteLength(body));
  res.end(body);
}

/** Serialize the original upgrade request line and headers for replay upstream. */
function serializeRequestHead(req: IncomingMessage, host: string): string {
  const lines = [`${req.method ?? "GET"} ${req.url ?? "/"} HTTP/1.1`];
  for (let i = 0; i < req.rawHeaders.length; i += 2) {
    const name = req.rawHeaders[i];
    const value = req.rawHeaders[i + 1];
    lines.push(`${name}: ${name.toLowerCase() === "host" ? host : value}`);
  }
  return `${lines.join("\r\n")}\r\n\r\n`;
}

function tlsServerName(hostname: string): string | undefined {
  return net.isIP(hostname) ? undefined : hostname;
}

function upstreamRequestOptions(target: ProxyTarget, req: IncomingMessage): https.RequestOptions {
  const headers = { ...req.headers };
  if (target.kind === "unix") {
    return {
      socketPath: target.socketPath,
      path: req.url ?? "/",
      method: req.method ?? "GET",
      headers: { ...headers, host: "localhost" },
    };
  }
  const basePath = target.base.pathname.replace(/\/$/, "");
  const path = `${basePath}${req.url ?? "/"}`;
  return {
    protocol: target.base.protocol,
    hostname: target.base.hostname,
    port: target.base.port || (target.base.protocol === "https:" ? 443 : 80),
    path,
    method: req.method ?? "GET",
    headers: { ...headers, host: target.base.host },
    ca: target.ca,
    cert: target.cert,
    key: target.key,
    servername: tlsServerName(target.base.hostname),
  };
}

function createUpstreamSocket(target: ProxyTarget): Duplex {
  if (target.kind === "unix") {
    return net.connect({ path: target.socketPath });
  }
  const options = {
    host: target.base.hostname,
    port: Number(target.base.port || (target.base.protocol === "https:" ? 443 : 80)),
    ca: target.ca,
    cert: target.cert,
    key: target.key,
    servername: tlsServerName(target.base.hostname),
  };
  return target.base.protocol === "https:"
    ? tls.connect(options)
    : net.connect({ host: options.host, port: options.port });
}

function upstreamHost(target: ProxyTarget): string {
  return target.kind === "unix" ? "localhost" : target.base.host;
}

/**
 * Tunnel WebSocket upgrades for `/v1/*` to fastvmd.
 *
 * Vite may also attach upgrade listeners (HMR); we deliberately ignore
 * non-`/v1/` paths so the dev client keeps working. Non-HTTP leaves (HMR) are
 * left untouched.
 */
function attachUpgradeProxy(httpServer: HttpServer | null | undefined, target: ProxyTarget): void {
  if (httpServer === null || httpServer === undefined) {
    return;
  }

  (httpServer as NodeHttpServer).on(
    "upgrade",
    (req: IncomingMessage, socket: Duplex, head: Buffer): void => {
      const url = req.url ?? "/";
      if (!url.startsWith(API_PREFIX) || socket.destroyed) {
        return;
      }

      const upstream = createUpstreamSocket(target);
      let settled = false;

      const teardown = (err?: Error): void => {
        if (settled) {
          return;
        }
        settled = true;
        upstream.destroy();
        if (!socket.destroyed) {
          socket.destroy(err);
        }
      };

      const connectedEvent =
        target.kind === "tcp" && target.base.protocol === "https:" ? "secureConnect" : "connect";
      upstream.once(connectedEvent, () => {
        upstream.write(serializeRequestHead(req, upstreamHost(target)));
        if (head.length > 0) {
          upstream.write(head);
        }
        socket.pipe(upstream);
        upstream.pipe(socket);
      });
      upstream.once("error", (err: NodeJS.ErrnoException) => {
        teardown(err);
      });
      upstream.once("close", () => {
        teardown();
      });
      socket.once("error", () => {
        teardown();
      });
      socket.once("close", () => {
        teardown();
      });
    },
  );
}

/**
 * Vite plugin that forwards same-origin `/v1/*` HTTP requests and WebSocket
 * upgrades to fastvmd. It is installed in both the dev server and `vp preview`,
 * so the browser always talks to the API same-origin (no CORS, no browser-side
 * mTLS).
 *
 * Upstream selection:
 *   FASTVM_HOST / FASTVM_API_URL + FASTVM_CERT_PATH -> mTLS TCP API
 *   FASTVM_SOCKET                                 -> unix socket fallback
 */
export function fastvmProxy(): Plugin {
  const target = resolveTarget();

  const middleware = (req: IncomingMessage, res: ServerResponse, next: NextFunction): void => {
    const url = req.url ?? "/";
    if (!url.startsWith(API_PREFIX)) {
      next();
      return;
    }

    const client = target.kind === "tcp" && target.base.protocol === "https:" ? https : http;
    const options = upstreamRequestOptions(target, req);
    const upstream = client.request(options, (upstreamRes) => {
      res.writeHead(upstreamRes.statusCode ?? 502, upstreamRes.headers);
      upstreamRes.on("error", (err) => {
        res.destroy(err);
      });
      upstreamRes.pipe(res);
    });

    upstream.on("error", (err: NodeJS.ErrnoException) => {
      sendUnavailable(res, target, err);
    });
    // If the browser goes away mid-request, stop waiting on the daemon.
    res.on("close", () => {
      upstream.destroy();
    });
    req.pipe(upstream);
  };

  return {
    name: "fastvm:api-proxy",
    configureServer(server) {
      server.middlewares.use(middleware);
      attachUpgradeProxy(server.httpServer, target);
    },
    configurePreviewServer(server) {
      server.middlewares.use(middleware);
      attachUpgradeProxy(server.httpServer, target);
    },
  };
}
