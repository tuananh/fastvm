import http from "node:http";
import type { IncomingHttpHeaders, ServerResponse } from "node:http";
import { AddressInfo } from "node:net";

export interface RecordedRequest {
  method: string;
  path: string;
  query: URLSearchParams;
  headers: IncomingHttpHeaders;
  body: Buffer;
}

type Handler = (request: RecordedRequest, response: ServerResponse) => void;

export interface MockDaemon {
  url: string;
  requests: RecordedRequest[];
  close(): Promise<void>;
  handle(method: string, path: string, handler: Handler): void;
}

export async function mockDaemon(): Promise<MockDaemon> {
  const requests: RecordedRequest[] = [];
  const routes = new Map<string, Handler>();
  const server = http.createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (chunk: Buffer) => chunks.push(chunk));
    req.on("end", () => {
      const url = new URL(req.url ?? "/", "http://localhost");
      const request: RecordedRequest = {
        method: req.method ?? "GET",
        path: url.pathname,
        query: url.searchParams,
        headers: req.headers,
        body: Buffer.concat(chunks),
      };
      requests.push(request);
      const route = routes.get(`${request.method} ${request.path}`);
      if (route === undefined) {
        res.statusCode = 404;
        res.setHeader("Content-Type", "application/json");
        res.end(
          JSON.stringify({
            error: `unexpected ${request.method} ${request.path}`,
            kind: "not_found",
          }),
        );
        return;
      }
      route(request, res);
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const address = server.address() as AddressInfo;
  return {
    url: `http://127.0.0.1:${address.port}`,
    requests,
    close: () =>
      new Promise((resolve, reject) =>
        server.close((error) => (error ? reject(error) : resolve())),
      ),
    handle(method, path, handler) {
      routes.set(`${method} ${path}`, handler);
    },
  };
}

export function json(response: ServerResponse, body: unknown, status = 200): void {
  response.statusCode = status;
  response.setHeader("Content-Type", "application/json");
  response.end(JSON.stringify(body));
}

export function empty(response: ServerResponse, status = 204): void {
  response.statusCode = status;
  response.end();
}

export const SANDBOX = {
  id: "sandbox-1",
  name: "web",
  base_disk: "sha256:abc",
  kernel: "kernel-1",
  kernel_path: "/kernels/kernel-1",
  volume_path: "/volumes/web.img",
  root_size_gb: 8,
  vcpus: 2,
  max_vcpus: 4,
  memory_mb: 512,
  max_memory_mb: 2048,
  state: "Running",
  workdir: "/workspace",
  auto_publish: true,
  no_auto_sleep: false,
  network_enabled: true,
  allow_hosts: ["example.com"],
  ports: [{ host_port: 8080, guest_port: 80, proto: "tcp", loopback_only: true }],
  volumes: [{ host_path: "/host", guest_path: "/guest", read_only: true }],
  data_volumes: [{ name: "data", size_gb: 2, mount: "/data", path: "/volumes/data.img" }],
  netns_path: "/run/netns/x",
  veth_host: "veth0",
  veth_in: "veth1",
  tap_name: "tap0",
  mac: "02:00:00:00:00:01",
  ip: "10.0.0.2",
  gateway: "10.0.0.1",
  vmm_api_sock: "/tmp/api.sock",
  vsock_ud_path: "/tmp/vsock.sock",
  serial_path: "/tmp/serial.log",
  snapshot_dir: "/tmp/snap",
  guest_cid: 3,
  wake_count: 2,
  sleep_count: 1,
  exec_count: 4,
  last_activity_at: "2026-01-01T00:00:00Z",
  workload_finished: true,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};

export const STATS = {
  sandbox_id: "sandbox-1",
  name: "web",
  state: "Running",
  cpu_percent: 12.5,
  vcpus: 2,
  memory_mb: 512,
  memory_rss_bytes: 123456,
  guest_memory_used_bytes: 100,
  guest_memory_total_bytes: 200,
  guest_memory_ballooned_bytes: 10,
  uptime_seconds: 60,
  wake_count: 2,
  block_read_bytes: 1,
  block_write_bytes: 2,
  rx_bytes: 3,
  tx_bytes: 4,
  captured_at: "2026-01-01T00:00:00Z",
};

export const DISK = {
  name: "demo:latest",
  tag: "demo:latest",
  tags: ["demo:latest"],
  digest: "sha256:abc",
  path: "/images/abc.img",
  size_bytes: 1024,
  allocated_bytes: 512,
  source: "registry",
  created_at: "2026-01-01T00:00:00Z",
};

export const KERNEL = {
  name: "release-1-vmlinux-x86_64",
  path: "/kernels/release-1-vmlinux-x86_64",
  size_bytes: 1024,
  sha256: "abc",
  release: "release-1",
  asset: "vmlinux-x86_64",
  active: true,
};
