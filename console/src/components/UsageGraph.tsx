import { cn } from "../ui";
import { clampPercent } from "../utils/format";
import { usageColor, useUsageHistory } from "./usageHistory";

export interface MiniUsageGraphProps {
  /** Raw percentage; clamped before rendering. */
  percent: number;
  /** Accessible metric name. */
  label: string;
  className?: string;
}

/** Compact vertical bar history used in the sandbox table. */
export function MiniUsageGraph({ percent, label, className }: MiniUsageGraphProps) {
  const clamped = clampPercent(percent);
  const history = useUsageHistory(clamped);
  const color = usageColor(clamped);

  return (
    <div
      role="progressbar"
      aria-label={label}
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={clamped}
      title={`${label}: ${clamped.toFixed(1)}%`}
      className={cn(
        "flex h-6 w-full min-w-12 items-end gap-px overflow-hidden rounded bg-gray-50 px-0.5 py-0.5 dark:bg-gray-800/60",
        className,
      )}
    >
      {history.map((sample, index) => (
        <span
          key={index}
          aria-hidden="true"
          className="min-w-px flex-1 rounded-sm transition-[height] duration-300"
          style={{
            height: `${Math.max(4, sample)}%`,
            backgroundColor: color,
            opacity: 0.35 + (0.65 * (index + 1)) / history.length,
          }}
        />
      ))}
    </div>
  );
}

export default MiniUsageGraph;
