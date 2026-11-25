import { z } from "zod";
import {
  baseDiskListSchema,
  baseDiskSchema,
  eventListSchema,
  kernelListSchema,
  sandboxListSchema,
  sandboxSchema,
  sandboxStatsSchema,
  versionInfoSchema,
} from "./types";
import type {
  BaseDisk,
  DataVolume,
  Event,
  KernelList,
  Sandbox,
  SandboxStats,
  VersionInfo,
  Volume,
} from "./types";

/** JSON error envelope returned by fastvmd (`internal/api.ErrorEnvelope`). */
export interface ApiErrorBody {
  error?: string;
  kind?: string;
}

/** Error raised for any non-2xx fastvmd response. */
export class ApiError extends Error {
  readonly status: number;
  readonly kind?: string;

  constructor(status: number, message: string, kind?: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    if (kind !== undefined) {
      this.kind = kind;
    }
  }
}

function parseErrorBody(text: string): ApiErrorBody | undefined {
  try {
    const parsed: unknown = JSON.parse(text);
    if (typeof parsed === "object" && parsed !== null) {
      return parsed as ApiErrorBody;
    }
    return undefined;
  } catch {
    return undefined;
  }
}

async function toApiError(response: Response): Promise<ApiError> {
  const text = await response.text().catch(() => "");
  const body = parseErrorBody(text);
  const fallback = `HTTP ${response.status}${response.statusText ? ` ${response.statusText}` : ""}`;
  const message = typeof body?.error === "string" && body.error !== "" ? body.error : fallback;
  return new ApiError(response.status, message, body?.kind);
}

async function readPayload(response: Response): Promise<unknown> {
  const text = await response.text();
  if (text.trim() === "") {
    return undefined;
  }
  try {
    return JSON.parse(text) as unknown;
  } catch {
    throw new ApiError(response.status, `fastvmd returned invalid JSON (HTTP ${response.status})`);
  }
}

/**
 * Same-origin API request. The dev/preview server proxies `/v1/*` to fastvmd,
 * so relative paths are always used.
 */
export async function request<T>(
  path: string,
  init: RequestInit = {},
  schema?: z.ZodType<T>,
): Promise<T> {
  const headers: Record<string, string> = { Accept: "application/json" };
  if (init.body !== undefined) {
    headers["Content-Type"] = "application/json";
  }
  const response = await fetch(path, { ...init, headers });
  if (!response.ok) {
    throw await toApiError(response);
  }
  const payload = await readPayload(response);
  if (schema === undefined) {
    return payload as T;
  }
  return schema.parse(payload);
}

async function requestText(path: string): Promise<string> {
  const response = await fetch(path, { headers: { Accept: "text/plain" } });
  if (!response.ok) {
    if (response.status === 404) {
      return "";
    }
    throw await toApiError(response);
  }
  return response.text();
}

const API_PREFIX = "/v1";

function sandboxPath(ref: string): string {
  return `${API_PREFIX}/sandboxes/${encodeURIComponent(ref)}`;
}

/** `GET /v1/version` */
export function getVersion(): Promise<VersionInfo> {
  return request(`${API_PREFIX}/version`, {}, versionInfoSchema);
}

/** `GET /v1/sandboxes` */
export function listSandboxes(): Promise<Sandbox[]> {
  return request(`${API_PREFIX}/sandboxes`, {}, sandboxListSchema);
}

/** `GET /v1/sandboxes/{ref}` */
export function getSandbox(ref: string): Promise<Sandbox> {
  return request(sandboxPath(ref), {}, sandboxSchema);
}

/** Log source exposed by the console. PTY output is intentionally absent. */
export type SandboxLogSource = "app" | "console" | "system";

const GUEST_STDOUT = "/var/log/fastvm/stdout.log";
const GUEST_STDERR = "/var/log/fastvm/stderr.log";

async function getGuestLog(ref: string, path: string): Promise<string> {
  const params = new URLSearchParams({ path });
  try {
    return await requestText(`${sandboxPath(ref)}/file?${params.toString()}`);
  } catch (error) {
    // A freshly created sandbox has no log file yet. Older daemons surface this
    // as a 500 "no such file" error; treat it as an empty log.
    if (error instanceof ApiError && /no such file or directory/i.test(error.message)) {
      return "";
    }
    throw error;
  }
}

/** `GET /v1/sandboxes/{ref}/console` */
export function getSandboxConsole(ref: string): Promise<string> {
  return requestText(`${sandboxPath(ref)}/console`);
}

/** Captured logs for one console source. */
export async function getSandboxLog(ref: string, source: SandboxLogSource): Promise<string> {
  if (source === "console") {
    return getSandboxConsole(ref);
  }
  if (source === "system") {
    const [events, sandbox] = await Promise.all([listEvents(1000), getSandbox(ref)]);
    return events
      .filter((event) => event.sandbox_id === sandbox.id)
      .map((event) => `${event.time} ${event.type.padEnd(7)} ${event.message}`)
      .join("\n");
  }
  const [stdout, stderr] = await Promise.all([
    getGuestLog(ref, GUEST_STDOUT),
    getGuestLog(ref, GUEST_STDERR),
  ]);
  if (stdout === "" && stderr === "") {
    return "";
  }
  const parts: string[] = [];
  if (stdout !== "") {
    parts.push(`==> stdout <==\n${stdout.replace(/\n$/, "")}`);
  }
  if (stderr !== "") {
    parts.push(`==> stderr <==\n${stderr.replace(/\n$/, "")}`);
  }
  return parts.join("\n");
}

/** `GET /v1/sandboxes/{ref}/stats` */
export function getSandboxStats(ref: string): Promise<SandboxStats> {
  return request(`${sandboxPath(ref)}/stats`, {}, sandboxStatsSchema);
}

/** `GET /v1/kernels` */
export function listKernels(): Promise<KernelList> {
  return request(`${API_PREFIX}/kernels`, {}, kernelListSchema);
}

/** `GET /v1/images` */
export function listImages(): Promise<BaseDisk[]> {
  return request(`${API_PREFIX}/images`, {}, baseDiskListSchema);
}

/** Request body for `POST /v1/images/pull`. */
export interface PullImageInput {
  ref: string;
  size_gb?: number;
}

/** `POST /v1/images/pull` */
export function pullImage(input: PullImageInput): Promise<BaseDisk> {
  return request(
    `${API_PREFIX}/images/pull`,
    { method: "POST", body: JSON.stringify(input) },
    baseDiskSchema,
  );
}

/** `DELETE /v1/images?ref=...&force=...` */
export function removeImage(ref: string, force = false): Promise<BaseDisk> {
  const params = new URLSearchParams({ ref });
  if (force) {
    params.set("force", "true");
  }
  return request(`${API_PREFIX}/images?${params.toString()}`, { method: "DELETE" }, baseDiskSchema);
}

/** `GET /v1/events?limit=N` */
export function listEvents(limit = 50): Promise<Event[]> {
  return request(
    `${API_PREFIX}/events?limit=${encodeURIComponent(String(limit))}`,
    {},
    eventListSchema,
  );
}

/** `POST /v1/sandboxes/{ref}/sleep` */
export function sleepSandbox(ref: string, deep: boolean): Promise<void> {
  return request<void>(`${sandboxPath(ref)}/sleep`, {
    method: "POST",
    body: JSON.stringify({ deep }),
  });
}

/** `POST /v1/sandboxes/{ref}/wake` */
export function wakeSandbox(ref: string): Promise<void> {
  return request<void>(`${sandboxPath(ref)}/wake`, { method: "POST" });
}

/** `DELETE /v1/sandboxes/{ref}` */
export function deleteSandbox(ref: string): Promise<void> {
  return request<void>(sandboxPath(ref), { method: "DELETE" });
}

/** Request body for `POST /v1/sandboxes`. */
export interface CreateSandboxInput {
  name?: string;
  image: string;
  /** Installed kernel release; omitted uses the daemon current kernel. */
  kernel?: string;
  vcpus?: number;
  max_vcpus?: number;
  memory_mb?: number;
  max_memory_mb?: number;
  cmd?: string[];
  no_auto_sleep?: boolean;
  /** Host directories to mount into the guest with virtio-fs. */
  volumes?: Volume[];
  /** Writable block volumes cloned from a blank ext4 template. */
  data_volumes?: DataVolume[];
}

/** `POST /v1/sandboxes` */
export function createSandbox(input: CreateSandboxInput): Promise<Sandbox> {
  return request(
    `${API_PREFIX}/sandboxes`,
    { method: "POST", body: JSON.stringify(input) },
    sandboxSchema,
  );
}
