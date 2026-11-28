import {
  ArrowTopRightOnSquareIcon,
  BookOpenIcon,
  CircleStackIcon,
  MagnifyingGlassIcon,
  PlusIcon,
  Squares2X2Icon,
  XMarkIcon,
} from "@heroicons/react/24/outline";
import { useMemo, useState } from "react";
import type { Sandbox } from "../lib/types";
import { cn } from "../ui";
import { BoltIcon } from "./icons";
import { ThemeMenu } from "./ThemeMenu";
import { UserMenu } from "./UserMenu";

export type ConsoleView = "sandboxes" | "images" | "sandbox";

export interface AppSidebarProps {
  open: boolean;
  view: ConsoleView;
  sandboxes: readonly Sandbox[];
  selectedSandboxId: string | null;
  onNavigate: (view: ConsoleView) => void;
  onSelectSandbox: (sandbox: Sandbox) => void;
  onNewVm: () => void;
  onClose: () => void;
}

const DOCS_URL = "https://github.com/tuananh/fastvm/tree/main/docs";
const MAX_AGENTS = 5;

const NAV_LINK =
  "flex w-full items-center gap-3 rounded-lg px-3 py-2 text-[13px] font-medium text-[var(--app-muted)] transition-colors hover:bg-[var(--app-selected)] hover:text-[var(--app-text)]";
const NAV_LINK_ACTIVE =
  "flex w-full items-center gap-3 rounded-lg bg-[var(--app-selected)] px-3 py-2 text-[13px] font-medium text-[var(--app-text)]";

const AGENT_COLORS = [
  "var(--om-blue)",
  "var(--om-cyan)",
  "var(--om-orange)",
  "var(--om-magenta)",
  "var(--om-green)",
];

function sandboxIconColor(id: string): string {
  let hash = 0;
  for (let i = 0; i < id.length; i += 1) {
    hash = (hash * 31 + id.charCodeAt(i)) >>> 0;
  }
  return AGENT_COLORS[hash % AGENT_COLORS.length] ?? AGENT_COLORS[0]!;
}

function stateDot(state: Sandbox["state"]): string {
  if (state === "Running") return "bg-[var(--om-green)]";
  if (state === "Idle") return "bg-[var(--om-yellow)]";
  if (state === "Paused" || state === "Dormant") return "bg-[var(--om-magenta)]";
  if (state === "Failed") return "bg-[var(--om-red)]";
  return "bg-[var(--app-faint)]";
}

function SandboxRow({
  sandbox,
  selected,
  onSelect,
}: {
  sandbox: Sandbox;
  selected: boolean;
  onSelect: () => void;
}) {
  const color = sandboxIconColor(sandbox.id);
  return (
    <button
      type="button"
      onClick={onSelect}
      aria-current={selected ? "page" : undefined}
      className={cn(
        "group flex w-full items-center gap-3 rounded-lg px-2.5 py-2 text-left transition-colors",
        selected
          ? "bg-[var(--app-selected)] text-[var(--app-text)]"
          : "text-[var(--app-muted)] hover:bg-[var(--app-selected)] hover:text-[var(--app-text)]",
      )}
    >
      <span
        className="grid size-7 shrink-0 place-items-center rounded-md text-[12px] font-semibold text-white"
        style={{ backgroundColor: color }}
        aria-hidden="true"
      >
        {sandbox.name.slice(0, 1).toUpperCase()}
      </span>
      <span className="min-w-0 flex-1 truncate text-[13px] font-medium">{sandbox.name}</span>
      {selected ? (
        <span className="rounded border border-[var(--app-border)] px-1 py-0.5 text-[10px] text-[var(--app-muted)]">
          here
        </span>
      ) : (
        <span
          className={cn("size-1.5 shrink-0 rounded-full", stateDot(sandbox.state))}
          aria-hidden="true"
        />
      )}
    </button>
  );
}

function SectionTitle({ children }: { children: string }) {
  return (
    <p className="px-2.5 pb-1.5 pt-4 text-[10px] font-semibold uppercase tracking-[0.14em] text-[var(--app-faint)]">
      {children}
    </p>
  );
}

/**
 * Workspace sidebar: compact brand header, primary navigation, sandbox switcher
 * and recent conversation-style lists, matching the term-llm shell.
 */
export function AppSidebar({
  open,
  view,
  sandboxes,
  selectedSandboxId,
  onNavigate,
  onSelectSandbox,
  onNewVm,
  onClose,
}: AppSidebarProps) {
  const [filter, setFilter] = useState("");
  const [todayKey] = useState(() => new Date().toDateString());
  const filtered = useMemo(() => {
    const needle = filter.trim().toLowerCase();
    if (needle === "") return sandboxes;
    return sandboxes.filter(
      (sandbox) =>
        sandbox.name.toLowerCase().includes(needle) ||
        sandbox.id.toLowerCase().includes(needle) ||
        sandbox.state.toLowerCase().includes(needle),
    );
  }, [filter, sandboxes]);

  const agents = filtered.slice(0, MAX_AGENTS);
  const rest = filtered.slice(MAX_AGENTS);
  const today = rest.filter((sandbox) => new Date(sandbox.created_at).toDateString() === todayKey);
  const older = rest.filter((sandbox) => !today.includes(sandbox));

  return (
    <>
      <div
        role="presentation"
        onClick={onClose}
        className={cn("fixed inset-0 z-30 bg-black/60 lg:hidden", open ? "block" : "hidden")}
      />

      <aside
        id="app-sidebar"
        className={cn(
          "fixed inset-y-0 left-0 z-40 flex w-[280px] flex-col border-r border-[var(--app-border)] bg-[var(--app-sidebar)] text-[var(--app-text)] transition-transform duration-300 lg:static lg:shrink-0 lg:translate-x-0",
          open ? "translate-x-0 max-lg:visible" : "-translate-x-full max-lg:invisible",
        )}
      >
        <div className="flex items-center justify-between gap-2 px-4 pt-4">
          <button
            type="button"
            onClick={() => onNavigate("sandboxes")}
            className="flex min-w-0 items-center gap-2 text-left"
          >
            <span className="grid size-7 shrink-0 place-items-center rounded-md bg-[var(--app-accent)] text-white">
              <BoltIcon className="size-4" />
            </span>
            <span className="truncate text-[14px] font-semibold">fastvm</span>
          </button>
          <div className="flex items-center gap-1">
            <button
              type="button"
              onClick={onClose}
              aria-label="Close sidebar"
              className="rounded-md p-1.5 text-[var(--app-muted)] hover:bg-[var(--app-selected)] hover:text-[var(--app-text)] lg:hidden"
            >
              <XMarkIcon className="size-4" />
            </button>
            <span className="hidden size-7 place-items-center rounded-md border border-[var(--app-border)] text-[var(--app-muted)] lg:grid">
              <Squares2X2Icon className="size-3.5" />
            </span>
          </div>
        </div>

        <div className="px-3 pt-4">
          <button
            type="button"
            onClick={onNewVm}
            className="flex h-10 w-full items-center gap-2 rounded-lg border border-[var(--app-border)] bg-[var(--app-panel)] px-3 text-[13px] font-medium text-[var(--app-text)] transition-colors hover:border-[var(--app-accent)] hover:bg-[var(--app-panel-2)]"
          >
            <PlusIcon className="size-4" />
            New VM
          </button>
        </div>

        <nav aria-label="Dashboard" className="px-3 pt-3">
          <ul className="space-y-0.5">
            <li>
              <a
                href="#sandboxes"
                onClick={(event) => {
                  event.preventDefault();
                  onNavigate("sandboxes");
                  onClose();
                }}
                aria-current={view === "sandboxes" ? "page" : undefined}
                className={view === "sandboxes" ? NAV_LINK_ACTIVE : NAV_LINK}
              >
                <Squares2X2Icon className="size-4 shrink-0" aria-hidden="true" />
                Overview
                <span
                  aria-hidden="true"
                  className="ml-auto rounded-full border border-[var(--app-border)] px-1.5 py-0.5 text-[10px] text-[var(--app-muted)]"
                >
                  {sandboxes.length}
                </span>
              </a>
            </li>
            <li>
              <a
                href="#images"
                onClick={(event) => {
                  event.preventDefault();
                  onNavigate("images");
                  onClose();
                }}
                aria-current={view === "images" ? "page" : undefined}
                className={view === "images" ? NAV_LINK_ACTIVE : NAV_LINK}
              >
                <CircleStackIcon className="size-4 shrink-0" aria-hidden="true" />
                Images
              </a>
            </li>
            <li>
              <a
                href={DOCS_URL}
                target="_blank"
                rel="noreferrer"
                onClick={onClose}
                className={NAV_LINK}
              >
                <BookOpenIcon className="size-4 shrink-0" aria-hidden="true" />
                Documentation
                <ArrowTopRightOnSquareIcon className="ml-auto size-3.5 shrink-0 opacity-60" />
              </a>
            </li>
          </ul>
        </nav>

        <div className="px-3 pt-3">
          <label className="flex h-9 items-center gap-2 rounded-lg border border-[var(--app-border)] bg-[var(--app-panel)] px-2.5">
            <MagnifyingGlassIcon className="size-3.5 text-[var(--app-faint)]" />
            <input
              value={filter}
              onChange={(event) => setFilter(event.currentTarget.value)}
              placeholder="Search sandboxes"
              aria-label="Search sandboxes"
              className="min-w-0 flex-1 bg-transparent text-[12px] text-[var(--app-text)] outline-none placeholder:text-[var(--app-faint)]"
            />
            <kbd className="rounded border border-[var(--app-border)] px-1 text-[9px] text-[var(--app-faint)]">
              ⌘K
            </kbd>
          </label>
        </div>

        <div className="min-h-0 flex-1 overflow-y-auto px-1.5 pb-3">
          <SectionTitle>Sandboxes</SectionTitle>
          {agents.length === 0 ? (
            <p className="px-2.5 py-2 text-[12px] text-[var(--app-faint)]">
              {filter === "" ? "No sandboxes yet" : "No matches"}
            </p>
          ) : (
            <ul className="space-y-0.5">
              {agents.map((sandbox) => (
                <li key={sandbox.id}>
                  <SandboxRow
                    sandbox={sandbox}
                    selected={sandbox.id === selectedSandboxId}
                    onSelect={() => {
                      onSelectSandbox(sandbox);
                      onClose();
                    }}
                  />
                </li>
              ))}
            </ul>
          )}

          {today.length > 0 ? (
            <>
              <SectionTitle>Today</SectionTitle>
              <ul className="space-y-0.5">
                {today.map((sandbox) => (
                  <li key={sandbox.id}>
                    <SandboxRow
                      sandbox={sandbox}
                      selected={sandbox.id === selectedSandboxId}
                      onSelect={() => {
                        onSelectSandbox(sandbox);
                        onClose();
                      }}
                    />
                  </li>
                ))}
              </ul>
            </>
          ) : null}

          {older.length > 0 ? (
            <>
              <SectionTitle>Older</SectionTitle>
              <ul className="space-y-0.5">
                {older.map((sandbox) => (
                  <li key={sandbox.id}>
                    <SandboxRow
                      sandbox={sandbox}
                      selected={sandbox.id === selectedSandboxId}
                      onSelect={() => {
                        onSelectSandbox(sandbox);
                        onClose();
                      }}
                    />
                  </li>
                ))}
              </ul>
            </>
          ) : null}
        </div>

        <div className="border-t border-[var(--app-border)]">
          <div className="flex items-center gap-1 px-2.5 py-2">
            <div className="min-w-0 flex-1">
              <UserMenu />
            </div>
            <ThemeMenu />
          </div>
        </div>
      </aside>
    </>
  );
}

export default AppSidebar;
