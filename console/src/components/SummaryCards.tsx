import { useMemo } from "react";
import type { ReactNode } from "react";
import type { Sandbox } from "../lib/types";
import { cn } from "../ui";
import { ActivityIcon, MoonIcon, ServerIcon, TrashIcon } from "./icons";

export interface SummaryCardsProps {
  sandboxes: readonly Sandbox[];
  /** When true and there is no data yet, animate the values as skeletons. */
  loading?: boolean;
}

type SummaryTone = "total" | "running" | "sleeping" | "failed";

interface SummaryDefinition {
  key: string;
  label: string;
  hint: string;
  value: number;
  icon: ReactNode;
  tone: SummaryTone;
}

const TONE_BADGE: Record<SummaryTone, string> = {
  total: "bg-indigo-100 text-indigo-700 dark:bg-indigo-900/40 dark:text-indigo-300",
  running: "bg-green-100 text-green-700 dark:bg-green-900/40 dark:text-green-300",
  sleeping: "bg-purple-100 text-purple-700 dark:bg-purple-900/40 dark:text-purple-300",
  failed: "bg-red-100 text-red-700 dark:bg-red-900/40 dark:text-red-300",
};

function isRunning(state: Sandbox["state"]): boolean {
  return state === "Running" || state === "Idle";
}

function isSleeping(state: Sandbox["state"]): boolean {
  return state === "Paused" || state === "Dormant";
}

function isFailed(state: Sandbox["state"]): boolean {
  return state === "Failed" || state === "Stopped";
}

/** Four headline counters derived from the polled sandbox list. */
export function SummaryCards({ sandboxes, loading = false }: SummaryCardsProps) {
  const summaries: SummaryDefinition[] = useMemo(() => {
    const counts = { total: sandboxes.length, running: 0, sleeping: 0, failed: 0 };
    for (const sandbox of sandboxes) {
      if (isRunning(sandbox.state)) {
        counts.running += 1;
      } else if (isSleeping(sandbox.state)) {
        counts.sleeping += 1;
      } else if (isFailed(sandbox.state)) {
        counts.failed += 1;
      }
    }
    return [
      {
        key: "total",
        label: "Total VMs",
        hint: "All sandboxes in this daemon",
        value: counts.total,
        icon: <ServerIcon />,
        tone: "total",
      },
      {
        key: "running",
        label: "Running",
        hint: "Running or idle, vCPUs live",
        value: counts.running,
        icon: <ActivityIcon />,
        tone: "running",
      },
      {
        key: "sleeping",
        label: "Sleeping",
        hint: "Paused or dormant, wake on traffic",
        value: counts.sleeping,
        icon: <MoonIcon />,
        tone: "sleeping",
      },
      {
        key: "failed",
        label: "Failed / stopped",
        hint: "Needs attention or cleanup",
        value: counts.failed,
        icon: <TrashIcon />,
        tone: "failed",
      },
    ];
  }, [sandboxes]);

  const showSkeletons = loading && sandboxes.length === 0;

  return (
    <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
      {summaries.map((summary) => (
        <article
          key={summary.key}
          data-tone={summary.tone}
          title={summary.hint}
          className="flex items-center justify-between gap-3 rounded-xl border border-[var(--app-border)] bg-[var(--app-panel)] px-4 py-3"
        >
          {showSkeletons ? (
            <div className="flex flex-col gap-1.5" aria-hidden="true">
              <span className="h-3.5 w-20 animate-pulse rounded bg-gray-200 dark:bg-gray-800" />
              <span className="h-6 w-10 animate-pulse rounded bg-gray-200 dark:bg-gray-800" />
            </div>
          ) : (
            <div className="min-w-0">
              <p className="truncate text-[11px] font-medium text-[var(--app-muted)]">
                {summary.label}
              </p>
              <p
                data-testid={`summary-value-${summary.key}`}
                className="mt-0.5 text-xl font-semibold text-[var(--app-text)] tabular-nums"
              >
                {summary.value}
              </p>
            </div>
          )}
          <span
            className={cn(
              "grid size-8 shrink-0 place-items-center rounded-md border border-[var(--app-border)] bg-[var(--app-panel-2)] text-[var(--app-muted)]",
              TONE_BADGE[summary.tone],
            )}
            aria-hidden="true"
          >
            <span className="size-4">{summary.icon}</span>
          </span>
        </article>
      ))}
    </div>
  );
}

export default SummaryCards;
