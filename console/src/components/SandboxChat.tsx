import {
  ArrowPathIcon,
  CheckIcon,
  ChevronDownIcon,
  ClipboardDocumentIcon,
  PlusIcon,
  PaperAirplaneIcon,
  WrenchScrewdriverIcon,
} from "@heroicons/react/24/outline";
import { useEffect, useRef, useState } from "react";
import { execSandbox } from "../lib/api";
import { useSandboxStats } from "../lib/queries";
import type { BaseDisk, Sandbox } from "../lib/types";
import { cn } from "../ui";
import { describeError } from "../utils/errors";
import { formatBytes, formatPercent, imageLabel, shortDigest, shortId } from "../utils/format";
import { ActivityIcon, ClockIcon, HardDriveIcon, NetworkIcon, ServerIcon } from "./icons";

interface CommandResult {
  command: string;
  stdout?: string;
  stderr?: string;
  exitCode?: number;
  error?: string;
}

export interface SandboxChatProps {
  sandbox: Sandbox;
  images: readonly BaseDisk[];
  onNewVm: () => void;
  onTerminal: () => void;
  onLogs: () => void;
}

function CommandOutput({ result }: { result: CommandResult }) {
  if (result.error !== undefined) {
    return (
      <div className="rounded-lg border border-rose-500/30 bg-rose-500/10 px-4 py-3 font-mono text-[12px] whitespace-pre-wrap text-rose-300">
        {result.error}
      </div>
    );
  }
  const failed = result.exitCode !== undefined && result.exitCode !== 0;
  return (
    <div className="overflow-hidden rounded-lg border border-[var(--app-border)] bg-[var(--app-code)]">
      <div className="flex items-center justify-between border-b border-[var(--app-border)] px-3 py-1.5 font-mono text-[11px] text-[var(--app-muted)]">
        <span>exit {result.exitCode ?? 0}</span>
        {failed ? (
          <span className="text-rose-400">failed</span>
        ) : (
          <span className="text-emerald-400">ok</span>
        )}
      </div>
      {result.stdout !== undefined && result.stdout !== "" ? (
        <pre className="overflow-x-auto px-4 py-3 font-mono text-[12px] leading-relaxed whitespace-pre-wrap text-[var(--app-text)]">
          {result.stdout}
        </pre>
      ) : null}
      {result.stderr !== undefined && result.stderr !== "" ? (
        <pre className="overflow-x-auto border-t border-[var(--app-border)] px-4 py-3 font-mono text-[12px] leading-relaxed whitespace-pre-wrap text-rose-300">
          {result.stderr}
        </pre>
      ) : null}
      {failed && result.stderr === undefined ? (
        <p className="px-4 py-3 text-[12px] text-[var(--app-muted)]">
          Command exited with code {result.exitCode}.
        </p>
      ) : null}
    </div>
  );
}

function ToolRow({
  icon,
  name,
  detail,
  value,
}: {
  icon: React.ReactNode;
  name: string;
  detail: string;
  value: string;
}) {
  return (
    <div className="flex items-center gap-3 border-t border-[var(--app-border)] px-4 py-2.5 first:border-t-0">
      <span className="grid size-6 shrink-0 place-items-center rounded-md border border-[var(--app-border)] text-[var(--app-muted)]">
        {icon}
      </span>
      <div className="min-w-0 flex-1">
        <div className="flex min-w-0 items-center gap-2">
          <span className="shrink-0 text-[12px] font-medium text-[var(--app-text)]">{name}</span>
          <span className="truncate font-mono text-[11px] text-[var(--app-muted)]">{detail}</span>
        </div>
      </div>
      <span className="shrink-0 text-right font-mono text-[11px] text-[var(--app-muted)]">
        {value}
      </span>
      <CheckIcon className="size-3.5 shrink-0 text-emerald-400" />
    </div>
  );
}

/**
 * Chat-like sandbox inspector: live state, a tool-call summary and an executable
 * command composer. It intentionally looks like the reference agent workspace,
 * while every panel is backed by the real sandbox API.
 */
export function SandboxChat({ sandbox, images, onNewVm, onTerminal, onLogs }: SandboxChatProps) {
  const statsQuery = useSandboxStats(sandbox.id);
  const stats = statsQuery.data;
  const [input, setInput] = useState("");
  const [results, setResults] = useState<CommandResult[]>([]);
  const [pending, setPending] = useState(false);
  const [copied, setCopied] = useState(false);
  const bottomRef = useRef<HTMLDivElement>(null);

  const image = images.find((disk) => disk.digest === sandbox.base_disk);
  const command = "fastvmctl exec " + sandbox.name + " -- uname -a";

  useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: "smooth", block: "end" });
  }, [results.length, pending]);

  const submit = async () => {
    const commandLine = input.trim();
    if (commandLine === "" || pending) return;
    setInput("");
    setPending(true);
    try {
      const result = await execSandbox(sandbox.id, ["/bin/sh", "-c", commandLine]);
      setResults((previous) => [
        ...previous,
        {
          command: commandLine,
          stdout: result.stdout,
          stderr: result.stderr,
          exitCode: result.exit_code,
        },
      ]);
    } catch (error) {
      setResults((previous) => [
        ...previous,
        { command: commandLine, error: describeError(error) },
      ]);
    } finally {
      setPending(false);
    }
  };

  const copyCommand = async () => {
    try {
      await navigator.clipboard.writeText(command);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1200);
    } catch {
      setCopied(false);
    }
  };

  const stateText = sandbox.state;
  const cpu = stats === undefined ? "—" : formatPercent(stats.cpu_percent);
  const rss = stats === undefined ? "—" : formatBytes(stats.memory_rss_bytes);
  const ports =
    sandbox.ports === undefined || sandbox.ports.length === 0
      ? "none"
      : sandbox.ports
          .map((port) => `${port.host_port}→${port.guest_port}/${port.proto}`)
          .join(", ");

  return (
    <div className="flex h-full min-h-0 flex-col bg-[var(--app-bg)]">
      <div className="min-h-0 flex-1 overflow-y-auto">
        <div className="mx-auto max-w-3xl px-4 py-8 sm:px-6">
          <div className="flex justify-end">
            <div className="max-w-lg rounded-xl border border-[var(--app-border)] bg-[var(--app-panel-2)] px-4 py-3 text-[13px] leading-relaxed text-[var(--app-text)]">
              Inspect <span className="font-medium">{sandbox.name}</span> and help me understand
              what this sandbox is doing.
            </div>
          </div>

          <div className="mt-7 flex gap-3">
            <div className="mt-0.5 flex size-5 shrink-0 items-center justify-center text-[var(--app-muted)]">
              <ChevronDownIcon className="size-3.5" />
            </div>
            <div className="min-w-0 flex-1">
              <p className="text-[12px] font-medium text-[var(--app-text)]">
                Thought: Read live sandbox state
              </p>
              <div className="mt-2 border-l-2 border-[var(--app-border)] pl-4 text-[12px] leading-relaxed text-[var(--app-muted)] italic">
                The VM is {stateText.toLowerCase()}. I will inspect lifecycle state, resource use,
                the base image and published ports, then show a command the user can run or execute
                here.
              </div>
            </div>
          </div>

          <p className="mt-7 text-[13px] leading-relaxed text-[var(--app-text)]">
            Here is the current snapshot for <span className="font-medium">{sandbox.name}</span>.
          </p>

          <div className="mt-4 overflow-hidden rounded-xl border border-[var(--app-border)] bg-[var(--app-panel)]">
            <div className="flex flex-wrap items-center justify-between gap-2 px-4 py-3">
              <div className="flex items-center gap-2 text-[12px] text-[var(--app-text)]">
                <WrenchScrewdriverIcon className="size-4 text-[var(--app-muted)]" />
                <span>5 checks</span>
                <span className="text-[var(--app-muted)]">
                  · sandbox, stats, image, ports, agent
                </span>
              </div>
              <span className="inline-flex items-center gap-1.5 text-[11px] text-[var(--app-muted)]">
                <ClockIcon className="size-3.5" /> live
              </span>
            </div>
            <ToolRow
              icon={<ServerIcon className="size-3.5" />}
              name="get_sandbox"
              detail={shortId(sandbox.id)}
              value={stateText}
            />
            <ToolRow
              icon={<ActivityIcon className="size-3.5" />}
              name="get_stats"
              detail="cpu / rss"
              value={`${cpu} · ${rss}`}
            />
            <ToolRow
              icon={<HardDriveIcon className="size-3.5" />}
              name="image"
              detail={image === undefined ? shortDigest(sandbox.base_disk) : imageLabel(image)}
              value={sandbox.root_size_gb === undefined ? "—" : `${sandbox.root_size_gb} GiB`}
            />
            <ToolRow
              icon={<NetworkIcon className="size-3.5" />}
              name="ports"
              detail={sandbox.ip === undefined ? "no ip" : sandbox.ip}
              value={ports}
            />
            <ToolRow
              icon={<ServerIcon className="size-3.5" />}
              name="resources"
              detail={`${sandbox.vcpus} vCPU · ${sandbox.memory_mb} MiB`}
              value={sandbox.no_auto_sleep ? "sleep disabled" : "auto-sleep"}
            />
          </div>

          <p className="mt-6 text-[13px] leading-relaxed text-[var(--app-text)]">
            The sandbox is <span className="font-medium">{stateText.toLowerCase()}</span>.
            Executions wake it automatically; the agent stays reachable over vsock. Use the composer
            below or open a terminal for an interactive session.
          </p>

          <div className="mt-4 overflow-hidden rounded-xl border border-[var(--app-border)] bg-[var(--app-code)]">
            <div className="flex items-center justify-between border-b border-[var(--app-border)] px-4 py-2">
              <span className="font-mono text-[10px] text-[var(--app-muted)]">sh</span>
              <button
                type="button"
                onClick={() => void copyCommand()}
                className="inline-flex items-center gap-1 text-[11px] text-[var(--app-muted)] hover:text-[var(--app-text)]"
              >
                <ClipboardDocumentIcon className="size-3.5" />
                {copied ? "copied" : "copy"}
              </button>
            </div>
            <pre className="overflow-x-auto px-4 py-3 font-mono text-[12px] text-[var(--app-text)]">
              {command}
            </pre>
          </div>

          <ul className="mt-5 space-y-2 text-[13px] leading-relaxed text-[var(--app-text)]">
            <li className="flex gap-2">
              <span className="mt-1 size-1.5 shrink-0 rounded-full bg-[var(--app-accent)]" />
              State and stats update from the daemon in real time.
            </li>
            <li className="flex gap-2">
              <span className="mt-1 size-1.5 shrink-0 rounded-full bg-[var(--app-accent)]" />
              Commands run through the same exec API as{" "}
              <code className="font-mono text-[12px]">fastvmctl</code>.
            </li>
          </ul>

          {results.map((result, index) => (
            <div key={index} className="mt-6 space-y-3">
              <div className="flex justify-end">
                <div className="max-w-xl rounded-xl bg-[var(--app-panel-2)] px-4 py-2.5 font-mono text-[12px] text-[var(--app-text)]">
                  <span className="mr-2 text-[var(--app-accent)]">$</span>
                  {result.command}
                </div>
              </div>
              <CommandOutput result={result} />
            </div>
          ))}

          {pending ? (
            <div className="mt-5 flex items-center gap-2 text-[12px] text-[var(--app-muted)]">
              <ArrowPathIcon className="size-4 animate-spin" /> Running command…
            </div>
          ) : null}
          <div ref={bottomRef} />
        </div>
      </div>

      <div className="shrink-0 border-t border-[var(--app-border)] bg-[var(--app-bg)] px-4 py-3 sm:px-6">
        <div className="mx-auto max-w-3xl rounded-xl border border-[var(--app-border)] bg-[var(--app-panel)] p-2 shadow-sm">
          <textarea
            value={input}
            onChange={(event) => setInput(event.currentTarget.value)}
            onKeyDown={(event) => {
              if (event.key === "Enter" && !event.shiftKey) {
                event.preventDefault();
                void submit();
              }
            }}
            rows={1}
            placeholder={`Run a command in ${sandbox.name}…`}
            className="block max-h-32 min-h-8 w-full resize-none bg-transparent px-2 py-1.5 text-[13px] text-[var(--app-text)] outline-none placeholder:text-[var(--app-faint)]"
          />
          <div className="mt-1 flex items-center gap-1.5">
            <button
              type="button"
              onClick={onNewVm}
              aria-label="Create a new VM"
              className="grid size-7 place-items-center rounded-md text-[var(--app-muted)] hover:bg-[var(--app-selected)] hover:text-[var(--app-text)]"
            >
              <PlusIcon className="size-4" />
            </button>
            <button
              type="button"
              onClick={onTerminal}
              className="inline-flex h-7 items-center gap-1.5 rounded-md border border-[var(--app-border)] px-2 text-[11px] text-[var(--app-muted)] hover:text-[var(--app-text)]"
            >
              <span className="size-1.5 rounded-full bg-emerald-400" />
              terminal
            </button>
            <button
              type="button"
              onClick={onLogs}
              className="inline-flex h-7 items-center rounded-md px-2 text-[11px] text-[var(--app-muted)] hover:bg-[var(--app-selected)] hover:text-[var(--app-text)]"
            >
              logs
            </button>
            <span className="ml-auto hidden items-center gap-1 text-[11px] text-[var(--app-muted)] sm:flex">
              <span className="rounded border border-[var(--app-border)] px-1.5 py-0.5">
                fastvm-agent
              </span>
              <span className="rounded border border-[var(--app-border)] px-1.5 py-0.5">
                {stateText}
              </span>
            </span>
            <button
              type="button"
              onClick={() => void submit()}
              disabled={input.trim() === "" || pending}
              aria-label="Send command"
              className={cn(
                "ml-auto grid size-8 place-items-center rounded-md text-white transition-opacity",
                input.trim() === "" || pending ? "opacity-40" : "opacity-100 hover:opacity-90",
              )}
              style={{ backgroundColor: "var(--app-accent)" }}
            >
              <PaperAirplaneIcon className="size-4 -rotate-45" />
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}

export default SandboxChat;
