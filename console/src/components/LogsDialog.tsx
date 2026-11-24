import { Dialog, DialogBackdrop, DialogPanel, DialogTitle } from "@headlessui/react";
import { XMarkIcon } from "@heroicons/react/24/outline";
import { useState } from "react";
import type { SandboxLogSource } from "../lib/api";
import { useSandboxLogs } from "../lib/queries";
import type { Sandbox } from "../lib/types";
import { monoFontFamily, terminalTheme } from "../theme";
import { useTheme } from "../theme/theme-mode";
import { describeError } from "../utils/errors";
import { shortId } from "../utils/format";
import { ListBulletIcon, RefreshIcon } from "./icons";

const SOURCES: { id: SandboxLogSource; label: string }[] = [
  { id: "app", label: "Application" },
  { id: "console", label: "Console" },
  { id: "system", label: "System" },
];

export interface LogsDialogProps {
  sandbox: Sandbox | null;
  open: boolean;
  onClose: () => void;
}

/** Read-only terminal-styled log viewer for a sandbox. */
export function LogsDialog({ sandbox, open, onClose }: LogsDialogProps) {
  const { resolvedMode } = useTheme();
  const [source, setSource] = useState<SandboxLogSource>("app");
  const logs = useSandboxLogs(sandbox?.id, source, open && sandbox !== null);
  const terminal = terminalTheme(resolvedMode);

  return (
    <Dialog open={open} onClose={onClose} className="relative z-50">
      <DialogBackdrop className="fixed inset-0 bg-gray-950/80 backdrop-blur-sm" />
      <div className="fixed inset-0 flex items-end justify-center p-4 sm:items-center">
        <DialogPanel className="flex h-[min(80vh,760px)] w-full max-w-[1100px] flex-col overflow-hidden rounded-xl border border-gray-200 bg-[#0b0e14] shadow-2xl dark:border-gray-800">
          <div className="flex shrink-0 items-center justify-between gap-3 border-b border-gray-200 bg-gray-50 px-4 py-2.5 dark:border-gray-800 dark:bg-[#111827]">
            <div className="flex min-w-0 items-center gap-3">
              <span className="flex shrink-0 items-center gap-1.5" aria-hidden="true">
                <span className="size-2.5 rounded-full bg-[#ff5f57]" />
                <span className="size-2.5 rounded-full bg-[#febc2e]" />
                <span className="size-2.5 rounded-full bg-[#28c840]" />
              </span>
              <span className="shrink-0 text-gray-500" aria-hidden="true">
                <ListBulletIcon className="size-4" />
              </span>
              <DialogTitle className="truncate text-sm font-medium text-gray-900 dark:text-gray-100">
                {sandbox === null ? "Logs" : `${sandbox.name} logs`}
              </DialogTitle>
              {sandbox !== null ? (
                <span className="hidden font-mono text-[11px] text-gray-500 sm:inline">
                  {shortId(sandbox.id, 12)}
                </span>
              ) : null}
            </div>
            <button
              type="button"
              onClick={onClose}
              aria-label="Close logs"
              className="rounded-md p-1.5 text-gray-500 transition-colors hover:bg-gray-200 hover:text-gray-900 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-indigo-500 dark:text-gray-400 dark:hover:bg-gray-800 dark:hover:text-gray-100"
            >
              <XMarkIcon className="size-4" />
            </button>
          </div>

          <div
            className="flex min-h-0 flex-1 flex-col gap-2 p-3"
            style={{ backgroundColor: terminal.background }}
          >
            <div className="flex flex-wrap items-center justify-between gap-2">
              <div
                role="tablist"
                aria-label="Log source"
                className="flex rounded-md border border-gray-200 bg-gray-100 p-0.5 dark:border-white/10 dark:bg-white/5"
              >
                {SOURCES.map((entry) => (
                  <button
                    key={entry.id}
                    type="button"
                    role="tab"
                    aria-selected={source === entry.id}
                    onClick={() => setSource(entry.id)}
                    className={
                      source === entry.id
                        ? "rounded bg-white px-2.5 py-1 text-xs font-medium text-gray-900 shadow-sm dark:bg-white/15 dark:text-white dark:shadow-none"
                        : "rounded px-2.5 py-1 text-xs font-medium text-gray-600 hover:bg-gray-200 hover:text-gray-900 dark:text-gray-400 dark:hover:bg-white/5 dark:hover:text-gray-200"
                    }
                  >
                    {entry.label}
                  </button>
                ))}
              </div>
              <button
                type="button"
                onClick={() => void logs.refetch()}
                disabled={logs.isFetching}
                className="inline-flex h-7 items-center gap-1.5 rounded-md border border-gray-300 bg-white px-2.5 text-xs font-medium text-gray-700 transition-colors hover:bg-gray-100 disabled:pointer-events-none disabled:opacity-50 dark:border-white/10 dark:bg-white/5 dark:text-gray-300 dark:hover:bg-white/10"
              >
                <RefreshIcon className="size-3.5" /> Refresh
              </button>
            </div>

            {logs.isError ? (
              <div
                className="rounded-lg border border-red-200 bg-red-50 px-3 py-2 text-sm text-red-700 dark:border-red-900 dark:bg-red-950/40 dark:text-red-300"
                role="alert"
              >
                {describeError(logs.error)}
              </div>
            ) : null}

            <div
              className="min-h-0 flex-1 overflow-auto rounded-md border border-gray-200 p-3 dark:border-white/10"
              style={{ backgroundColor: terminal.background }}
            >
              {logs.isPending ? (
                <span className="text-xs text-gray-500">Loading logs…</span>
              ) : logs.data === "" ? (
                <span className="text-xs text-gray-500">No logs captured yet.</span>
              ) : (
                <pre
                  className="w-max min-w-full text-xs leading-5 whitespace-pre"
                  style={{ fontFamily: monoFontFamily, color: terminal.foreground }}
                >
                  {logs.data}
                </pre>
              )}
            </div>
          </div>

          <div className="flex shrink-0 flex-wrap items-center justify-between gap-3 border-t border-gray-200 bg-gray-50 px-4 py-2.5 dark:border-gray-800 dark:bg-[#111827]">
            <span className="inline-flex items-center gap-1.5 text-xs text-gray-500 dark:text-gray-400">
              <ListBulletIcon className="size-3.5" /> PTY sessions are not logged to avoid capturing
              typed input
            </span>
            <button
              type="button"
              onClick={onClose}
              className="inline-flex h-8 items-center rounded-md border border-gray-300 bg-white px-3 text-xs font-medium text-gray-700 transition-colors hover:bg-gray-100 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-indigo-500 dark:border-gray-700 dark:bg-gray-800 dark:text-gray-200 dark:hover:bg-gray-700"
            >
              Close
            </button>
          </div>
        </DialogPanel>
      </div>
    </Dialog>
  );
}

export default LogsDialog;
