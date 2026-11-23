import { useEffect, useMemo, useSyncExternalStore } from "react";
import { clampPercent } from "../utils/format";

const HISTORY_SAMPLES = 30;

interface HistoryStore {
  push(value: number): void;
  subscribe(listener: () => void): () => void;
  getSnapshot(): readonly number[];
}

function createHistoryStore(): HistoryStore {
  let values: readonly number[] = [];
  const listeners = new Set<() => void>();
  return {
    push(value) {
      if (values[values.length - 1] === value) {
        return;
      }
      values = [...values, value].slice(-HISTORY_SAMPLES);
      for (const listener of listeners) {
        listener();
      }
    },
    subscribe(listener) {
      listeners.add(listener);
      return () => {
        listeners.delete(listener);
      };
    },
    getSnapshot() {
      return values;
    },
  };
}

/** Keep the last 30 distinct samples for a utilization graph. */
export function useUsageHistory(value: number): readonly number[] {
  const store = useMemo(() => createHistoryStore(), []);
  useEffect(() => {
    store.push(clampPercent(value));
  }, [store, value]);
  return useSyncExternalStore(
    (listener) => store.subscribe(listener),
    () => store.getSnapshot(),
    () => store.getSnapshot(),
  );
}

/** Shared indigo -> amber -> red utilization ramp. */
export function usageColor(percent: number): string {
  if (percent >= 85) {
    return "#dc2626";
  }
  if (percent >= 60) {
    return "#f59e0b";
  }
  return "#4f46e5";
}
