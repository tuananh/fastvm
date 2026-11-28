export type SandboxState =
  | "Creating"
  | "Created"
  | "Starting"
  | "Running"
  | "Idle"
  | "Paused"
  | "Dormant"
  | "Resuming"
  | "Stopping"
  | "Stopped"
  | "Failed";

export interface PortMapping {
  hostPort: number;
  guestPort: number;
  proto: "tcp" | "udp";
  loopbackOnly?: boolean;
}

export interface Volume {
  hostPath: string;
  guestPath: string;
  readOnly?: boolean;
}

export interface DataVolume {
  name?: string;
  sizeGb: number;
  mount: string;
  path?: string;
}

export interface Sandbox {
  id: string;
  name: string;
  baseDisk: string;
  kernel?: string;
  kernelPath?: string;
  volumePath: string;
  rootSizeGb?: number;
  vcpus: number;
  maxVcpus?: number;
  memoryMb: number;
  maxMemoryMb?: number;
  state: SandboxState;
  workdir?: string;
  autoPublish?: boolean;
  noAutoSleep: boolean;
  networkEnabled: boolean;
  allowHosts?: string[];
  ports?: PortMapping[];
  volumes?: Volume[];
  dataVolumes?: DataVolume[];
  netnsPath?: string;
  vethHost?: string;
  vethIn?: string;
  tapName?: string;
  mac?: string;
  ip?: string;
  gateway?: string;
  vmmapiSock?: string;
  vsockUDPath?: string;
  serialPath?: string;
  snapshotDir?: string;
  guestCID?: number;
  wakeCount: number;
  sleepCount?: number;
  execCount?: number;
  lastActivityAt: string;
  workloadFinished?: boolean;
  error?: string;
  createdAt: string;
  updatedAt: string;
}

export interface SandboxStats {
  sandboxId: string;
  name: string;
  state: SandboxState;
  cpuPercent: number;
  vcpus: number;
  memoryMb: number;
  memoryRssBytes: number;
  guestMemoryUsedBytes?: number;
  guestMemoryTotalBytes?: number;
  guestMemoryBalloonedBytes?: number;
  uptimeSeconds: number;
  wakeCount: number;
  blockReadBytes: number;
  blockWriteBytes: number;
  rxBytes: number;
  txBytes: number;
  capturedAt: string;
}

export interface BaseDisk {
  name?: string;
  tag?: string;
  tags?: string[];
  digest: string;
  path: string;
  sizeBytes: number;
  allocatedBytes: number;
  source: string;
  createdAt: string;
}

export interface Event {
  time: string;
  sandboxId: string;
  sandboxName?: string;
  type: string;
  message: string;
}

export interface SnapshotInfo {
  name: string;
  createdAt: string;
  imageDigest: string;
  vcpus: number;
  memoryMb: number;
  networkEnabled: boolean;
}

export interface KernelInfo {
  name: string;
  path?: string;
  sizeBytes: number;
  sha256?: string;
  release?: string;
  asset?: string;
  active: boolean;
}

export interface KernelList {
  dir: string;
  current?: string;
  kernels: KernelInfo[];
}

export interface KernelRemoteInfo {
  release: string;
  asset: string;
  sizeBytes: number;
  sha256?: string;
}

export interface KernelRemoteList {
  kernels: KernelRemoteInfo[];
}

export interface VersionInfo {
  version: string;
  name: string;
}

export interface SandboxCreateOptions {
  image: string;
  name?: string;
  kernel?: string;
  cpu?: number;
  maxCpu?: number;
  memoryMb?: number;
  maxMemoryMb?: number;
  rootSizeGb?: number;
  ports?: PortMapping[];
  cmd?: string[];
  env?: Record<string, string>;
  workdir?: string;
  autoPublish?: boolean;
  noAutoSleep?: boolean;
  volumes?: Volume[];
  dataVolumes?: DataVolume[];
  start?: boolean;
  network?: boolean;
  allowHosts?: string[];
  fromSnapshot?: string;
  userData?: string;
}

export interface ExecOptions {
  env?: Record<string, string>;
  timeoutMs?: number;
  signal?: AbortSignal;
}

export interface ExecResponse {
  id: string;
  exitCode: number;
  stdout?: string;
  stderr?: string;
  ports?: number[];
  fileSize?: number;
  fileMode?: number;
  data?: string;
  error?: string;
  memoryTotalBytes?: number;
  memoryAvailableBytes?: number;
  memoryUsedBytes?: number;
  memoryBalloonedBytes?: number;
}

export interface ConsoleOptions {
  offset?: number;
  signal?: AbortSignal;
}

export interface CopyFromOptions {
  offset?: number;
  signal?: AbortSignal;
}

export interface CopyToOptions {
  mode?: number;
  signal?: AbortSignal;
}

export interface GuestFile {
  data: Uint8Array;
  mode: number;
  size: number;
}

export interface ResizeOptions {
  cpu?: number;
  memoryMb?: number;
}

export interface PullImageOptions {
  sizeGb?: number;
  signal?: AbortSignal;
}

export interface IngestImageOptions {
  rootfsDir: string;
  tag: string;
  signal?: AbortSignal;
}

export interface RemoveImageOptions {
  force?: boolean;
}

export interface KernelDownloadOptions {
  release?: string;
  asset?: string;
  force?: boolean;
  signal?: AbortSignal;
}

export interface KernelDownloadResult {
  path: string;
  release: string;
  asset: string;
  sha256: string;
}

export interface KernelUseResult {
  path: string;
  current: string;
}

export interface KernelRemoveResult {
  release: string;
  path: string;
}

export interface ShellOptions {
  argv?: string[];
  rows?: number;
  cols?: number;
}

export type ShellEvent =
  | { type: "output"; data: Uint8Array }
  | { type: "exit"; code: number }
  | { type: "error"; message: string };

export interface ShellHandle {
  write(data: string | Uint8Array): void;
  resize(rows: number, cols: number): void;
  close(): void;
  events(): AsyncIterable<ShellEvent>;
}
