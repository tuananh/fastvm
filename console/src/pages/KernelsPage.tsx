import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  CircleStackIcon,
  CloudArrowDownIcon,
  CpuChipIcon,
  TrashIcon,
} from "@heroicons/react/24/outline";
import { useState } from "react";
import { downloadKernel, listRemoteKernels, removeKernel, useKernel } from "../lib/api";
import { queryKeys, useKernels } from "../lib/queries";
import type { KernelInfo, KernelRemoteInfo } from "../lib/types";
import { Button, Spinner, cn, useToast } from "../ui";
import { describeError } from "../utils/errors";
import { formatBytes } from "../utils/format";

function shortSha(value: string | undefined): string {
  return value === undefined || value === "" ? "—" : value.slice(0, 12);
}

function KernelRow({
  kernel,
  busy,
  onUse,
  onRemove,
}: {
  kernel: KernelInfo;
  busy: boolean;
  onUse: (release: string) => void;
  onRemove: (release: string) => void;
}) {
  const release =
    kernel.release === undefined || kernel.release === "" ? kernel.name : kernel.release;
  return (
    <tr className="border-b border-[var(--app-border)] last:border-b-0">
      <td className="px-4 py-3">
        <div className="flex items-center gap-2">
          <span
            className={cn(
              "size-1.5 shrink-0",
              kernel.active ? "bg-[var(--om-green)]" : "bg-[var(--app-faint)]",
            )}
          />
          <span className="font-mono text-[12px] text-[var(--app-text)]">{release}</span>
          {kernel.active ? (
            <span className="border border-[var(--om-green)]/40 px-1.5 text-[10px] text-[var(--om-green)]">
              default
            </span>
          ) : null}
        </div>
        <p className="mt-0.5 break-all font-mono text-[10px] text-[var(--app-muted)]">
          {kernel.path}
        </p>
      </td>
      <td className="px-4 py-3 font-mono text-[11px] text-[var(--app-muted)]">
        {kernel.asset ?? "—"}
      </td>
      <td className="px-4 py-3 text-[11px] text-[var(--app-muted)]">
        {formatBytes(kernel.size_bytes)}
      </td>
      <td className="px-4 py-3 font-mono text-[10px] text-[var(--app-muted)]">
        {shortSha(kernel.sha256)}
      </td>
      <td className="px-4 py-3 text-right">
        <div className="flex justify-end gap-2">
          <Button
            size="xs"
            variant="secondary"
            disabled={kernel.active || busy}
            onClick={() => onUse(release)}
          >
            Set default
          </Button>
          <Button
            size="xs"
            variant="ghost"
            disabled={kernel.active || busy}
            onClick={() => onRemove(release)}
            icon={<TrashIcon className="size-3.5" />}
          >
            Remove
          </Button>
        </div>
      </td>
    </tr>
  );
}

function RemoteRow({
  kernel,
  busy,
  onDownload,
}: {
  kernel: KernelRemoteInfo;
  busy: boolean;
  onDownload: (release: string) => void;
}) {
  return (
    <tr className="border-b border-[var(--app-border)] last:border-b-0">
      <td className="px-4 py-2.5 font-mono text-[11px] text-[var(--app-text)]">{kernel.release}</td>
      <td className="px-4 py-2.5 font-mono text-[11px] text-[var(--app-muted)]">{kernel.asset}</td>
      <td className="px-4 py-2.5 text-[11px] text-[var(--app-muted)]">
        {formatBytes(kernel.size_bytes)}
      </td>
      <td className="px-4 py-2.5 text-right">
        <Button
          size="xs"
          variant="secondary"
          disabled={busy}
          onClick={() => onDownload(kernel.release)}
          icon={<CloudArrowDownIcon className="size-3.5" />}
        >
          Download
        </Button>
      </td>
    </tr>
  );
}

export default function KernelsPage() {
  const toast = useToast();
  const queryClient = useQueryClient();
  const kernels = useKernels();
  const [release, setRelease] = useState("");
  const [showRemote, setShowRemote] = useState(false);
  const [busy, setBusy] = useState(false);

  const remote = useQuery({
    queryKey: [...queryKeys.kernels, "remote"],
    queryFn: listRemoteKernels,
    enabled: showRemote,
    staleTime: 60_000,
  });

  const invalidate = () => {
    void queryClient.invalidateQueries({ queryKey: queryKeys.kernels });
  };

  const download = useMutation({
    mutationFn: (value: string) => downloadKernel(value),
    onSuccess: (result) => {
      toast.success(`Downloaded ${result.release}`);
      invalidate();
    },
    onError: (error) => toast.error(describeError(error)),
    onSettled: () => setBusy(false),
  });
  const activate = useMutation({
    mutationFn: useKernel,
    onSuccess: (result) => {
      toast.success(`Default kernel: ${result.current}`);
      invalidate();
    },
    onError: (error) => toast.error(describeError(error)),
    onSettled: () => setBusy(false),
  });
  const remove = useMutation({
    mutationFn: removeKernel,
    onSuccess: (result) => {
      toast.success(`Removed ${result.release}`);
      invalidate();
    },
    onError: (error) => toast.error(describeError(error)),
    onSettled: () => setBusy(false),
  });

  const startDownload = (value: string) => {
    if (busy) return;
    setBusy(true);
    download.mutate(value.trim());
  };

  const startUse = (value: string) => {
    if (busy) return;
    setBusy(true);
    activate.mutate(value);
  };

  const startRemove = (value: string) => {
    if (busy) return;
    if (!window.confirm(`Remove kernel ${value}?`)) return;
    setBusy(true);
    remove.mutate(value);
  };

  const list = kernels.data;
  const kernelsList = list?.kernels ?? [];

  return (
    <div className="mx-auto max-w-[1180px] px-4 py-6 sm:px-6 lg:px-8">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <div className="flex items-center gap-2">
            <CpuChipIcon className="size-4 text-[var(--app-accent)]" />
            <h1 className="text-[18px] font-semibold tracking-tight text-[var(--app-text)]">
              Kernels
            </h1>
          </div>
          <p className="mt-1 font-mono text-[11px] text-[var(--app-muted)]">
            {list?.dir ?? "daemon kernel directory"}
          </p>
        </div>
        <Button
          onClick={() => {
            setShowRemote(true);
            void remote.refetch();
          }}
          icon={<CircleStackIcon className="size-4" />}
        >
          List remote
        </Button>
      </div>

      <section className="mt-5 border border-[var(--app-border)] bg-[var(--app-panel)]">
        <header className="flex flex-wrap items-center justify-between gap-3 border-b border-[var(--app-border)] px-4 py-3">
          <div>
            <h2 className="text-[12px] font-semibold text-[var(--app-text)]">Installed kernels</h2>
            <p className="mt-0.5 text-[11px] text-[var(--app-muted)]">
              {kernelsList.length} kernel{kernelsList.length === 1 ? "" : "s"} ·{" "}
              {list?.current ?? "no default selected"}
            </p>
          </div>
          <Button
            size="xs"
            variant="ghost"
            onClick={() => void kernels.refetch()}
            icon={<CloudArrowDownIcon className="size-3.5" />}
          >
            Refresh
          </Button>
        </header>
        {kernels.isPending ? (
          <div className="flex items-center gap-2 p-4 text-[12px] text-[var(--app-muted)]">
            <Spinner className="size-4" /> Loading kernels…
          </div>
        ) : kernels.isError ? (
          <p className="p-4 text-[12px] text-[var(--om-red)]">{describeError(kernels.error)}</p>
        ) : kernelsList.length === 0 ? (
          <p className="p-4 text-[12px] text-[var(--app-muted)]">
            No kernels installed. Download one below or from the remote list.
          </p>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full min-w-[760px] text-left">
              <thead className="border-b border-[var(--app-border)] text-[10px] tracking-[0.08em] text-[var(--app-muted)] uppercase">
                <tr>
                  <th className="px-4 py-2 font-medium">Release / path</th>
                  <th className="px-4 py-2 font-medium">Asset</th>
                  <th className="px-4 py-2 font-medium">Size</th>
                  <th className="px-4 py-2 font-medium">SHA-256</th>
                  <th className="px-4 py-2 text-right font-medium">Actions</th>
                </tr>
              </thead>
              <tbody>
                {kernelsList.map((kernel) => (
                  <KernelRow
                    key={kernel.path ?? kernel.name}
                    kernel={kernel}
                    busy={busy}
                    onUse={startUse}
                    onRemove={startRemove}
                  />
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      <section className="mt-4 border border-[var(--app-border)] bg-[var(--app-panel)]">
        <header className="border-b border-[var(--app-border)] px-4 py-3">
          <h2 className="text-[12px] font-semibold text-[var(--app-text)]">Download</h2>
          <p className="mt-0.5 text-[11px] text-[var(--app-muted)]">
            Downloads are verified against the release SHA-256. Leave the release empty for the
            latest.
          </p>
        </header>
        <div className="flex flex-wrap items-center gap-2 p-4">
          <input
            value={release}
            onChange={(event) => setRelease(event.currentTarget.value)}
            placeholder="release, e.g. ch-release-v7.2.8-20261009"
            className="h-8 min-w-64 flex-1 border border-[var(--app-border)] bg-[var(--app-code)] px-2.5 font-mono text-[12px] text-[var(--app-text)] outline-none placeholder:text-[var(--app-faint)] focus:border-[var(--app-accent)]"
          />
          <Button
            variant="primary"
            disabled={busy}
            onClick={() => startDownload(release)}
            icon={<CloudArrowDownIcon className="size-4" />}
          >
            {release.trim() === "" ? "Download latest" : "Download"}
          </Button>
        </div>
      </section>

      {showRemote ? (
        <section className="mt-4 border border-[var(--app-border)] bg-[var(--app-panel)]">
          <header className="flex items-center justify-between gap-3 border-b border-[var(--app-border)] px-4 py-3">
            <div>
              <h2 className="text-[12px] font-semibold text-[var(--app-text)]">
                Available kernels
              </h2>
              <p className="mt-0.5 text-[11px] text-[var(--app-muted)]">
                cloud-hypervisor/linux direct-boot assets
              </p>
            </div>
            {remote.isFetching ? <Spinner className="size-4 text-[var(--app-muted)]" /> : null}
          </header>
          {remote.isPending ? (
            <p className="p-4 text-[12px] text-[var(--app-muted)]">Fetching release list…</p>
          ) : remote.isError ? (
            <p className="p-4 text-[12px] text-[var(--om-red)]">{describeError(remote.error)}</p>
          ) : (remote.data?.kernels ?? []).length === 0 ? (
            <p className="p-4 text-[12px] text-[var(--app-muted)]">No matching kernels found.</p>
          ) : (
            <div className="overflow-x-auto">
              <table className="w-full min-w-[620px] text-left">
                <thead className="border-b border-[var(--app-border)] text-[10px] tracking-[0.08em] text-[var(--app-muted)] uppercase">
                  <tr>
                    <th className="px-4 py-2 font-medium">Release</th>
                    <th className="px-4 py-2 font-medium">Asset</th>
                    <th className="px-4 py-2 font-medium">Size</th>
                    <th className="px-4 py-2 text-right font-medium">Action</th>
                  </tr>
                </thead>
                <tbody>
                  {(remote.data?.kernels ?? []).map((kernel) => (
                    <RemoteRow
                      key={`${kernel.release}-${kernel.asset}`}
                      kernel={kernel}
                      busy={busy}
                      onDownload={startDownload}
                    />
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </section>
      ) : null}
    </div>
  );
}
