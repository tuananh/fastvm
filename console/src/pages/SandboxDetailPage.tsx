import type { ReactNode } from "react";
import { ArrowLeftIcon } from "@heroicons/react/24/outline";
import { Button, cn } from "../ui";
import { useEvents, useSandboxStats } from "../lib/queries";
import type { BaseDisk, Event, Sandbox } from "../lib/types";
import { describeError } from "../utils/errors";
import {
  clampPercent,
  DASH,
  formatBytes,
  formatDateTime,
  formatDuration,
  formatPercent,
  imageLabel,
  shortId,
} from "../utils/format";
import {
  canDeepSleep,
  canDelete,
  canExec,
  canPause,
  canResume,
  type PendingAction,
  type VmActionKind,
} from "../components/vmActions";
import {
  ListBulletIcon,
  MoonIcon,
  PauseIcon,
  PlayIcon,
  TerminalIcon,
  TrashIcon,
} from "../components/icons";
import { UtilizationBar } from "../components/UtilizationBar";
import { VmStatusTag } from "../components/VmStatusTag";

export interface SandboxDetailPageProps {
  sandbox: Sandbox;
  images: readonly BaseDisk[];
  pendingAction: PendingAction | null;
  onBack: () => void;
  onAction: (kind: VmActionKind, sandbox: Sandbox) => void;
  onExec: (sandbox: Sandbox) => void;
  onLogs: (sandbox: Sandbox) => void;
}

type Tone = "neutral" | "success" | "warning" | "danger" | "info";

const TONE_CLASS: Record<Tone, string> = {
  neutral: "border-[var(--app-border)] text-[var(--app-muted)]",
  success: "border-[var(--om-green)]/40 text-[var(--om-green)]",
  warning: "border-[var(--om-yellow)]/40 text-[var(--om-yellow)]",
  danger: "border-[var(--om-red)]/40 text-[var(--om-red)]",
  info: "border-[var(--om-blue)]/40 text-[var(--om-blue)]",
};

function Badge({ tone = "neutral", children }: { tone?: Tone; children: ReactNode }) {
  return (
    <span
      className={cn(
        "inline-flex w-fit items-center border px-2 py-0.5 text-[11px] whitespace-nowrap",
        TONE_CLASS[tone],
      )}
    >
      {children}
    </span>
  );
}

function Card({
  title,
  aside,
  children,
}: {
  title: string;
  aside?: ReactNode;
  children: ReactNode;
}) {
  return (
    <section className="border border-[var(--app-border)] bg-[var(--app-panel)]">
      <header className="flex items-center justify-between gap-3 border-b border-[var(--app-border)] px-4 py-3">
        <h2 className="text-[12px] font-semibold text-[var(--app-text)]">{title}</h2>
        {aside !== undefined ? (
          <span className="text-[11px] text-[var(--app-muted)]">{aside}</span>
        ) : null}
      </header>
      <div className="p-4">{children}</div>
    </section>
  );
}

function InfoRow({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="grid grid-cols-1 gap-1 border-b border-[var(--app-border)] py-2.5 last:border-b-0 sm:grid-cols-[150px_1fr] sm:gap-4">
      <dt className="text-[10px] font-medium tracking-[0.1em] text-[var(--app-muted)] uppercase">
        {label}
      </dt>
      <dd className="text-[12px] break-words text-[var(--app-text)]">{children}</dd>
    </div>
  );
}

function text(value: unknown): string {
  return typeof value === "string" && value !== "" ? value : DASH;
}

function eventTone(type: string): Tone {
  const normalized = type.toLowerCase();
  if (normalized.includes("fail") || normalized.includes("error")) return "danger";
  if (normalized.includes("wake") || normalized.includes("start") || normalized.includes("create"))
    return "success";
  if (
    normalized.includes("pause") ||
    normalized.includes("sleep") ||
    normalized.includes("dormant")
  )
    return "warning";
  return "info";
}

function eventMatches(event: Event, sandbox: Sandbox): boolean {
  return (
    event.sandbox_id === sandbox.id ||
    (event.sandbox_name !== undefined && event.sandbox_name === sandbox.name)
  );
}

/** Full-page sandbox inspector, replacing the old slide-over drawer. */
export function SandboxDetailPage({
  sandbox,
  images,
  pendingAction,
  onBack,
  onAction,
  onExec,
  onLogs,
}: SandboxDetailPageProps) {
  const stats = useSandboxStats(sandbox.id);
  const events = useEvents(100);
  const pending = pendingAction?.ref === sandbox.id ? pendingAction : null;
  const busy = pending !== null;
  const image = images.find((candidate) => candidate.digest === sandbox.base_disk);
  const statsData = stats.data;
  const sandboxEvents = (events.data ?? []).filter((event) => eventMatches(event, sandbox));

  const memoryLimitBytes = sandbox.memory_mb * 1024 * 1024;
  const guestMemoryUsed = statsData?.guest_memory_used_bytes;
  const guestMemoryTotal = statsData?.guest_memory_total_bytes;
  const guestMemoryBallooned = statsData?.guest_memory_ballooned_bytes;
  const hasGuestMemory = guestMemoryUsed !== undefined && guestMemoryUsed > 0;
  const memoryPercent =
    statsData === undefined
      ? 0
      : clampPercent(
          memoryLimitBytes > 0
            ? ((hasGuestMemory ? guestMemoryUsed : statsData.memory_rss_bytes) / memoryLimitBytes) *
                100
            : 0,
        );

  return (
    <div className="h-full overflow-y-auto bg-[var(--app-bg)]">
      <div className="mx-auto max-w-[1220px] px-4 py-6 sm:px-6 lg:px-8">
        <button
          type="button"
          onClick={onBack}
          className="mb-5 inline-flex items-center gap-1.5 text-[11px] text-[var(--app-muted)] hover:text-[var(--app-text)]"
        >
          <ArrowLeftIcon className="size-3.5" /> All sandboxes
        </button>

        <div className="flex flex-wrap items-start justify-between gap-4">
          <div className="min-w-0">
            <div className="flex flex-wrap items-center gap-2">
              <h1 className="text-[22px] font-semibold tracking-tight text-[var(--app-text)]">
                {sandbox.name}
              </h1>
              <VmStatusTag state={sandbox.state} />
            </div>
            <p className="mt-1 font-mono text-[11px] text-[var(--app-muted)]">{sandbox.id}</p>
          </div>

          <div className="flex flex-wrap items-center gap-2">
            <Button
              variant="primary"
              onClick={() => onExec(sandbox)}
              disabled={busy || !canExec(sandbox.state)}
              icon={<TerminalIcon className="size-4" />}
            >
              Terminal
            </Button>
            <Button onClick={() => onLogs(sandbox)} icon={<ListBulletIcon className="size-4" />}>
              Logs
            </Button>
            <Button
              onClick={() => onAction("pause", sandbox)}
              disabled={busy || !canPause(sandbox.state)}
              loading={pending?.kind === "pause"}
              icon={<PauseIcon className="size-4" />}
            >
              Pause
            </Button>
            <Button
              onClick={() => onAction("deepSleep", sandbox)}
              disabled={busy || !canDeepSleep(sandbox.state)}
              loading={pending?.kind === "deepSleep"}
              icon={<MoonIcon className="size-4" />}
            >
              Deep sleep
            </Button>
            <Button
              onClick={() => onAction("resume", sandbox)}
              disabled={busy || !canResume(sandbox.state)}
              loading={pending?.kind === "resume"}
              icon={<PlayIcon className="size-4" />}
            >
              Resume
            </Button>
            <Button
              variant="danger"
              onClick={() => onAction("delete", sandbox)}
              disabled={busy || !canDelete(sandbox.state)}
              loading={pending?.kind === "delete"}
              icon={<TrashIcon className="size-4" />}
            >
              Delete
            </Button>
          </div>
        </div>

        {sandbox.error !== undefined && sandbox.error !== "" ? (
          <div className="mt-4 border border-[var(--om-red)]/40 bg-[var(--om-red)]/10 p-3 text-[12px] text-[var(--om-red)]">
            <p className="font-medium">Sandbox error</p>
            <p className="mt-0.5">{sandbox.error}</p>
          </div>
        ) : null}

        <div className="mt-5 grid gap-4 lg:grid-cols-[minmax(0,1fr)_320px]">
          <div className="space-y-4">
            <Card title="Live utilization" aside="updates every 2s">
              {stats.isPending ? (
                <div className="space-y-2" role="status" aria-label="Loading details">
                  {[0, 1, 2].map((row) => (
                    <div key={row} className="h-3 animate-pulse bg-[var(--app-panel-2)]" />
                  ))}
                </div>
              ) : stats.isError ? (
                <p className="text-[12px] text-[var(--app-muted)]">
                  Live stats unavailable: {describeError(stats.error)}
                </p>
              ) : statsData === undefined ? (
                <p className="text-[12px] text-[var(--app-muted)]">No stats yet.</p>
              ) : (
                <div className="space-y-4">
                  <UtilizationBar
                    label="CPU"
                    percent={statsData.cpu_percent}
                    showHistory
                    detail={`${statsData.cpu_percent.toFixed(1)}% of ${statsData.vcpus} vCPU${statsData.vcpus === 1 ? "" : "s"}`}
                  />
                  {hasGuestMemory ? (
                    <UtilizationBar
                      label="Memory (guest)"
                      percent={memoryPercent}
                      showHistory
                      detail={`${formatBytes(guestMemoryUsed)} / ${formatBytes(guestMemoryTotal ?? memoryLimitBytes)} used`}
                    />
                  ) : (
                    <div className="text-[12px] text-[var(--app-muted)]">
                      Waiting for guest memory stats…
                    </div>
                  )}
                  <div className="grid grid-cols-2 gap-2 sm:grid-cols-4">
                    {[
                      { label: "Uptime", value: formatDuration(statsData.uptime_seconds) },
                      { label: "CPU", value: formatPercent(statsData.cpu_percent) },
                      { label: "Host RSS", value: formatBytes(statsData.memory_rss_bytes) },
                      ...(guestMemoryBallooned !== undefined && guestMemoryBallooned > 0
                        ? [{ label: "Ballooned", value: formatBytes(guestMemoryBallooned) }]
                        : []),
                      { label: "Block read", value: formatBytes(statsData.block_read_bytes) },
                      { label: "Block write", value: formatBytes(statsData.block_write_bytes) },
                      { label: "Network RX", value: formatBytes(statsData.rx_bytes) },
                      { label: "Network TX", value: formatBytes(statsData.tx_bytes) },
                    ].map((counter) => (
                      <div
                        key={counter.label}
                        className="border border-[var(--app-border)] bg-[var(--app-panel-2)] p-2.5"
                      >
                        <span className="text-[10px] tracking-[0.08em] text-[var(--app-muted)] uppercase">
                          {counter.label}
                        </span>
                        <span className="mt-0.5 block font-mono text-[12px] text-[var(--app-text)]">
                          {counter.value}
                        </span>
                      </div>
                    ))}
                  </div>
                  <p className="text-[10px] text-[var(--app-faint)]">
                    Sampled {formatDateTime(statsData.captured_at)}
                  </p>
                </div>
              )}
            </Card>

            <Card title="Overview">
              <dl>
                <InfoRow label="Sandbox ID">
                  <span className="font-mono">{sandbox.id}</span>
                </InfoRow>
                <InfoRow label="Base image">
                  {image === undefined ? shortId(sandbox.base_disk, 18) : imageLabel(image)}
                  {image !== undefined ? (
                    <span className="text-[var(--app-muted)]">
                      {" "}
                      · {formatBytes(image.size_bytes)}
                    </span>
                  ) : null}
                </InfoRow>
                <InfoRow label="Kernel">
                  <span className="font-mono">
                    {sandbox.kernel === undefined || sandbox.kernel === ""
                      ? "daemon default"
                      : text(sandbox.kernel)}
                  </span>
                </InfoRow>
                <InfoRow label="IP address">
                  <span className="font-mono">{sandbox.ip ?? DASH}</span>
                </InfoRow>
                <InfoRow label="Resources">
                  {sandbox.vcpus} vCPU{sandbox.vcpus === 1 ? "" : "s"}
                  {sandbox.max_vcpus !== undefined && sandbox.max_vcpus > sandbox.vcpus
                    ? ` (max ${sandbox.max_vcpus})`
                    : ""}{" "}
                  · {sandbox.memory_mb} MB memory
                  {sandbox.max_memory_mb !== undefined && sandbox.max_memory_mb > sandbox.memory_mb
                    ? ` (max ${sandbox.max_memory_mb} MB)`
                    : ""}
                </InfoRow>
                <InfoRow label="Root disk">
                  {sandbox.root_size_gb === undefined ? DASH : `${sandbox.root_size_gb} GiB`}
                </InfoRow>
                <InfoRow label="Auto sleep">
                  {sandbox.no_auto_sleep ? (
                    <Badge tone="warning">Disabled</Badge>
                  ) : (
                    <Badge tone="success">Enabled</Badge>
                  )}
                </InfoRow>
                <InfoRow label="Wake count">
                  <span className="font-mono">{sandbox.wake_count}</span>
                </InfoRow>
                <InfoRow label="Created">{formatDateTime(sandbox.created_at)}</InfoRow>
                <InfoRow label="Last activity">{formatDateTime(sandbox.last_activity_at)}</InfoRow>
                <InfoRow label="Updated">{formatDateTime(sandbox.updated_at)}</InfoRow>
              </dl>
            </Card>

            <div className="grid gap-4 sm:grid-cols-2">
              <Card title="Host volumes" aside={`${sandbox.volumes?.length ?? 0}`}>
                {sandbox.volumes !== undefined && sandbox.volumes.length > 0 ? (
                  <ul className="space-y-2">
                    {sandbox.volumes.map((volume, index) => (
                      <li
                        key={`${volume.host_path}-${volume.guest_path}-${index}`}
                        className="border border-[var(--app-border)] px-3 py-2"
                      >
                        <div className="break-all font-mono text-[11px] text-[var(--app-text)]">
                          {volume.host_path} → {volume.guest_path}
                        </div>
                        <Badge tone={volume.read_only === true ? "warning" : "success"}>
                          {volume.read_only === true ? "read-only" : "read-write"}
                        </Badge>
                      </li>
                    ))}
                  </ul>
                ) : (
                  <p className="text-[12px] text-[var(--app-muted)]">No host volumes mounted.</p>
                )}
              </Card>

              <Card title="Data volumes" aside={`${sandbox.data_volumes?.length ?? 0}`}>
                {sandbox.data_volumes !== undefined && sandbox.data_volumes.length > 0 ? (
                  <ul className="space-y-2">
                    {sandbox.data_volumes.map((volume, index) => (
                      <li
                        key={`${volume.mount}-${index}`}
                        className="border border-[var(--app-border)] px-3 py-2"
                      >
                        <div className="break-all font-mono text-[11px] text-[var(--app-text)]">
                          {volume.size_gb} GiB → {volume.mount}
                        </div>
                        <Badge>block</Badge>
                      </li>
                    ))}
                  </ul>
                ) : (
                  <p className="text-[12px] text-[var(--app-muted)]">No data volumes attached.</p>
                )}
              </Card>
            </div>

            <Card title="Recent events" aside={`${sandboxEvents.length} shown`}>
              {events.isPending ? (
                <div className="space-y-2">
                  {[0, 1, 2].map((row) => (
                    <div key={row} className="h-3 animate-pulse bg-[var(--app-panel-2)]" />
                  ))}
                </div>
              ) : events.isError ? (
                <p className="text-[12px] text-[var(--app-muted)]">
                  Could not load events: {describeError(events.error)}
                </p>
              ) : sandboxEvents.length === 0 ? (
                <p className="text-[12px] text-[var(--app-muted)]">No events recorded yet.</p>
              ) : (
                <ul className="divide-y divide-[var(--app-border)]">
                  {sandboxEvents.map((event) => (
                    <li
                      key={`${event.time}-${event.type}-${event.message}`}
                      className="flex flex-col gap-1 py-2.5 sm:flex-row sm:items-start sm:justify-between"
                    >
                      <div className="min-w-0">
                        <Badge tone={eventTone(event.type)}>{event.type}</Badge>
                        <p className="mt-1 text-[12px] break-words text-[var(--app-text)]">
                          {event.message}
                        </p>
                      </div>
                      <span className="shrink-0 font-mono text-[10px] text-[var(--app-muted)]">
                        {formatDateTime(event.time)}
                      </span>
                    </li>
                  ))}
                </ul>
              )}
            </Card>
          </div>

          <aside className="space-y-4">
            <Card title="Image">
              <p className="font-mono text-[12px] text-[var(--app-text)]">
                {image === undefined ? shortId(sandbox.base_disk, 18) : imageLabel(image)}
              </p>
              <p className="mt-1 break-all font-mono text-[10px] text-[var(--app-muted)]">
                {sandbox.base_disk}
              </p>
            </Card>
            <Card title="Port mappings">
              {sandbox.ports !== undefined && sandbox.ports.length > 0 ? (
                <ul className="space-y-2">
                  {sandbox.ports.map((port) => (
                    <li
                      key={`${port.host_port}-${port.guest_port}-${port.proto}`}
                      className="flex items-center justify-between gap-3 border border-[var(--app-border)] px-3 py-2"
                    >
                      <span className="font-mono text-[11px] text-[var(--app-text)]">
                        {port.host_port} → {port.guest_port}/{port.proto}
                      </span>
                      <Badge>{port.proto}</Badge>
                    </li>
                  ))}
                </ul>
              ) : (
                <p className="text-[12px] text-[var(--app-muted)]">No published ports.</p>
              )}
            </Card>
            <Card title="Connections">
              <dl>
                <InfoRow label="IP">{sandbox.ip ?? DASH}</InfoRow>
                <InfoRow label="TAP">{text(sandbox.tap_name)}</InfoRow>
              </dl>
            </Card>
          </aside>
        </div>
      </div>
    </div>
  );
}

export default SandboxDetailPage;
