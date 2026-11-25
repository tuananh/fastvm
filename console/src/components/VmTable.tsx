import { useQueries } from "@tanstack/react-query";
import { useState } from "react";
import { getSandboxStats } from "../lib/api";
import { queryKeys } from "../lib/queries";
import type { BaseDisk, Sandbox, SandboxState } from "../lib/types";
import { Button, Input, Select, Spinner, cn } from "../ui";
import {
  clampPercent,
  DASH,
  formatBytes,
  formatDuration,
  formatPercent,
  imageLabel,
  shortDigest,
  shortId,
} from "../utils/format";
import { MagnifyingGlassIcon, XMarkIcon } from "@heroicons/react/24/outline";
import { PlusIcon, ServerIcon } from "./icons";
import type { PendingAction, VmActionKind } from "./vmActions";
import { MiniUsageGraph } from "./UsageGraph";
import { VmActionsMenu } from "./VmActionsMenu";
import { VmStatusTag } from "./VmStatusTag";

export interface VmTableProps {
  sandboxes: readonly Sandbox[];
  images: readonly BaseDisk[];
  loading: boolean;
  error: string | null;
  pendingAction: PendingAction | null;
  onCreate: () => void;
  onRetry: () => void;
  onDetails: (sandbox: Sandbox) => void;
  onAction: (kind: VmActionKind, sandbox: Sandbox) => void;
  onExec: (sandbox: Sandbox) => void;
  onLogs: (sandbox: Sandbox) => void;
}

const PAGE_SIZE = 10;

const STATE_FILTERS: readonly SandboxState[] = [
  "Created",
  "Creating",
  "Starting",
  "Running",
  "Idle",
  "Paused",
  "Dormant",
  "Resuming",
  "Stopping",
  "Stopped",
  "Failed",
];

const TH_CLASS =
  "px-4 py-3 text-left text-xs font-medium uppercase tracking-wide text-gray-500 dark:text-gray-400";
const TD_CLASS = "px-4 py-3 align-middle text-sm";

function StatsPlaceholder({ title }: { title: string }) {
  return (
    <span className="text-gray-400 dark:text-gray-500" title={title}>
      {DASH}
    </span>
  );
}

/** Skeleton rows shown on the very first load (before any sandbox exists). */
function VmTableSkeleton() {
  return (
    <div
      className="divide-y divide-gray-200 dark:divide-gray-700"
      aria-label="Loading sandboxes"
      role="status"
    >
      {Array.from({ length: 5 }, (_, index) => (
        <div key={index} className="flex items-center gap-4 px-4 py-4">
          <div className="size-7 animate-pulse rounded-lg bg-gray-200 dark:bg-gray-700" />
          <div className="h-3 flex-1 animate-pulse rounded bg-gray-200 dark:bg-gray-700" />
          <div className="h-3 w-24 animate-pulse rounded bg-gray-200 dark:bg-gray-700" />
          <div className="h-3 w-16 animate-pulse rounded bg-gray-200 dark:bg-gray-700" />
        </div>
      ))}
    </div>
  );
}

/**
 * Sandbox list with live per-row utilization. Stats are fetched for the whole
 * visible list via `useQueries` (never inside cell renderers) so hook order is
 * stable while the polled list changes.
 */
export function VmTable({
  sandboxes,
  images,
  loading,
  error,
  pendingAction,
  onCreate,
  onRetry,
  onDetails,
  onAction,
  onExec,
  onLogs,
}: VmTableProps) {
  const [page, setPage] = useState(1);
  const [query, setQuery] = useState("");
  const [stateFilter, setStateFilter] = useState<SandboxState | "all">("all");

  const statsQueries = useQueries({
    queries: sandboxes.map((sandbox) => ({
      queryKey: queryKeys.sandboxStats(sandbox.id),
      queryFn: () => getSandboxStats(sandbox.id),
      refetchInterval: 2_000,
      retry: 0,
    })),
  });

  const statsByRef = new Map(
    sandboxes.map((sandbox, index) => [sandbox.id, statsQueries[index]] as const),
  );
  const imagesByDigest = new Map(images.map((image) => [image.digest, image] as const));
  const needle = query.trim().toLowerCase();
  const filtered = sandboxes.filter((sandbox) => {
    if (stateFilter !== "all" && sandbox.state !== stateFilter) {
      return false;
    }
    if (needle === "") {
      return true;
    }
    const image = imagesByDigest.get(sandbox.base_disk);
    const imageText = image === undefined ? shortDigest(sandbox.base_disk, 18) : imageLabel(image);
    return [sandbox.name, sandbox.id, sandbox.ip ?? "", sandbox.base_disk, imageText].some(
      (value) => value.toLowerCase().includes(needle),
    );
  });
  const filtersActive = needle !== "" || stateFilter !== "all";
  const clearFilters = () => {
    setQuery("");
    setStateFilter("all");
    setPage(1);
  };

  if (loading && sandboxes.length === 0) {
    return <VmTableSkeleton />;
  }

  if (error !== null && sandboxes.length === 0) {
    return (
      <div className="px-6 py-16 text-center">
        <div className="rounded-lg border border-red-200 bg-red-50 px-4 py-3 text-left dark:border-red-900 dark:bg-red-950/40">
          <p className="text-sm font-medium text-red-700 dark:text-red-300">
            Could not load sandboxes
          </p>
          <p className="mt-1 text-xs text-red-600 dark:text-red-400">{error}</p>
        </div>
        <Button className="mt-6" variant="secondary" onClick={onRetry}>
          Retry
        </Button>
      </div>
    );
  }

  if (sandboxes.length === 0) {
    return (
      <div className="px-6 py-16 text-center">
        <ServerIcon className="mx-auto size-12 text-gray-400 dark:text-gray-500" />
        <h3 className="mt-4 text-lg font-semibold text-gray-900 dark:text-white">
          No virtual machines yet
        </h3>
        <p className="mx-auto mt-2 max-w-sm text-sm text-gray-500 dark:text-gray-400">
          Create one from a base disk image. It boots from a copy-on-write volume and appears in the
          list immediately.
        </p>
        <Button
          className="mt-6"
          variant="primary"
          icon={<PlusIcon className="size-4" />}
          onClick={onCreate}
        >
          Create VM
        </Button>
      </div>
    );
  }

  const pageCount = Math.max(1, Math.ceil(filtered.length / PAGE_SIZE));
  const currentPage = Math.min(page, pageCount);
  const visible = filtered.slice((currentPage - 1) * PAGE_SIZE, currentPage * PAGE_SIZE);

  return (
    <div>
      {error !== null ? (
        <div className="border-b border-amber-200 bg-amber-50 px-4 py-3 dark:border-amber-900 dark:bg-amber-950/40">
          <div className="flex flex-wrap items-center justify-between gap-3">
            <div>
              <p className="text-sm font-medium text-amber-800 dark:text-amber-200">
                Showing the last known sandbox list
              </p>
              <p className="mt-0.5 text-xs text-amber-700 dark:text-amber-300">{error}</p>
            </div>
            <Button variant="secondary" size="xs" onClick={onRetry}>
              Retry
            </Button>
          </div>
        </div>
      ) : null}

      <div className="flex flex-col gap-3 border-b border-gray-200 px-4 py-3 sm:flex-row sm:items-center sm:justify-between dark:border-gray-800">
        <div className="flex flex-1 flex-col gap-2 sm:flex-row sm:items-center">
          <div className="relative w-full sm:max-w-xs">
            <MagnifyingGlassIcon
              className="pointer-events-none absolute top-1/2 left-3 size-4 -translate-y-1/2 text-gray-400 dark:text-gray-500"
              aria-hidden="true"
            />
            <Input
              aria-label="Filter sandboxes"
              placeholder="Search name, ID, image or IP"
              className="pl-9"
              value={query}
              onChange={(event) => {
                setQuery(event.target.value);
                setPage(1);
              }}
            />
          </div>
          <Select
            aria-label="Filter by state"
            className="w-full sm:w-44"
            value={stateFilter}
            onChange={(event) => {
              setStateFilter(event.target.value as SandboxState | "all");
              setPage(1);
            }}
          >
            <option value="all">All states</option>
            {STATE_FILTERS.map((state) => (
              <option key={state} value={state}>
                {state}
              </option>
            ))}
          </Select>
          {filtersActive ? (
            <Button
              size="xs"
              variant="ghost"
              icon={<XMarkIcon className="size-3.5" />}
              onClick={clearFilters}
            >
              Clear
            </Button>
          ) : null}
        </div>
        <p className="text-xs whitespace-nowrap text-gray-500 dark:text-gray-400">
          {filtered.length} of {sandboxes.length} VM{sandboxes.length === 1 ? "" : "s"}
        </p>
      </div>

      {filtered.length === 0 ? (
        <div className="px-6 py-16 text-center">
          <MagnifyingGlassIcon className="mx-auto size-12 text-gray-400 dark:text-gray-500" />
          <h3 className="mt-4 text-lg font-semibold text-gray-900 dark:text-white">
            No sandboxes match
          </h3>
          <p className="mx-auto mt-2 max-w-sm text-sm text-gray-500 dark:text-gray-400">
            Try a different search term or state, or clear the filters to see every VM.
          </p>
          <Button className="mt-6" variant="secondary" onClick={clearFilters}>
            Clear filters
          </Button>
        </div>
      ) : (
        <div className="relative overflow-x-auto">
          {loading ? (
            <div
              className="absolute top-3 right-4 z-10"
              aria-label="Refreshing sandboxes"
              role="status"
            >
              <Spinner className="size-4 text-indigo-600 dark:text-indigo-400" />
            </div>
          ) : null}
          <table className="min-w-full divide-y-2 divide-gray-200 dark:divide-gray-700">
            <caption className="sr-only">Virtual machines</caption>
            <thead className="ltr:text-left rtl:text-right">
              <tr>
                <th scope="col" className={cn(TH_CLASS, "w-[190px] whitespace-nowrap")}>
                  Name
                </th>
                <th scope="col" className={cn(TH_CLASS, "w-[120px] whitespace-nowrap")}>
                  State
                </th>
                <th scope="col" className={cn(TH_CLASS, "w-[170px] whitespace-nowrap")}>
                  Image
                </th>
                <th scope="col" className={cn(TH_CLASS, "w-[150px] whitespace-nowrap")}>
                  CPU
                </th>
                <th scope="col" className={cn(TH_CLASS, "w-[190px] whitespace-nowrap")}>
                  Memory
                </th>
                <th scope="col" className={cn(TH_CLASS, "w-[84px] whitespace-nowrap text-right")}>
                  CPUs
                </th>
                <th scope="col" className={cn(TH_CLASS, "w-[132px] whitespace-nowrap")}>
                  IP
                </th>
                <th scope="col" className={cn(TH_CLASS, "w-[104px] whitespace-nowrap")}>
                  Uptime
                </th>
                <th scope="col" className={cn(TH_CLASS, "w-[84px] whitespace-nowrap text-right")}>
                  Wakes
                </th>
                <th scope="col" className={cn(TH_CLASS, "w-[56px]")}>
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody className="divide-y divide-gray-200 dark:divide-gray-700">
              {visible.map((sandbox) => {
                const statsQuery = statsByRef.get(sandbox.id);
                const stats = statsQuery?.data;
                const statsPending = statsQuery === undefined || statsQuery.isPending;
                const statsFailed = statsQuery?.isError === true;
                const cpuPercent = stats === undefined ? 0 : clampPercent(stats.cpu_percent);
                const memoryLimitBytes =
                  (stats !== undefined && stats.memory_mb > 0
                    ? stats.memory_mb
                    : sandbox.memory_mb) *
                  1024 *
                  1024;
                const guestMemoryUsed = stats?.guest_memory_used_bytes;
                const guestMemoryTotal = stats?.guest_memory_total_bytes;
                const hasGuestMemory = guestMemoryUsed !== undefined && guestMemoryUsed > 0;
                const memoryPercent =
                  stats === undefined || !hasGuestMemory
                    ? 0
                    : clampPercent(
                        memoryLimitBytes > 0 ? (guestMemoryUsed / memoryLimitBytes) * 100 : 0,
                      );
                const image = imagesByDigest.get(sandbox.base_disk);
                const imageText =
                  image === undefined ? shortDigest(sandbox.base_disk, 18) : imageLabel(image);
                const volumes = sandbox.volumes ?? [];
                const dataVolumes = sandbox.data_volumes ?? [];
                const volumeCount = volumes.length + dataVolumes.length;

                return (
                  <tr
                    key={sandbox.id}
                    className="cursor-pointer hover:bg-gray-50 dark:hover:bg-gray-800/60"
                    title="Open details"
                    aria-label={`Open details for ${sandbox.name}`}
                    tabIndex={0}
                    onClick={() => {
                      onDetails(sandbox);
                    }}
                    onKeyDown={(event) => {
                      if (event.key === "Enter" || event.key === " ") {
                        event.preventDefault();
                        onDetails(sandbox);
                      }
                    }}
                  >
                    <td className={cn(TD_CLASS, "whitespace-nowrap")}>
                      <div className="flex flex-col gap-0.5">
                        <span
                          className="font-medium text-gray-900 dark:text-white"
                          title={sandbox.name}
                        >
                          {sandbox.name}
                        </span>
                        <span
                          className="text-xs text-gray-500 dark:text-gray-400"
                          title={
                            volumeCount > 0
                              ? [
                                  sandbox.id,
                                  ...volumes.map(
                                    (volume) =>
                                      `${volume.host_path} → ${volume.guest_path}${
                                        volume.read_only === true ? " (read-only)" : ""
                                      }`,
                                  ),
                                  ...dataVolumes.map(
                                    (volume) => `${volume.size_gb} GiB → ${volume.mount} (block)`,
                                  ),
                                ].join("\n")
                              : sandbox.id
                          }
                        >
                          {shortId(sandbox.id, 10)}
                          {volumeCount > 0
                            ? ` · ${volumeCount} vol${volumeCount === 1 ? "" : "s"}`
                            : ""}
                        </span>
                      </div>
                    </td>
                    <td className={cn(TD_CLASS, "whitespace-nowrap")}>
                      <VmStatusTag state={sandbox.state} />
                    </td>
                    <td className={cn(TD_CLASS, "whitespace-nowrap")}>
                      <span
                        className="inline-flex max-w-[160px] items-center truncate rounded-full bg-gray-100 px-2.5 py-0.5 font-mono text-xs text-gray-700 dark:bg-gray-800 dark:text-gray-300"
                        title={sandbox.base_disk}
                      >
                        {imageText}
                      </span>
                    </td>
                    <td className={TD_CLASS}>
                      {statsPending ? (
                        <span className="text-gray-400 dark:text-gray-500">…</span>
                      ) : statsFailed || stats === undefined ? (
                        <StatsPlaceholder title="Live stats are unavailable for this sandbox." />
                      ) : (
                        <div className="grid min-w-0 grid-cols-[minmax(48px,1fr)_auto] items-center gap-2">
                          <MiniUsageGraph percent={cpuPercent} label="CPU utilization" />
                          <span className="font-mono text-xs text-gray-500 tabular-nums dark:text-gray-400">
                            {formatPercent(cpuPercent)}
                          </span>
                        </div>
                      )}
                    </td>
                    <td className={TD_CLASS}>
                      {statsPending ? (
                        <span className="text-gray-400 dark:text-gray-500">…</span>
                      ) : statsFailed || stats === undefined ? (
                        <StatsPlaceholder title="Live stats are unavailable for this sandbox." />
                      ) : hasGuestMemory ? (
                        <div className="grid min-w-0 grid-cols-[minmax(48px,1fr)_auto] items-center gap-2">
                          <MiniUsageGraph
                            percent={memoryPercent}
                            label="Guest memory utilization"
                          />
                          <span
                            className="whitespace-nowrap font-mono text-xs text-gray-500 tabular-nums dark:text-gray-400"
                            title="Guest memory used from /proc/meminfo"
                          >
                            {`${formatBytes(guestMemoryUsed)} / ${formatBytes(
                              guestMemoryTotal ?? memoryLimitBytes,
                            )}`}
                          </span>
                        </div>
                      ) : (
                        <span
                          className="text-xs text-gray-400 dark:text-gray-500"
                          title="Guest memory stats are not available yet"
                        >
                          —
                        </span>
                      )}
                    </td>
                    <td className={cn(TD_CLASS, "text-right font-mono whitespace-nowrap")}>
                      {sandbox.vcpus}
                    </td>
                    <td className={cn(TD_CLASS, "font-mono whitespace-nowrap")}>
                      {sandbox.ip === undefined || sandbox.ip === "" ? DASH : sandbox.ip}
                    </td>
                    <td className={cn(TD_CLASS, "font-mono whitespace-nowrap")}>
                      {statsPending || statsFailed || stats === undefined ? (
                        <span className="text-gray-400 dark:text-gray-500">{DASH}</span>
                      ) : (
                        formatDuration(stats.uptime_seconds)
                      )}
                    </td>
                    <td className={cn(TD_CLASS, "text-right font-mono whitespace-nowrap")}>
                      {sandbox.wake_count}
                    </td>
                    <td className={cn(TD_CLASS, "text-right whitespace-nowrap")}>
                      <VmActionsMenu
                        sandbox={sandbox}
                        pending={pendingAction?.ref === sandbox.id ? pendingAction : null}
                        onAction={onAction}
                        onDetails={onDetails}
                        onExec={onExec}
                        onLogs={onLogs}
                      />
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}

      {filtered.length > PAGE_SIZE ? (
        <nav
          aria-label="Pagination"
          className="flex flex-wrap items-center justify-between gap-3 border-t border-gray-200 px-4 py-3 dark:border-gray-800"
        >
          <p className="text-sm text-gray-500 dark:text-gray-400">
            Showing{" "}
            <span className="font-medium text-gray-900 dark:text-white">
              {(currentPage - 1) * PAGE_SIZE + 1}–
              {Math.min(currentPage * PAGE_SIZE, filtered.length)}
            </span>{" "}
            of <span className="font-medium text-gray-900 dark:text-white">{filtered.length}</span>
          </p>
          <div className="flex items-center gap-2">
            <Button
              size="xs"
              aria-label="Previous page"
              disabled={currentPage <= 1}
              onClick={() => setPage(currentPage - 1)}
            >
              Previous
            </Button>
            <span className="text-sm text-gray-500 dark:text-gray-400">
              Page {currentPage} of {pageCount}
            </span>
            <Button
              size="xs"
              aria-label="Next page"
              disabled={currentPage >= pageCount}
              onClick={() => setPage(currentPage + 1)}
            >
              Next
            </Button>
          </div>
        </nav>
      ) : null}
    </div>
  );
}

export default VmTable;
