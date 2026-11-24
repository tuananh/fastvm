import { z } from "zod";

// Schemas mirror the JSON emitted by fastvmd (`internal/types` + `internal/api`).
// `.passthrough()` keeps the console tolerant of additive daemon fields.

/** Lifecycle states of a sandbox (docs/DESIGN.md §5.1). */
export const sandboxStateSchema = z.enum([
  "Creating",
  "Created",
  "Starting",
  "Running",
  "Idle",
  "Paused",
  "Dormant",
  "Resuming",
  "Stopping",
  "Stopped",
  "Failed",
]);
export type SandboxState = z.infer<typeof sandboxStateSchema>;

/** `GET /v1/version` */
export const versionInfoSchema = z
  .object({
    version: z.string(),
    name: z.string().optional(),
  })
  .passthrough();
export type VersionInfo = z.infer<typeof versionInfoSchema>;

/** Host port -> guest port mapping. */
export const portMappingSchema = z.object({
  host_port: z.number().int(),
  guest_port: z.number().int(),
  proto: z.string(),
});
export type PortMapping = z.infer<typeof portMappingSchema>;

/** Host-directory volume mounted into the guest with virtio-fs. */
export const volumeSchema = z
  .object({
    host_path: z.string(),
    guest_path: z.string(),
    read_only: z.boolean().optional(),
  })
  .passthrough();
export type Volume = z.infer<typeof volumeSchema>;

export const dataVolumeSchema = z
  .object({
    name: z.string().optional(),
    size_gb: z.number().int(),
    mount: z.string(),
    path: z.string().optional(),
  })
  .passthrough();
export type DataVolume = z.infer<typeof dataVolumeSchema>;

/** `GET /v1/sandboxes` and `GET /v1/sandboxes/{ref}` */
export const sandboxSchema = z
  .object({
    id: z.string(),
    name: z.string(),
    state: sandboxStateSchema,
    base_disk: z.string(),
    vcpus: z.number().int(),
    memory_mb: z.number().int(),
    ip: z.string().optional(),
    wake_count: z.number().int(),
    no_auto_sleep: z.boolean(),
    last_activity_at: z.string(),
    created_at: z.string(),
    updated_at: z.string(),
    ports: z.array(portMappingSchema).optional(),
    volumes: z.array(volumeSchema).optional(),
    data_volumes: z.array(dataVolumeSchema).optional(),
    error: z.string().optional(),
  })
  .passthrough();
export type Sandbox = z.infer<typeof sandboxSchema>;

/** `GET /v1/images` element: a read-only, content-addressed base disk. */
export const baseDiskSchema = z
  .object({
    name: z.string().optional(),
    tag: z.string().optional(),
    tags: z.array(z.string()).optional(),
    digest: z.string(),
    size_bytes: z.number().int(),
    source: z.string(),
    created_at: z.string(),
  })
  .passthrough();
export type BaseDisk = z.infer<typeof baseDiskSchema>;

/** `GET /v1/kernels` element. */
export const kernelInfoSchema = z
  .object({
    name: z.string(),
    release: z.string().optional(),
    asset: z.string().optional(),
    size_bytes: z.number().int(),
    sha256: z.string().optional(),
    active: z.boolean(),
  })
  .passthrough();
export type KernelInfo = z.infer<typeof kernelInfoSchema>;

/** `GET /v1/kernels` */
export const kernelListSchema = z
  .object({
    dir: z.string(),
    current: z.string().optional(),
    kernels: z.array(kernelInfoSchema),
  })
  .passthrough();
export type KernelList = z.infer<typeof kernelListSchema>;

/** `GET /v1/events` element. */
export const eventSchema = z
  .object({
    time: z.string(),
    sandbox_id: z.string(),
    sandbox_name: z.string().optional(),
    type: z.string(),
    message: z.string(),
  })
  .passthrough();
export type Event = z.infer<typeof eventSchema>;

/** `GET /v1/sandboxes/{ref}/stats` (exact daemon contract). */
export const sandboxStatsSchema = z
  .object({
    sandbox_id: z.string(),
    name: z.string(),
    state: sandboxStateSchema,
    cpu_percent: z.number().min(0).max(100),
    vcpus: z.number().int(),
    memory_mb: z.number().int(),
    memory_rss_bytes: z.number().int(),
    guest_memory_used_bytes: z.number().int().optional(),
    guest_memory_total_bytes: z.number().int().optional(),
    uptime_seconds: z.number(),
    wake_count: z.number().int(),
    block_read_bytes: z.number().int(),
    block_write_bytes: z.number().int(),
    rx_bytes: z.number().int(),
    tx_bytes: z.number().int(),
    captured_at: z.string(),
  })
  .passthrough();
export type SandboxStats = z.infer<typeof sandboxStatsSchema>;

export const sandboxListSchema = z.array(sandboxSchema);
export const baseDiskListSchema = z.array(baseDiskSchema);
export const eventListSchema = z.array(eventSchema);
