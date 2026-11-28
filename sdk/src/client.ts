import { ExecResult } from "./result.js";
import { ShellSession } from "./shell.js";
import { LegacyShellSession } from "./legacy-shell.js";
import { Transport } from "./transport.js";
import type { FastVMOptions } from "./transport.js";
import type {
  BaseDisk,
  ConsoleOptions,
  CopyFromOptions,
  CopyToOptions,
  Event,
  ExecOptions,
  GuestFile,
  IngestImageOptions,
  KernelDownloadOptions,
  KernelDownloadResult,
  KernelList,
  KernelRemoteList,
  KernelRemoveResult,
  KernelUseResult,
  PullImageOptions,
  RemoveImageOptions,
  ResizeOptions,
  SandboxCreateOptions,
  SandboxStats,
  ShellHandle,
  ShellOptions,
  SnapshotInfo,
} from "./types.js";
import type { Sandbox as SandboxInfo } from "./types.js";
import {
  createBody,
  toBaseDisk,
  toEvent,
  toKernelList,
  toRemoteKernelList,
  toSandbox,
  toSnapshot,
  toStats,
} from "./map.js";

type Wire = Record<string, unknown>;

/** One micro-VM. Instances are returned by {@link FastVM.sandboxes.create}. */
export interface Sandbox extends SandboxInfo {}

export class Sandbox {
  readonly client: FastVM;
  readonly data: SandboxInfo;

  constructor(client: FastVM, data: SandboxInfo) {
    this.client = client;
    this.data = data;
    Object.assign(this, data);
  }

  /** Low-level command execution; maps to POST /v1/sandboxes/{ref}/exec. */
  async exec(argv: string[], options: ExecOptions = {}): Promise<ExecResult> {
    const response = await this.client.transport.request<Wire>(
      "POST",
      `/v1/sandboxes/${encodeURIComponent(this.id)}/exec`,
      { argv, ...(options.env === undefined ? {} : { env: options.env }) },
      {
        ...(options.signal === undefined ? {} : { signal: options.signal }),
        ...(options.timeoutMs === undefined ? {} : { timeoutMs: options.timeoutMs }),
      },
    );
    const result = new ExecResult({
      exitCode: Number(response.exit_code ?? 0),
      ...(typeof response.stdout === "string" ? { stdout: response.stdout } : {}),
      ...(typeof response.stderr === "string" ? { stderr: response.stderr } : {}),
    });
    return result;
  }

  /** Alias for {@link exec}. */
  run(argv: string[], options: ExecOptions = {}): Promise<ExecResult> {
    return this.exec(argv, options);
  }

  stats(options: { signal?: AbortSignal } = {}): Promise<SandboxStats> {
    return this.client.transport
      .request<Wire>(
        "GET",
        `/v1/sandboxes/${encodeURIComponent(this.id)}/stats`,
        undefined,
        options,
      )
      .then(toStats);
  }

  async refresh(): Promise<this> {
    const data = toSandbox(
      await this.client.transport.request<Wire>(
        "GET",
        `/v1/sandboxes/${encodeURIComponent(this.id)}`,
      ),
    );
    Object.assign(this, data);
    return this;
  }

  resize(options: ResizeOptions): Promise<SandboxInfo> {
    return this.client.transport
      .request<Wire>("PATCH", `/v1/sandboxes/${encodeURIComponent(this.id)}/resources`, {
        ...(options.cpu === undefined ? {} : { vcpus: options.cpu }),
        ...(options.memoryMb === undefined ? {} : { memory_mb: options.memoryMb }),
      })
      .then(toSandbox);
  }

  async delete(): Promise<void> {
    await this.client.transport.request<void>(
      "DELETE",
      `/v1/sandboxes/${encodeURIComponent(this.id)}`,
    );
  }

  async start(): Promise<void> {
    await this.client.transport.request<void>(
      "POST",
      `/v1/sandboxes/${encodeURIComponent(this.id)}/start`,
    );
  }

  async sleep(options: { deep?: boolean } = {}): Promise<void> {
    await this.client.transport.request<void>(
      "POST",
      `/v1/sandboxes/${encodeURIComponent(this.id)}/sleep`,
      {
        deep: options.deep === true,
      },
    );
  }

  async pause(): Promise<void> {
    await this.sleep();
  }

  async deepSleep(): Promise<void> {
    await this.sleep({ deep: true });
  }

  async resume(): Promise<void> {
    await this.client.transport.request<void>(
      "POST",
      `/v1/sandboxes/${encodeURIComponent(this.id)}/wake`,
    );
  }

  async wake(): Promise<void> {
    await this.resume();
  }

  async snapshot(name?: string): Promise<string> {
    const response = await this.client.transport.request<{ snapshot: string }>(
      "POST",
      `/v1/sandboxes/${encodeURIComponent(this.id)}/snapshot`,
      name === undefined ? {} : { name },
    );
    return response.snapshot;
  }

  snapshotCopy(name: string): Promise<SandboxInfo> {
    return this.client.transport
      .request<Wire>("POST", `/v1/sandboxes/${encodeURIComponent(this.id)}/snapshot/copy`, { name })
      .then(toSandbox);
  }

  console(options: ConsoleOptions = {}): Promise<string> {
    const query = options.offset === undefined ? "" : `?offset=${options.offset}`;
    return this.client.transport
      .text(`/v1/sandboxes/${encodeURIComponent(this.id)}/console${query}`, options)
      .then((response) => response.text);
  }

  async writeFile(
    path: string,
    data: string | Uint8Array,
    options: CopyToOptions = {},
  ): Promise<void> {
    const bytes = typeof data === "string" ? new TextEncoder().encode(data) : data;
    const params = new URLSearchParams({ path });
    if (options.mode !== undefined) params.set("mode", options.mode.toString(8));
    await this.client.transport.binary(
      "PUT",
      `/v1/sandboxes/${encodeURIComponent(this.id)}/file?${params.toString()}`,
      bytes,
      options,
    );
  }

  async readFile(path: string, options: CopyFromOptions = {}): Promise<GuestFile> {
    const params = new URLSearchParams({ path });
    if (options.offset !== undefined) params.set("offset", String(options.offset));
    const response = await this.client.transport.binary(
      "GET",
      `/v1/sandboxes/${encodeURIComponent(this.id)}/file?${params.toString()}`,
      undefined,
      options,
    );
    return {
      data: response.data,
      mode: Number.parseInt(response.headers.get("x-fastvm-mode") ?? "0", 8),
      size: Number(response.headers.get("x-fastvm-size") ?? response.data.byteLength),
    };
  }

  shell(options: ShellOptions = {}): Promise<ShellHandle> {
    return ShellSession.open(this.client.transport, this.id, options);
  }

  legacyShell(options: ShellOptions = {}): Promise<ShellHandle> {
    return LegacyShellSession.open(this.client.transport, this.id, options);
  }

  async logs(source: "stdout" | "stderr" = "stdout", offset?: number): Promise<string> {
    const paths = {
      stdout: "/var/log/fastvm/stdout.log",
      stderr: "/var/log/fastvm/stderr.log",
    } as const;
    const file = await this.readFile(paths[source], offset === undefined ? {} : { offset });
    return new TextDecoder().decode(file.data);
  }
}

export class SandboxesAPI {
  constructor(private readonly client: FastVM) {}

  async list(options: { signal?: AbortSignal } = {}): Promise<Sandbox[]> {
    const list = await this.client.transport.request<Wire[]>(
      "GET",
      "/v1/sandboxes",
      undefined,
      options,
    );
    return list.map((item) => new Sandbox(this.client, toSandbox(item)));
  }

  async create(
    options: SandboxCreateOptions,
    request: { signal?: AbortSignal } = {},
  ): Promise<Sandbox> {
    const raw = await this.client.transport.request<Wire>(
      "POST",
      "/v1/sandboxes",
      createBody(options, true),
      request,
    );
    return new Sandbox(this.client, toSandbox(raw));
  }

  async get(ref: string, options: { signal?: AbortSignal } = {}): Promise<Sandbox> {
    const raw = await this.client.transport.request<Wire>(
      "GET",
      `/v1/sandboxes/${encodeURIComponent(ref)}`,
      undefined,
      options,
    );
    return new Sandbox(this.client, toSandbox(raw));
  }

  async stats(ref: string, options: { signal?: AbortSignal } = {}): Promise<SandboxStats> {
    return toStats(
      await this.client.transport.request<Wire>(
        "GET",
        `/v1/sandboxes/${encodeURIComponent(ref)}/stats`,
        undefined,
        options,
      ),
    );
  }

  async statsAll(options: { signal?: AbortSignal } = {}): Promise<SandboxStats[]> {
    const list = await this.client.transport.request<Wire[]>(
      "GET",
      "/v1/sandboxes/stats",
      undefined,
      options,
    );
    return list.map(toStats);
  }
}

export class ImagesAPI {
  constructor(private readonly client: FastVM) {}

  async list(options: { signal?: AbortSignal } = {}): Promise<BaseDisk[]> {
    const list = await this.client.transport.request<Wire[]>(
      "GET",
      "/v1/images",
      undefined,
      options,
    );
    return list.map(toBaseDisk);
  }

  async pull(ref: string, options: PullImageOptions = {}): Promise<BaseDisk> {
    return toBaseDisk(
      await this.client.transport.request<Wire>(
        "POST",
        "/v1/images/pull",
        { ref, ...(options.sizeGb === undefined ? {} : { size_gb: options.sizeGb }) },
        options,
      ),
    );
  }

  async ingest(options: IngestImageOptions): Promise<BaseDisk> {
    return toBaseDisk(
      await this.client.transport.request<Wire>(
        "POST",
        "/v1/images/ingest",
        { rootfs_dir: options.rootfsDir, tag: options.tag },
        options,
      ),
    );
  }

  async remove(ref: string, options: RemoveImageOptions = {}): Promise<BaseDisk> {
    const params = new URLSearchParams({ ref });
    if (options.force === true) params.set("force", "true");
    return toBaseDisk(
      await this.client.transport.request<Wire>("DELETE", `/v1/images?${params.toString()}`),
    );
  }
}

export class KernelsAPI {
  constructor(private readonly client: FastVM) {}

  async list(options: { signal?: AbortSignal } = {}): Promise<KernelList> {
    return toKernelList(
      await this.client.transport.request<Wire>("GET", "/v1/kernels", undefined, options),
    );
  }

  async remoteList(options: { signal?: AbortSignal } = {}): Promise<KernelRemoteList> {
    return toRemoteKernelList(
      await this.client.transport.request<Wire>("GET", "/v1/kernels/remote", undefined, options),
    );
  }

  download(options: KernelDownloadOptions = {}): Promise<KernelDownloadResult> {
    return this.client.transport.request<KernelDownloadResult>(
      "POST",
      "/v1/kernels/download",
      {
        ...(options.release === undefined ? {} : { release: options.release }),
        ...(options.asset === undefined ? {} : { asset: options.asset }),
        ...(options.force === undefined ? {} : { force: options.force }),
      },
      options,
    );
  }

  use(release: string): Promise<KernelUseResult> {
    return this.client.transport.request<KernelUseResult>("POST", "/v1/kernels/use", { release });
  }

  remove(release: string): Promise<KernelRemoveResult> {
    return this.client.transport.request<KernelRemoveResult>(
      "DELETE",
      `/v1/kernels?${new URLSearchParams({ release }).toString()}`,
    );
  }
}

export interface SnapshotCopyOptions {
  name: string;
}

export interface RunOptions extends SandboxCreateOptions {
  command: string[];
  exec?: ExecOptions;
}

export class FastVM {
  readonly transport: Transport;
  readonly sandboxes: SandboxesAPI;
  readonly images: ImagesAPI;
  readonly kernels: KernelsAPI;

  constructor(options: FastVMOptions = {}) {
    this.transport = new Transport(options);
    this.sandboxes = new SandboxesAPI(this);
    this.images = new ImagesAPI(this);
    this.kernels = new KernelsAPI(this);
  }

  static async connect(options: FastVMOptions = {}): Promise<FastVM> {
    return new FastVM(options);
  }

  version(options: { signal?: AbortSignal } = {}): Promise<{ version: string; name: string }> {
    return this.transport.request("GET", "/v1/version", undefined, options);
  }

  metrics(options: { signal?: AbortSignal } = {}): Promise<string> {
    return this.transport.text("/metrics", options).then((response) => response.text);
  }

  events(limit = 50, options: { signal?: AbortSignal } = {}): Promise<Event[]> {
    return this.transport
      .request<Wire[]>("GET", `/v1/events?limit=${limit}`, undefined, options)
      .then((items) => items.map(toEvent));
  }

  listSnapshots(options: { signal?: AbortSignal } = {}): Promise<SnapshotInfo[]> {
    return this.transport
      .request<Wire[]>("GET", "/v1/snapshots", undefined, options)
      .then((items) => items.map(toSnapshot));
  }

  async removeSnapshot(name: string): Promise<void> {
    await this.transport.request<void>("DELETE", `/v1/snapshots/${encodeURIComponent(name)}`);
  }

  /** Create, run one command, and delete the sandbox. */
  async run(options: RunOptions): Promise<ExecResult> {
    const sandbox = await this.sandboxes.create(options);
    try {
      return await sandbox.exec(options.command, options.exec ?? {});
    } finally {
      await sandbox.delete();
    }
  }

  shell(ref: string, options: ShellOptions = {}): Promise<ShellHandle> {
    return ShellSession.open(this.transport, ref, options);
  }

  legacyShell(ref: string, options: ShellOptions = {}): Promise<ShellHandle> {
    return LegacyShellSession.open(this.transport, ref, options);
  }

  async close(): Promise<void> {
    await this.transport.close();
  }
}
