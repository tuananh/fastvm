import { Dialog, DialogBackdrop, DialogPanel, DialogTitle } from "@headlessui/react";
import { XMarkIcon } from "@heroicons/react/24/outline";
import { useEffect, useRef, useState } from "react";
import type { FitAddon as GhosttyFitAddon, Terminal as GhosttyTerminal } from "ghostty-web";
import { connectShell } from "../lib/shell";
import type { ShellHandle } from "../lib/shell";
import type { Sandbox } from "../lib/types";
import { Spinner } from "../ui";
import { monoFontFamily, terminalTheme } from "../theme";
import { useTheme } from "../theme/theme-mode";
import { describeError } from "../utils/errors";
import { shortId } from "../utils/format";
import { RefreshIcon, TerminalIcon } from "./icons";

type GhosttyModule = typeof import("ghostty-web");
type GhosttyImports = Pick<GhosttyModule, "Terminal" | "FitAddon">;

/** Only one WASM `init()` per page, shared by every terminal dialog. */
let ghosttyImportsPromise: Promise<GhosttyImports> | null = null;

async function loadGhostty(): Promise<GhosttyImports> {
  if (ghosttyImportsPromise === null) {
    ghosttyImportsPromise = (async () => {
      // Dynamic import keeps the ~400 KiB ghostty WASM bundle out of the initial
      // console chunk; it is fetched the first time a terminal is opened.
      const module = await import("ghostty-web");
      await module.init();
      return { Terminal: module.Terminal, FitAddon: module.FitAddon };
    })().catch((error: unknown) => {
      ghosttyImportsPromise = null;
      throw error;
    });
  }
  return ghosttyImportsPromise;
}

type TerminalStatusKind = "connecting" | "connected" | "exited" | "error";

interface TerminalStatus {
  /** Session generation this status belongs to; stale statuses are ignored. */
  readonly session: number;
  readonly kind: TerminalStatusKind;
  readonly exitCode?: number | null;
  readonly message?: string;
}

const INITIAL_STATUS: TerminalStatus = { session: 0, kind: "connecting" };

function exitLabel(exitCode: number | null | undefined): string {
  if (exitCode === null || exitCode === undefined) {
    return "Session ended";
  }
  return `Session ended (exit ${exitCode})`;
}

function TerminalStatusBadge({ status }: { status: TerminalStatus }) {
  const base =
    "inline-flex w-fit items-center gap-1.5 rounded-full border px-2.5 py-0.5 text-xs font-medium whitespace-nowrap";
  switch (status.kind) {
    case "connecting":
      return (
        <span
          className={`${base} border-gray-200 bg-gray-50 text-gray-600 dark:border-gray-700 dark:bg-gray-800 dark:text-gray-300`}
        >
          <Spinner className="size-3" /> Connecting…
        </span>
      );
    case "connected":
      return (
        <span
          className={`${base} border-green-200 bg-green-50 text-green-700 dark:border-green-900 dark:bg-green-950/40 dark:text-green-300`}
        >
          Connected
        </span>
      );
    case "exited":
      return (
        <span
          className={`${base} border-gray-200 bg-gray-50 text-gray-600 dark:border-gray-700 dark:bg-gray-800 dark:text-gray-300`}
        >
          {exitLabel(status.exitCode)}
        </span>
      );
    case "error":
      return (
        <span
          className={`${base} border-red-200 bg-red-50 text-red-700 dark:border-red-900 dark:bg-red-950/40 dark:text-red-300`}
          title={status.message}
        >
          Connection error
        </span>
      );
  }
}

export interface TerminalDialogProps {
  sandbox: Sandbox | null;
  open: boolean;
  onClose: () => void;
}

/**
 * Interactive shell over the fastvmd exec WebSocket. A fresh ghostty terminal +
 * WebSocket session is created every time the dialog opens or Reconnect is
 * pressed; closing/disposing always tears both down.
 */
export function TerminalDialog({ sandbox, open, onClose }: TerminalDialogProps) {
  const { resolvedMode } = useTheme();
  // The dialog body can mount after the effect runs, so the terminal container
  // arrives through a callback-ref state instead of a ref.
  const [container, setContainer] = useState<HTMLDivElement | null>(null);
  const terminalRef = useRef<GhosttyTerminal | null>(null);
  const [session, setSession] = useState(0);
  const [status, setStatus] = useState<TerminalStatus>(INITIAL_STATUS);

  const sandboxRef = sandbox?.id;

  useEffect(() => {
    if (!open || sandboxRef === undefined || container === null) {
      return undefined;
    }
    const sessionId = session;
    let disposed = false;
    let handle: ShellHandle | null = null;
    let terminal: GhosttyTerminal | null = null;
    let fitAddon: GhosttyFitAddon | null = null;
    const disposables: Array<{ dispose(): void }> = [];

    const setSessionStatus = (next: Omit<TerminalStatus, "session">) => {
      if (!disposed) {
        setStatus({ session: sessionId, ...next });
      }
    };

    const start = async () => {
      try {
        const { Terminal: TerminalCtor, FitAddon: FitCtor } = await loadGhostty();
        // The dialog may have been closed while the WASM bundle was loading.
        if (disposed) {
          return;
        }
        // Ghostty measures the font when the terminal opens. Wait for the web
        // font so the first render does not fall back to a different monospace.
        try {
          await document.fonts?.load(`14px ${JSON.stringify("JetBrains Mono")}`);
        } catch {
          // Font loading is best-effort; ghostty will use the fallback stack.
        }
        if (disposed) {
          return;
        }
        container.replaceChildren();
        terminal = new TerminalCtor({
          fontSize: 14,
          fontFamily: monoFontFamily,
          cursorBlink: true,
          scrollback: 10_000,
          theme: terminalTheme(resolvedMode),
        });
        terminalRef.current = terminal;
        fitAddon = new FitCtor();
        terminal.loadAddon(fitAddon);
        terminal.open(container);
        fitAddon.fit();
        fitAddon.observeResize();
        terminal.focus();

        // ghostty-web 0.4.0 can carry WASM screen state over from a previously
        // disposed Terminal, so a fresh session may inherit the old history.
        // Clear the screen and scrollback (ED 2 + ED 3 + cursor home) before the
        // new shell writes its prompt; the write queue preserves this ordering.
        terminal.write("\x1b[2J\x1b[3J\x1b[H");

        disposables.push(
          terminal.onData((data) => {
            handle?.sendInput(data);
          }),
          terminal.onResize(({ cols, rows }) => {
            handle?.resize(cols, rows);
          }),
        );

        const cols = terminal.cols > 0 ? terminal.cols : 80;
        const rows = terminal.rows > 0 ? terminal.rows : 24;
        handle = connectShell(
          sandboxRef,
          { cols, rows },
          {
            onOpen: () => {
              setSessionStatus({ kind: "connected" });
            },
            onOutput: (data) => {
              terminal?.write(data);
            },
            onExit: (code) => {
              setSessionStatus({ kind: "exited", exitCode: code });
            },
            onError: (message) => {
              setSessionStatus({ kind: "error", message });
            },
          },
        );
      } catch (error) {
        setSessionStatus({ kind: "error", message: describeError(error) });
      }
    };

    void start();

    return () => {
      disposed = true;
      for (const disposable of disposables) {
        try {
          disposable.dispose();
        } catch {
          // Terminal is already disposed; nothing left to unregister.
        }
      }
      try {
        fitAddon?.dispose();
      } catch {
        // Best effort during teardown.
      }
      try {
        terminal?.dispose();
      } catch {
        // Best effort during teardown.
      }
      terminalRef.current = null;
      try {
        handle?.close();
      } catch {
        // Socket already closed.
      }
      container.replaceChildren();
    };
  }, [open, sandboxRef, session, container, resolvedMode]);

  const effectiveStatus: TerminalStatus = status.session === session ? status : INITIAL_STATUS;
  const connecting = effectiveStatus.kind === "connecting";
  const terminalBackground = terminalTheme(resolvedMode).background;

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
                <TerminalIcon className="size-4" />
              </span>
              <DialogTitle className="truncate text-sm font-medium text-gray-900 dark:text-gray-100">
                {sandbox?.name ?? "Terminal"}
              </DialogTitle>
              {sandbox !== null ? (
                <span className="hidden font-mono text-[11px] text-gray-500 sm:inline">
                  {shortId(sandbox.id, 12)}
                </span>
              ) : null}
            </div>
            <div className="flex shrink-0 items-center gap-2">
              <TerminalStatusBadge status={effectiveStatus} />
              <button
                type="button"
                onClick={onClose}
                aria-label="Close terminal"
                className="rounded-md p-1.5 text-gray-500 transition-colors hover:bg-gray-200 hover:text-gray-900 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-indigo-500 dark:text-gray-400 dark:hover:bg-gray-800 dark:hover:text-gray-100"
              >
                <XMarkIcon className="size-4" />
              </button>
            </div>
          </div>

          <div
            className="flex min-h-0 flex-1 flex-col gap-2 p-3"
            style={{ backgroundColor: terminalBackground }}
            onKeyDown={(event) => {
              // Keep Escape inside the shell instead of letting Headless UI
              // close the dialog (ghostty turns it into an ESC byte for the PTY).
              if (event.key === "Escape") {
                event.stopPropagation();
              }
            }}
          >
            {effectiveStatus.kind === "error" ? (
              <div
                className="rounded-lg border border-red-200 bg-red-50 px-3 py-2 text-sm text-red-700 dark:border-red-900 dark:bg-red-950/40 dark:text-red-300"
                role="alert"
              >
                {effectiveStatus.message ?? "Could not open a shell session."}
              </div>
            ) : null}
            {effectiveStatus.kind === "exited" ? (
              <div
                className="rounded-lg border border-gray-200 bg-gray-50 px-3 py-2 text-sm text-gray-700 dark:border-gray-700 dark:bg-gray-800/60 dark:text-gray-300"
                role="status"
              >
                The shell exited; the output above is preserved. Reconnect to start a new session.
              </div>
            ) : null}
            <div
              className="terminal-container min-h-0 flex-1"
              ref={setContainer}
              data-testid="terminal-container"
            />
          </div>

          <div className="flex shrink-0 flex-wrap items-center justify-between gap-3 border-t border-gray-200 bg-gray-50 px-4 py-2.5 dark:border-gray-800 dark:bg-[#111827]">
            <span className="inline-flex items-center gap-1.5 text-xs text-gray-500 dark:text-gray-400">
              <TerminalIcon className="size-3.5" /> Ctrl+C sends interrupt · closing ends the
              session
            </span>
            <span className="inline-flex items-center gap-2">
              <button
                type="button"
                onClick={() => {
                  setSession((value) => value + 1);
                }}
                disabled={connecting}
                className="inline-flex h-8 items-center gap-1.5 rounded-md border border-gray-300 bg-white px-3 text-xs font-medium text-gray-700 transition-colors hover:bg-gray-100 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-indigo-500 disabled:pointer-events-none disabled:opacity-50 dark:border-gray-700 dark:bg-gray-800 dark:text-gray-200 dark:hover:bg-gray-700"
              >
                <RefreshIcon className="size-3.5" /> Reconnect
              </button>
              <button
                type="button"
                onClick={onClose}
                className="inline-flex h-8 items-center rounded-md border border-gray-300 bg-white px-3 text-xs font-medium text-gray-700 transition-colors hover:bg-gray-100 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-indigo-500 dark:border-gray-700 dark:bg-gray-800 dark:text-gray-200 dark:hover:bg-gray-700"
              >
                Close
              </button>
            </span>
          </div>
        </DialogPanel>
      </div>
    </Dialog>
  );
}

export default TerminalDialog;
