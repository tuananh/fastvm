import type {
  BaseDisk,
  DataVolume,
  Event,
  KernelInfo,
  KernelList,
  KernelRemoteInfo,
  KernelRemoteList,
  PortMapping,
  Sandbox,
  SandboxCreateOptions,
  SandboxStats,
  SnapshotInfo,
  Volume,
} from "./types.js";

type Wire = Record<string, unknown>;

const str = (v: unknown): string => (typeof v === "string" ? v : "");
const num = (v: unknown): number => (typeof v === "number" ? v : 0);
const bool = (v: unknown): boolean => v === true;

function ports(value: unknown): PortMapping[] | undefined {
  if (!Array.isArray(value)) return undefined;
  return value.map((raw) => {
    const v = raw as Wire;
    return {
      hostPort: num(v.host_port),
      guestPort: num(v.guest_port),
      proto: str(v.proto) === "udp" ? "udp" : "tcp",
      ...(bool(v.loopback_only) ? { loopbackOnly: true } : {}),
    };
  });
}

function volumes(value: unknown): Volume[] | undefined {
  if (!Array.isArray(value)) return undefined;
  return value.map((raw) => {
    const v = raw as Wire;
    return {
      hostPath: str(v.host_path),
      guestPath: str(v.guest_path),
      ...(bool(v.read_only) ? { readOnly: true } : {}),
    };
  });
}

function dataVolumes(value: unknown): DataVolume[] | undefined {
  if (!Array.isArray(value)) return undefined;
  return value.map((raw) => {
    const v = raw as Wire;
    return {
      ...(typeof v.name === "string" && v.name !== "" ? { name: v.name } : {}),
      sizeGb: num(v.size_gb),
      mount: str(v.mount),
      ...(typeof v.path === "string" && v.path !== "" ? { path: v.path } : {}),
    };
  });
}

export function toSandbox(raw: Wire): Sandbox {
  return {
    id: str(raw.id),
    name: str(raw.name),
    baseDisk: str(raw.base_disk),
    ...(typeof raw.kernel === "string" && raw.kernel !== "" ? { kernel: raw.kernel } : {}),
    ...(typeof raw.kernel_path === "string" && raw.kernel_path !== ""
      ? { kernelPath: raw.kernel_path }
      : {}),
    volumePath: str(raw.volume_path),
    ...(typeof raw.root_size_gb === "number" && raw.root_size_gb > 0
      ? { rootSizeGb: raw.root_size_gb }
      : {}),
    vcpus: num(raw.vcpus),
    ...(typeof raw.max_vcpus === "number" && raw.max_vcpus > 0 ? { maxVcpus: raw.max_vcpus } : {}),
    memoryMb: num(raw.memory_mb),
    ...(typeof raw.max_memory_mb === "number" && raw.max_memory_mb > 0
      ? { maxMemoryMb: raw.max_memory_mb }
      : {}),
    state: str(raw.state) as Sandbox["state"],
    ...(typeof raw.workdir === "string" && raw.workdir !== "" ? { workdir: raw.workdir } : {}),
    ...(bool(raw.auto_publish) ? { autoPublish: true } : {}),
    noAutoSleep: bool(raw.no_auto_sleep),
    networkEnabled: bool(raw.network_enabled),
    ...(Array.isArray(raw.allow_hosts) ? { allowHosts: raw.allow_hosts.map(str) } : {}),
    ...(ports(raw.ports) === undefined ? {} : { ports: ports(raw.ports) }),
    ...(volumes(raw.volumes) === undefined ? {} : { volumes: volumes(raw.volumes) }),
    ...(dataVolumes(raw.data_volumes) === undefined
      ? {}
      : { dataVolumes: dataVolumes(raw.data_volumes) }),
    ...(typeof raw.netns_path === "string" && raw.netns_path !== ""
      ? { netnsPath: raw.netns_path }
      : {}),
    ...(typeof raw.veth_host === "string" && raw.veth_host !== ""
      ? { vethHost: raw.veth_host }
      : {}),
    ...(typeof raw.veth_in === "string" && raw.veth_in !== "" ? { vethIn: raw.veth_in } : {}),
    ...(typeof raw.tap_name === "string" && raw.tap_name !== "" ? { tapName: raw.tap_name } : {}),
    ...(typeof raw.mac === "string" && raw.mac !== "" ? { mac: raw.mac } : {}),
    ...(typeof raw.ip === "string" && raw.ip !== "" ? { ip: raw.ip } : {}),
    ...(typeof raw.gateway === "string" && raw.gateway !== "" ? { gateway: raw.gateway } : {}),
    ...(typeof raw.vmm_api_sock === "string" && raw.vmm_api_sock !== ""
      ? { vmmapiSock: raw.vmm_api_sock }
      : {}),
    ...(typeof raw.vsock_ud_path === "string" && raw.vsock_ud_path !== ""
      ? { vsockUDPath: raw.vsock_ud_path }
      : {}),
    ...(typeof raw.serial_path === "string" && raw.serial_path !== ""
      ? { serialPath: raw.serial_path }
      : {}),
    ...(typeof raw.snapshot_dir === "string" && raw.snapshot_dir !== ""
      ? { snapshotDir: raw.snapshot_dir }
      : {}),
    ...(typeof raw.guest_cid === "number" && raw.guest_cid > 0 ? { guestCID: raw.guest_cid } : {}),
    wakeCount: num(raw.wake_count),
    ...(typeof raw.sleep_count === "number" ? { sleepCount: raw.sleep_count } : {}),
    ...(typeof raw.exec_count === "number" ? { execCount: raw.exec_count } : {}),
    lastActivityAt: str(raw.last_activity_at),
    ...(bool(raw.workload_finished) ? { workloadFinished: true } : {}),
    ...(typeof raw.error === "string" && raw.error !== "" ? { error: raw.error } : {}),
    createdAt: str(raw.created_at),
    updatedAt: str(raw.updated_at),
  };
}

export function toStats(raw: Wire): SandboxStats {
  return {
    sandboxId: str(raw.sandbox_id),
    name: str(raw.name),
    state: str(raw.state) as SandboxStats["state"],
    cpuPercent: num(raw.cpu_percent),
    vcpus: num(raw.vcpus),
    memoryMb: num(raw.memory_mb),
    memoryRssBytes: num(raw.memory_rss_bytes),
    ...(typeof raw.guest_memory_used_bytes === "number"
      ? { guestMemoryUsedBytes: raw.guest_memory_used_bytes }
      : {}),
    ...(typeof raw.guest_memory_total_bytes === "number"
      ? { guestMemoryTotalBytes: raw.guest_memory_total_bytes }
      : {}),
    ...(typeof raw.guest_memory_ballooned_bytes === "number"
      ? { guestMemoryBalloonedBytes: raw.guest_memory_ballooned_bytes }
      : {}),
    uptimeSeconds: num(raw.uptime_seconds),
    wakeCount: num(raw.wake_count),
    blockReadBytes: num(raw.block_read_bytes),
    blockWriteBytes: num(raw.block_write_bytes),
    rxBytes: num(raw.rx_bytes),
    txBytes: num(raw.tx_bytes),
    capturedAt: str(raw.captured_at),
  };
}

export function toBaseDisk(raw: Wire): BaseDisk {
  return {
    ...(typeof raw.name === "string" && raw.name !== "" ? { name: raw.name } : {}),
    ...(typeof raw.tag === "string" && raw.tag !== "" ? { tag: raw.tag } : {}),
    ...(Array.isArray(raw.tags) ? { tags: raw.tags.map(str) } : {}),
    digest: str(raw.digest),
    path: str(raw.path),
    sizeBytes: num(raw.size_bytes),
    allocatedBytes: num(raw.allocated_bytes),
    source: str(raw.source),
    createdAt: str(raw.created_at),
  };
}

export function toEvent(raw: Wire): Event {
  return {
    time: str(raw.time),
    sandboxId: str(raw.sandbox_id),
    ...(typeof raw.sandbox_name === "string" && raw.sandbox_name !== ""
      ? { sandboxName: raw.sandbox_name }
      : {}),
    type: str(raw.type),
    message: str(raw.message),
  };
}

export function toSnapshot(raw: Wire): SnapshotInfo {
  return {
    name: str(raw.name),
    createdAt: str(raw.created_at),
    imageDigest: str(raw.image_digest),
    vcpus: num(raw.vcpus),
    memoryMb: num(raw.memory_mb),
    networkEnabled: bool(raw.network_enabled),
  };
}

export function toKernel(raw: Wire): KernelInfo {
  return {
    name: str(raw.name),
    ...(typeof raw.path === "string" && raw.path !== "" ? { path: raw.path } : {}),
    sizeBytes: num(raw.size_bytes),
    ...(typeof raw.sha256 === "string" && raw.sha256 !== "" ? { sha256: raw.sha256 } : {}),
    ...(typeof raw.release === "string" && raw.release !== "" ? { release: raw.release } : {}),
    ...(typeof raw.asset === "string" && raw.asset !== "" ? { asset: raw.asset } : {}),
    active: bool(raw.active),
  };
}

export function toKernelList(raw: Wire): KernelList {
  return {
    dir: str(raw.dir),
    ...(typeof raw.current === "string" && raw.current !== "" ? { current: raw.current } : {}),
    kernels: Array.isArray(raw.kernels) ? raw.kernels.map((item) => toKernel(item as Wire)) : [],
  };
}

export function toRemoteKernel(raw: Wire): KernelRemoteInfo {
  return {
    release: str(raw.release),
    asset: str(raw.asset),
    sizeBytes: num(raw.size_bytes),
    ...(typeof raw.sha256 === "string" && raw.sha256 !== "" ? { sha256: raw.sha256 } : {}),
  };
}

export function toRemoteKernelList(raw: Wire): KernelRemoteList {
  return {
    kernels: Array.isArray(raw.kernels)
      ? raw.kernels.map((item) => toRemoteKernel(item as Wire))
      : [],
  };
}

export function createBody(options: SandboxCreateOptions, wire: boolean): Wire {
  if (!wire) {
    return {
      image: options.image,
      ...(options.name === undefined ? {} : { name: options.name }),
      ...(options.kernel === undefined ? {} : { kernel: options.kernel }),
      ...(options.cpu === undefined ? {} : { cpu: options.cpu }),
      ...(options.maxCpu === undefined ? {} : { maxCpu: options.maxCpu }),
      ...(options.memoryMb === undefined ? {} : { memoryMb: options.memoryMb }),
      ...(options.maxMemoryMb === undefined ? {} : { maxMemoryMb: options.maxMemoryMb }),
      ...(options.rootSizeGb === undefined ? {} : { rootSizeGb: options.rootSizeGb }),
      ...(options.ports === undefined ? {} : { ports: options.ports }),
      ...(options.cmd === undefined ? {} : { cmd: options.cmd }),
      ...(options.env === undefined ? {} : { env: options.env }),
      ...(options.workdir === undefined ? {} : { workdir: options.workdir }),
      ...(options.autoPublish === undefined ? {} : { autoPublish: options.autoPublish }),
      ...(options.noAutoSleep === undefined ? {} : { noAutoSleep: options.noAutoSleep }),
      ...(options.volumes === undefined ? {} : { volumes: options.volumes }),
      ...(options.dataVolumes === undefined ? {} : { dataVolumes: options.dataVolumes }),
      ...(options.start === undefined ? {} : { start: options.start }),
      ...(options.network === undefined ? {} : { network: options.network }),
      ...(options.allowHosts === undefined ? {} : { allowHosts: options.allowHosts }),
      ...(options.fromSnapshot === undefined ? {} : { fromSnapshot: options.fromSnapshot }),
      ...(options.userData === undefined ? {} : { userData: options.userData }),
    };
  }
  return {
    image: options.image,
    ...(options.name === undefined ? {} : { name: options.name }),
    ...(options.kernel === undefined ? {} : { kernel: options.kernel }),
    ...(options.cpu === undefined ? {} : { vcpus: options.cpu }),
    ...(options.maxCpu === undefined ? {} : { max_vcpus: options.maxCpu }),
    ...(options.memoryMb === undefined ? {} : { memory_mb: options.memoryMb }),
    ...(options.maxMemoryMb === undefined ? {} : { max_memory_mb: options.maxMemoryMb }),
    ...(options.rootSizeGb === undefined ? {} : { root_size_gb: options.rootSizeGb }),
    ...(options.ports === undefined
      ? {}
      : {
          ports: options.ports.map((p) => ({
            host_port: p.hostPort,
            guest_port: p.guestPort,
            proto: p.proto,
            ...(p.loopbackOnly === true ? { loopback_only: true } : {}),
          })),
        }),
    ...(options.cmd === undefined ? {} : { cmd: options.cmd }),
    ...(options.env === undefined ? {} : { env: options.env }),
    ...(options.workdir === undefined ? {} : { workdir: options.workdir }),
    ...(options.autoPublish === undefined ? {} : { auto_publish: options.autoPublish }),
    ...(options.noAutoSleep === undefined ? {} : { no_auto_sleep: options.noAutoSleep }),
    ...(options.volumes === undefined
      ? {}
      : {
          volumes: options.volumes.map((v) => ({
            host_path: v.hostPath,
            guest_path: v.guestPath,
            ...(v.readOnly === true ? { read_only: true } : {}),
          })),
        }),
    ...(options.dataVolumes === undefined
      ? {}
      : {
          data_volumes: options.dataVolumes.map((v) => ({
            ...(v.name === undefined ? {} : { name: v.name }),
            size_gb: v.sizeGb,
            mount: v.mount,
            ...(v.path === undefined ? {} : { path: v.path }),
          })),
        }),
    ...(options.start === undefined ? {} : { start: options.start }),
    ...(options.network === undefined ? {} : { network_enabled: options.network }),
    ...(options.allowHosts === undefined ? {} : { allow_hosts: options.allowHosts }),
    ...(options.fromSnapshot === undefined ? {} : { from_snapshot: options.fromSnapshot }),
    ...(options.userData === undefined ? {} : { user_data: options.userData }),
  };
}
