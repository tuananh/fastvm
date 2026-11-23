import type { ReactNode } from "react";
import { cn } from "../ui";
import { clampPercent, formatPercent } from "../utils/format";
import { usageColor, useUsageHistory } from "./usageHistory";

export interface UtilizationBarProps {
  /** Metric name, e.g. "CPU" or "Memory". */
  label: string;
  /** Raw percentage; clamped into 0–100 before rendering. */
  percent: number;
  /** Optional caption rendered under the bar (e.g. "312 MiB / 512 MiB"). */
  detail?: ReactNode;
  /** Explicit fill color; defaults to an indigo/amber/red threshold ramp. */
  color?: string;
  /** Render a small bar chart of recent samples instead of a horizontal bar. */
  showHistory?: boolean;
  className?: string;
}

/**
 * Labeled utilization bar. Keeps the numeric value visible next to the bar so
 * the metric is readable even when the bar is too small to judge.
 */
export function UtilizationBar({
  label,
  percent,
  detail,
  color,
  showHistory = false,
  className,
}: UtilizationBarProps) {
  const clamped = clampPercent(percent);
  const fill = color ?? usageColor(clamped);
  const history = useUsageHistory(clamped);

  return (
    <div className={cn("min-w-0", className)} data-percent={clamped}>
      <div className="flex items-baseline justify-between gap-3">
        <span className="text-sm font-medium text-gray-900 dark:text-white">{label}</span>
        <span className="text-sm font-medium text-gray-900 tabular-nums dark:text-white">
          {formatPercent(clamped)}
        </span>
      </div>
      {showHistory && history.length === 0 ? (
        <div className="mt-2 flex h-6 items-center rounded border border-dashed border-gray-300 px-2 text-xs text-gray-400 dark:border-gray-700 dark:text-gray-500">
          Waiting for samples…
        </div>
      ) : showHistory ? (
        <div
          role="progressbar"
          aria-label={label}
          aria-valuemin={0}
          aria-valuemax={100}
          aria-valuenow={clamped}
          data-testid="usage-history"
          className="mt-2 flex h-10 w-full items-end gap-0.5 overflow-hidden rounded bg-gray-50 px-1 py-1 dark:bg-gray-800/60"
        >
          {history.map((sample, index) => (
            <span
              key={index}
              aria-hidden="true"
              className="min-w-px flex-1 rounded-sm transition-[height] duration-300"
              style={{
                height: `${Math.max(4, clampPercent(sample))}%`,
                backgroundColor: fill,
                opacity: 0.35 + (0.65 * (index + 1)) / history.length,
              }}
            />
          ))}
        </div>
      ) : (
        <div
          role="progressbar"
          aria-label={label}
          aria-valuemin={0}
          aria-valuemax={100}
          aria-valuenow={clamped}
          className="mt-2 h-2 w-full overflow-hidden rounded-full bg-gray-200 dark:bg-gray-700"
        >
          <div
            className="h-full rounded-full transition-[width] duration-300"
            style={{ width: `${clamped}%`, backgroundColor: fill }}
          />
        </div>
      )}
      {detail !== undefined ? (
        <div className="mt-1.5 text-xs text-gray-500 tabular-nums dark:text-gray-400">{detail}</div>
      ) : null}
    </div>
  );
}

export default UtilizationBar;
