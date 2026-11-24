import type { ReactNode } from "react";
import { useEvents, useSandboxStats } from "../lib/queries";
import type { BaseDisk, Event, Sandbox } from "../lib/types";
import { Button, Drawer, cn } from "../ui";
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
} from "./vmActions";
import { ListBulletIcon, MoonIcon, PauseIcon, PlayIcon, TerminalIcon, TrashIcon } from "./icons";
import { UtilizationBar } from "./UtilizationBar";
import { VmStatusTag } from "./VmStatusTag";

export interface VmDetailDrawerProps {
  sandbox: Sandbox | null;
  open: boolean;
  images: readonly BaseDisk[];
  pendingAction: PendingAction | null;
  onClose: () => void;
  onAction: (kind: VmActionKind, sandbox: Sandbox) => void;
  /** Open an interactive shell; wakes paused/dormant sandboxes. */
  onExec: (sandbox: Sandbox) => void;
  /** Open the captured log viewer. */
  onLogs: (sandbox: Sandbox) => void;
}

type Tone = "neutral" | "success" | "warning" | "danger" | "info" | "accent";

const TONE_CLASS: Record<Tone, string> = {
  neutral: "bg-gray-100 text-gray-700 dark:bg-gray-800 dark:text-gray-300",
  success: "bg-green-100 text-green-700 dark:bg-green-900/40 dark:text-green-300",
  warning: "bg-amber-100 text-amber-700 dark:bg-amber-900/40 dark:text-amber-300",
  danger: "bg-red-100 text-red-700 dark:bg-red-900/40 dark:text-red-300",
  info: "bg-sky-100 text-sky-700 dark:bg-sky-900/40 dark:text-sky-300",
  accent: "bg-indigo-100 text-indigo-700 dark:bg-indigo-900/40 dark:text-indigo-300",
};

function Badge({
  tone = "neutral",
  children,
  title,
}: {
  tone?: Tone;
  children: ReactNode;
  title?: string;
}) {
  return (
    <span
      title={title}
      className={cn(
        "inline-flex w-fit items-center rounded-full px-2.5 py-0.5 text-xs font-medium whitespace-nowrap",
        TONE_CLASS[tone],
      )}
    >
      {children}
    </span>
  );
}

function InfoRow({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="grid grid-cols-1 gap-1 py-3 sm:grid-cols-3 sm:gap-4">
      <dt className="text-xs font-medium tracking-wide text-gray-500 uppercase dark:text-gray-400">
        {label}
      </dt>
      <dd className="text-sm break-words text-gray-900 sm:col-span-2 dark:text-white">
        {children}
      </dd>
    </div>
  );
}

function Notice({
  tone,
  title,
  description,
}: {
  tone: "warning" | "danger";
  title: string;
  description: ReactNode;
}) {
  return (
    <div
      role="alert"
      className={cn(
        "rounded-lg border p-3 text-sm",
        tone === "danger"
          ? "border-red-200 bg-red-50 dark:border-red-900 dark:bg-red-950/40"
          : "border-amber-200 bg-amber-50 dark:border-amber-900 dark:bg-amber-950/40",
      )}
    >
      <p
        className={cn(
          "font-medium",
          tone === "danger"
            ? "text-red-700 dark:text-red-300"
            : "text-amber-800 dark:text-amber-200",
        )}
      >
        {title}
      </p>
      <p
        className={cn(
          "mt-0.5 text-xs",
          tone === "danger"
            ? "text-red-600 dark:text-red-400"
            : "text-amber-700 dark:text-amber-300",
        )}
      >
        {description}
      </p>
    </div>
  );
}

function PulseBlock({ rows = 3 }: { rows?: number }) {
  return (
    <div className="space-y-2" role="status" aria-label="Loading details">
      {Array.from({ length: rows }, (_, index) => (
        <div key={index} className="h-3 animate-pulse rounded bg-gray-200 dark:bg-gray-800" />
      ))}
    </div>
  );
}

function eventTone(type: string): Tone {
  const normalized = type.toLowerCase();
  if (normalized.includes("fail") || normalized.includes("error")) {
    return "danger";
  }
  if (
    normalized.includes("wake") ||
    normalized.includes("start") ||
    normalized.includes("create")
  ) {
    return "success";
  }
  if (
    normalized.includes("pause") ||
    normalized.includes("sleep") ||
    normalized.includes("dormant")
  ) {
    return "warning";
  }
  if (normalized.includes("delete") || normalized.includes("stop")) {
    return "neutral";
  }
  return "info";
}

function eventMatches(event: Event, sandbox: Sandbox): boolean {
  return (
    event.sandbox_id === sandbox.id ||
    (event.sandbox_name !== undefined && event.sandbox_name === sandbox.name)
  );
}

/** Slide-over with full sandbox details, live stats, recent events and actions. */
export function VmDetailDrawer({
  sandbox,
  open,
  images,
  pendingAction,
  onClose,
  onAction,
  onExec,
  onLogs,
}: VmDetailDrawerProps) {
  const stats = useSandboxStats(sandbox?.id);
  const events = useEvents(100);

  if (sandbox === null) {
    return null;
  }

  const pending = pendingAction?.ref === sandbox.id ? pendingAction : null;
  const busy = pending !== null;
  const image = images.find((candidate) => candidate.digest === sandbox.base_disk);
  const sandboxEvents = (events.data ?? []).filter((event) => eventMatches(event, sandbox));
  const statsData = stats.data;

  const memoryLimitBytes = sandbox.memory_mb * 1024 * 1024;
  const guestMemoryUsed = statsData?.guest_memory_used_bytes;
  const guestMemoryTotal = statsData?.guest_memory_total_bytes;
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

  let statsSection: ReactNode;
  if (stats.isPending) {
    statsSection = <PulseBlock rows={4} />;
  } else if (stats.isError) {
    statsSection = (
      <Notice
        tone="warning"
        title="Live stats unavailable"
        description={describeError(stats.error)}
      />
    );
  } else if (statsData === undefined) {
    statsSection = <p className="text-sm text-gray-500 dark:text-gray-400">No stats yet.</p>;
  } else {
    statsSection = (
      <div>
        <div className="space-y-4">
          <UtilizationBar
            label="CPU"
            percent={statsData.cpu_percent}
            showHistory
            detail={`${statsData.cpu_percent.toFixed(1)}% of ${statsData.vcpus} vCPU${
              statsData.vcpus === 1 ? "" : "s"
            }`}
          />
          {hasGuestMemory ? (
            <UtilizationBar
              label="Memory (guest)"
              percent={memoryPercent}
              showHistory
              detail={`${formatBytes(guestMemoryUsed)} / ${formatBytes(
                guestMemoryTotal ?? memoryLimitBytes,
              )} used`}
            />
          ) : (
            <div>
              <div className="flex items-baseline justify-between gap-3">
                <span className="text-sm font-medium text-gray-900 dark:text-white">
                  Memory (guest)
                </span>
                <span className="text-sm font-medium text-gray-400 tabular-nums dark:text-gray-500">
                  —
                </span>
              </div>
              <div className="mt-2 flex h-6 items-center rounded border border-dashed border-gray-300 px-2 text-xs text-gray-400 dark:border-gray-700 dark:text-gray-500">
                Waiting for guest memory stats…
              </div>
              <div className="mt-1.5 text-xs text-gray-500 dark:text-gray-400">
                This appears after the guest agent reports its first sample.
              </div>
            </div>
          )}
        </div>
        <div className="mt-4 grid grid-cols-2 gap-3 sm:grid-cols-3">
          {[
            { label: "Uptime", value: formatDuration(statsData.uptime_seconds) },
            { label: "CPU", value: formatPercent(statsData.cpu_percent) },
            { label: "Host RSS", value: formatBytes(statsData.memory_rss_bytes) },
            { label: "Block read", value: formatBytes(statsData.block_read_bytes) },
            { label: "Block write", value: formatBytes(statsData.block_write_bytes) },
            { label: "Network RX", value: formatBytes(statsData.rx_bytes) },
            { label: "Network TX", value: formatBytes(statsData.tx_bytes) },
          ].map((counter) => (
            <div
              key={counter.label}
              className="rounded-lg border border-gray-200 bg-gray-50 p-3 dark:border-gray-800 dark:bg-gray-800/50"
            >
              <span className="text-xs font-medium tracking-wide text-gray-500 uppercase dark:text-gray-400">
                {counter.label}
              </span>
              <span className="mt-0.5 block font-mono text-sm text-gray-900 dark:text-white">
                {counter.value}
              </span>
            </div>
          ))}
        </div>
        <p className="mt-3 text-xs text-gray-500 dark:text-gray-400">
          Sampled {formatDateTime(statsData.captured_at)}
        </p>
      </div>
    );
  }

  const footer = (
    <div className="flex w-full flex-wrap items-center justify-between gap-3">
      <div className="flex flex-wrap gap-2">
        <Button
          variant="primary"
          onClick={() => onExec(sandbox)}
          disabled={busy || !canExec(sandbox.state)}
          icon={<TerminalIcon className="size-4" />}
        >
          Open terminal
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
      </div>
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
  );

  return (
    <Drawer
      open={open}
      onClose={onClose}
      widthClassName="w-full max-w-[560px]"
      title={
        <div className="flex min-w-0 items-center gap-2">
          <span className="truncate">{sandbox.name}</span>
          <VmStatusTag state={sandbox.state} />
        </div>
      }
      subtitle={<span className="font-mono">{shortId(sandbox.id, 14)}</span>}
      footer={footer}
    >
      <div className="space-y-6">
        <section>
          <h3 className="text-sm font-semibold text-gray-900 dark:text-white">Overview</h3>
          <dl className="mt-3 divide-y divide-gray-200 text-sm dark:divide-gray-800">
            <InfoRow label="Sandbox ID">
              <span className="font-mono break-all" title={sandbox.id}>
                {sandbox.id}
              </span>
            </InfoRow>
            <InfoRow label="Base image">
              <span>
                {image === undefined ? shortId(sandbox.base_disk, 18) : imageLabel(image)}
                {image !== undefined ? (
                  <span className="text-gray-500 dark:text-gray-400">
                    {" "}
                    · {formatBytes(image.size_bytes)}
                  </span>
                ) : null}
              </span>
            </InfoRow>
            <InfoRow label="State">
              <VmStatusTag state={sandbox.state} />
            </InfoRow>
            <InfoRow label="IP address">
              <span className="font-mono">{sandbox.ip ?? DASH}</span>
            </InfoRow>
            <InfoRow label="Resources">
              <span>
                {sandbox.vcpus} vCPU{sandbox.vcpus === 1 ? "" : "s"} · {sandbox.memory_mb} MB memory
              </span>
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
            <InfoRow label="Created">
              <span>{formatDateTime(sandbox.created_at)}</span>
            </InfoRow>
            <InfoRow label="Last activity">
              <span>{formatDateTime(sandbox.last_activity_at)}</span>
            </InfoRow>
          </dl>
          {sandbox.error !== undefined && sandbox.error !== "" ? (
            <div className="mt-4">
              <Notice tone="danger" title="Sandbox error" description={sandbox.error} />
            </div>
          ) : null}
        </section>

        <section className="border-t border-gray-100 pt-5 dark:border-gray-800">
          <div className="flex items-baseline justify-between gap-3">
            <h3 className="text-sm font-semibold text-gray-900 dark:text-white">
              Live utilization
            </h3>
            <span className="text-xs text-gray-500 dark:text-gray-400">updates every 2s</span>
          </div>
          <div className="mt-3">{statsSection}</div>
        </section>

        <section className="border-t border-gray-100 pt-5 dark:border-gray-800">
          <h3 className="text-sm font-semibold text-gray-900 dark:text-white">Port mappings</h3>
          {sandbox.ports !== undefined && sandbox.ports.length > 0 ? (
            <ul className="mt-3 space-y-2">
              {sandbox.ports.map((port) => (
                <li
                  key={`${port.host_port}-${port.guest_port}-${port.proto}`}
                  className="flex items-center justify-between gap-3 rounded-lg border border-gray-200 px-3 py-2 text-sm dark:border-gray-800"
                >
                  <span className="font-mono text-xs text-gray-700 dark:text-gray-300">
                    {port.host_port} → {port.guest_port}/{port.proto}
                  </span>
                  <Badge tone="neutral">{port.proto}</Badge>
                </li>
              ))}
            </ul>
          ) : (
            <p className="mt-3 text-sm text-gray-500 dark:text-gray-400">No published ports.</p>
          )}
        </section>

        <section className="border-t border-gray-100 pt-5 dark:border-gray-800">
          <h3 className="text-sm font-semibold text-gray-900 dark:text-white">Host volumes</h3>
          {sandbox.volumes !== undefined && sandbox.volumes.length > 0 ? (
            <ul className="mt-3 space-y-2">
              {sandbox.volumes.map((volume, index) => (
                <li
                  key={`${volume.host_path}-${volume.guest_path}-${index}`}
                  className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-gray-200 px-3 py-2 text-sm dark:border-gray-800"
                >
                  <span className="min-w-0 text-xs break-all text-gray-700 dark:text-gray-300">
                    <span className="font-mono">{volume.host_path}</span>
                    <span className="text-gray-400 dark:text-gray-500"> → </span>
                    <span className="font-mono">{volume.guest_path}</span>
                  </span>
                  <Badge tone={volume.read_only === true ? "warning" : "success"}>
                    {volume.read_only === true ? "read-only" : "read-write"}
                  </Badge>
                </li>
              ))}
            </ul>
          ) : (
            <p className="mt-3 text-sm text-gray-500 dark:text-gray-400">
              No host volumes mounted.
            </p>
          )}
        </section>

        <section className="border-t border-gray-100 pt-5 dark:border-gray-800">
          <h3 className="text-sm font-semibold text-gray-900 dark:text-white">Data volumes</h3>
          {sandbox.data_volumes !== undefined && sandbox.data_volumes.length > 0 ? (
            <ul className="mt-3 space-y-2">
              {sandbox.data_volumes.map((volume, index) => (
                <li
                  key={`${volume.mount}-${index}`}
                  className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-gray-200 px-3 py-2 text-sm dark:border-gray-800"
                >
                  <span className="min-w-0 text-xs break-all text-gray-700 dark:text-gray-300">
                    <span className="font-mono">{volume.size_gb} GiB</span>
                    <span className="text-gray-400 dark:text-gray-500"> → </span>
                    <span className="font-mono">{volume.mount}</span>
                  </span>
                  <Badge tone="neutral">block</Badge>
                </li>
              ))}
            </ul>
          ) : (
            <p className="mt-3 text-sm text-gray-500 dark:text-gray-400">
              No data volumes attached.
            </p>
          )}
        </section>

        <section className="border-t border-gray-100 pt-5 dark:border-gray-800">
          <div className="flex items-baseline justify-between gap-3">
            <h3 className="text-sm font-semibold text-gray-900 dark:text-white">Recent events</h3>
            <span className="text-xs text-gray-500 dark:text-gray-400">
              {sandboxEvents.length} shown
            </span>
          </div>
          {events.isPending ? (
            <div className="mt-3">
              <PulseBlock rows={3} />
            </div>
          ) : events.isError ? (
            <div className="mt-3">
              <Notice
                tone="warning"
                title="Could not load events"
                description={describeError(events.error)}
              />
            </div>
          ) : sandboxEvents.length === 0 ? (
            <p className="mt-3 text-sm text-gray-500 dark:text-gray-400">
              No events recorded for this sandbox yet.
            </p>
          ) : (
            <ul className="mt-3 divide-y divide-gray-200 dark:divide-gray-800">
              {sandboxEvents.map((event) => (
                <li
                  key={`${event.time}-${event.type}-${event.message}`}
                  className="flex flex-col gap-1 py-2.5 sm:flex-row sm:items-start sm:justify-between"
                >
                  <div className="flex min-w-0 flex-col gap-1">
                    <Badge tone={eventTone(event.type)}>{event.type}</Badge>
                    <span className="text-sm break-words text-gray-700 dark:text-gray-300">
                      {event.message}
                    </span>
                  </div>
                  <span className="shrink-0 font-mono text-xs text-gray-500 dark:text-gray-400">
                    {formatDateTime(event.time)}
                  </span>
                </li>
              ))}
            </ul>
          )}
        </section>
      </div>
    </Drawer>
  );
}

export default VmDetailDrawer;
