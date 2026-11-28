import { Bars3Icon, CloudIcon, FolderIcon } from "@heroicons/react/24/outline";
import { useQueryClient } from "@tanstack/react-query";
import { useCallback, useEffect, useState } from "react";
import { AppSidebar } from "../components/AppSidebar";
import type { ConsoleView } from "../components/AppSidebar";
import { CreateVmModal } from "../components/CreateVmModal";
import { ClockIcon, PlusIcon, RefreshIcon } from "../components/icons";
import { SummaryCards } from "../components/SummaryCards";
import { LogsDialog } from "../components/LogsDialog";
import { SandboxChat } from "../components/SandboxChat";
import { TerminalDialog } from "../components/TerminalDialog";
import { VmDetailDrawer } from "../components/VmDetailDrawer";
import { VmTable } from "../components/VmTable";
import ImagesPage from "./ImagesPage";
import type { PendingAction, VmActionKind } from "../components/vmActions";
import {
  queryKeys,
  useDeleteSandbox,
  useImages,
  useSandboxes,
  useSleepSandbox,
  useVersion,
  useWakeSandbox,
} from "../lib/queries";
import type { BaseDisk, Sandbox } from "../lib/types";
import { Button, IconButton, Modal, cn, useToast } from "../ui";
import { describeError } from "../utils/errors";
import { formatClockTime, formatDateTime } from "../utils/format";

const EMPTY_SANDBOXES: readonly Sandbox[] = [];
const EMPTY_IMAGES: readonly BaseDisk[] = [];

/**
 * Main console screen: HyperUI sidebar shell with a polled sandbox table,
 * summary counters, lifecycle actions, create flow and live detail drawer.
 */
export default function DashboardPage() {
  const toast = useToast();
  const queryClient = useQueryClient();
  const sandboxesQuery = useSandboxes();
  const imagesQuery = useImages();
  const version = useVersion();
  const sleepSandbox = useSleepSandbox();
  const wakeSandbox = useWakeSandbox();
  const deleteSandbox = useDeleteSandbox();

  const [view, setView] = useState<ConsoleView>(() =>
    typeof window !== "undefined" && window.location.hash === "#images" ? "images" : "sandboxes",
  );
  const [selectedSandboxId, setSelectedSandboxId] = useState<string | null>(null);
  const [sidebarOpen, setSidebarOpen] = useState(false);
  const [openPullOnImages, setOpenPullOnImages] = useState(false);
  const [createOpen, setCreateOpen] = useState(false);
  const [createImage, setCreateImage] = useState<string | null>(null);
  const [manualRefreshing, setManualRefreshing] = useState(false);
  const [pendingAction, setPendingAction] = useState<PendingAction | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<Sandbox | null>(null);
  const [deleteOpen, setDeleteOpen] = useState(false);
  const [detailSnapshot, setDetailSnapshot] = useState<Sandbox | null>(null);
  const [detailRef, setDetailRef] = useState<string | null>(null);
  const [drawerOpen, setDrawerOpen] = useState(false);
  const [terminalTarget, setTerminalTarget] = useState<Sandbox | null>(null);
  const [terminalOpen, setTerminalOpen] = useState(false);
  const [logsTarget, setLogsTarget] = useState<Sandbox | null>(null);
  const [logsOpen, setLogsOpen] = useState(false);

  useEffect(() => {
    const onHashChange = () => {
      setView(window.location.hash === "#images" ? "images" : "sandboxes");
    };
    window.addEventListener("hashchange", onHashChange);
    return () => {
      window.removeEventListener("hashchange", onHashChange);
    };
  }, []);

  const navigate = useCallback((next: ConsoleView) => {
    setView(next);
    const hash = next === "images" ? "#images" : "#sandboxes";
    if (window.location.hash !== hash) {
      window.history.replaceState(null, "", hash);
    }
  }, []);

  const selectSandbox = useCallback((sandbox: Sandbox) => {
    setSelectedSandboxId(sandbox.id);
    setView("sandbox");
    if (window.location.hash !== "#sandboxes") {
      window.history.replaceState(null, "", "#sandboxes");
    }
  }, []);

  const openCreate = useCallback(() => {
    setCreateImage(null);
    setCreateOpen(true);
  }, []);

  const createFromImage = useCallback(
    (image: BaseDisk) => {
      setCreateImage(image.digest);
      setCreateOpen(true);
      navigate("sandboxes");
    },
    [navigate],
  );

  const sandboxes = sandboxesQuery.data ?? EMPTY_SANDBOXES;
  const images = imagesQuery.data ?? EMPTY_IMAGES;
  const selectedSandbox =
    selectedSandboxId === null
      ? null
      : (sandboxes.find((sandbox) => sandbox.id === selectedSandboxId) ?? null);

  const detailSandbox =
    detailRef === null
      ? detailSnapshot
      : (sandboxes.find((sandbox) => sandbox.id === detailRef) ?? detailSnapshot);

  const terminalSandbox =
    terminalTarget === null
      ? null
      : (sandboxes.find((sandbox) => sandbox.id === terminalTarget.id) ?? terminalTarget);

  const logsSandbox =
    logsTarget === null
      ? null
      : (sandboxes.find((sandbox) => sandbox.id === logsTarget.id) ?? logsTarget);

  const openDetails = useCallback((sandbox: Sandbox) => {
    setDetailSnapshot(sandbox);
    setDetailRef(sandbox.id);
    setDrawerOpen(true);
  }, []);

  const closeDrawer = useCallback(() => {
    setDrawerOpen(false);
    setDetailRef(null);
    setDetailSnapshot(null);
  }, []);

  const openTerminal = useCallback((sandbox: Sandbox) => {
    setTerminalTarget(sandbox);
    setTerminalOpen(true);
  }, []);

  const closeTerminal = useCallback(() => {
    setTerminalOpen(false);
    setTerminalTarget(null);
  }, []);

  const openLogs = useCallback((sandbox: Sandbox) => {
    setLogsTarget(sandbox);
    setLogsOpen(true);
  }, []);

  const closeLogs = useCallback(() => {
    setLogsOpen(false);
    setLogsTarget(null);
  }, []);

  const handleRefresh = useCallback(async () => {
    setManualRefreshing(true);
    try {
      await sandboxesQuery.refetch();
    } finally {
      setManualRefreshing(false);
    }
  }, [sandboxesQuery]);

  const openDelete = useCallback((sandbox: Sandbox) => {
    setDeleteTarget(sandbox);
    setDeleteOpen(true);
  }, []);

  const handleAction = useCallback(
    async (kind: VmActionKind, sandbox: Sandbox) => {
      if (kind === "delete") {
        openDelete(sandbox);
        return;
      }
      if (pendingAction !== null) {
        return;
      }
      setPendingAction({ ref: sandbox.id, kind });
      try {
        if (kind === "pause") {
          await sleepSandbox.mutateAsync({ ref: sandbox.id, deep: false });
          toast.success(`${sandbox.name} paused`);
        } else if (kind === "deepSleep") {
          await sleepSandbox.mutateAsync({ ref: sandbox.id, deep: true });
          toast.success(`${sandbox.name} is deeply asleep`);
        } else {
          await wakeSandbox.mutateAsync(sandbox.id);
          toast.success(`${sandbox.name} resumed`);
        }
      } catch (error) {
        toast.error(describeError(error));
      } finally {
        setPendingAction(null);
      }
    },
    [openDelete, pendingAction, sleepSandbox, toast, wakeSandbox],
  );

  const closeDelete = useCallback(() => {
    if (pendingAction?.kind === "delete") {
      return;
    }
    setDeleteOpen(false);
    setDeleteTarget(null);
  }, [pendingAction]);

  const confirmDelete = useCallback(async () => {
    if (deleteTarget === null) {
      return;
    }
    const target = deleteTarget;
    setPendingAction({ ref: target.id, kind: "delete" });
    try {
      await deleteSandbox.mutateAsync(target.id);
      // Remove the row immediately: its stats query would otherwise keep polling a
      // sandbox that no longer exists and log a 404 until the list refetch lands.
      queryClient.setQueryData<Sandbox[]>(queryKeys.sandboxes, (previous) =>
        previous === undefined ? previous : previous.filter((sandbox) => sandbox.id !== target.id),
      );
      queryClient.removeQueries({ queryKey: queryKeys.sandboxStats(target.id) });
      toast.success(`Deleted ${target.name}`);
      setDeleteOpen(false);
      setDeleteTarget(null);
      if (detailRef === target.id) {
        closeDrawer();
      }
      if (terminalTarget?.id === target.id) {
        closeTerminal();
      }
      if (logsTarget?.id === target.id) {
        closeLogs();
      }
    } catch (error) {
      toast.error(describeError(error));
    } finally {
      setPendingAction(null);
    }
  }, [
    closeDrawer,
    closeLogs,
    closeTerminal,
    deleteSandbox,
    deleteTarget,
    detailRef,
    logsTarget,
    queryClient,
    terminalTarget,
    toast,
  ]);

  const deleting = pendingAction?.kind === "delete";
  const listError = sandboxesQuery.isError ? describeError(sandboxesQuery.error) : null;
  const lastUpdated = sandboxesQuery.dataUpdatedAt;
  const lastUpdatedLabel = lastUpdated > 0 ? formatClockTime(lastUpdated) : "—";

  const versionText = version.isPending
    ? "fastvmd …"
    : version.isError
      ? "fastvmd offline"
      : version.data?.name !== undefined && version.data.name !== ""
        ? `${version.data.name} v${version.data.version}`
        : `fastvmd ${version.data?.version ?? "?"}`;

  return (
    <div className="flex h-screen overflow-hidden bg-[var(--app-bg)] text-[var(--app-text)]">
      <AppSidebar
        open={sidebarOpen}
        view={view}
        sandboxes={sandboxes}
        selectedSandboxId={selectedSandboxId}
        onNavigate={(next) => {
          navigate(next);
          setSelectedSandboxId(null);
        }}
        onSelectSandbox={selectSandbox}
        onNewVm={openCreate}
        onClose={() => setSidebarOpen(false)}
      />

      <div className="flex flex-1 flex-col overflow-y-auto">
        <header className="flex h-14 shrink-0 items-center justify-between gap-3 border-b border-[var(--app-border)] bg-[var(--app-bg)] px-4 sm:px-6">
          <div className="flex min-w-0 items-center gap-2">
            <button
              type="button"
              aria-label="Open sidebar"
              aria-expanded={sidebarOpen}
              aria-controls="app-sidebar"
              onClick={() => setSidebarOpen(true)}
              className="rounded-md p-1.5 text-[var(--app-muted)] hover:bg-[var(--app-selected)] hover:text-[var(--app-text)] focus-visible:outline-none lg:hidden"
            >
              <Bars3Icon className="size-4" />
            </button>
            <FolderIcon className="hidden size-4 text-[var(--app-faint)] sm:block" />
            <nav aria-label="Breadcrumb" className="flex min-w-0 items-center gap-1.5 text-[13px]">
              <span className="hidden shrink-0 text-[var(--app-muted)] sm:inline">fastvm</span>
              <span className="hidden text-[var(--app-faint)] sm:inline">›</span>
              <span className="truncate font-medium text-[var(--app-text)]">
                {view === "sandbox" && selectedSandbox !== null
                  ? selectedSandbox.name
                  : view === "images"
                    ? "Images"
                    : "Activity"}
              </span>
              {view === "sandbox" && selectedSandbox !== null ? (
                <span className="ml-1 hidden truncate text-[var(--app-faint)] sm:inline">
                  {selectedSandbox.state}
                </span>
              ) : null}
            </nav>
          </div>

          <div className="flex shrink-0 items-center gap-1.5 sm:gap-2">
            <span
              className={cn(
                "hidden items-center gap-1.5 rounded-full border border-[var(--app-border)] px-2 py-0.5 text-[11px] font-medium whitespace-nowrap sm:inline-flex",
                version.isError ? "text-rose-400" : "text-[var(--app-muted)]",
              )}
              title={
                version.isError
                  ? "fastvmd is unreachable; lifecycle data cannot be loaded."
                  : "Connected to fastvmd over the same-origin API."
              }
            >
              <span
                aria-hidden="true"
                className={cn(
                  "size-1.5 rounded-full",
                  version.isError
                    ? "bg-rose-400"
                    : version.isPending
                      ? "bg-gray-400"
                      : "bg-emerald-400",
                )}
              />
              {versionText}
            </span>

            {view !== "images" ? (
              <IconButton
                label="Refresh sandboxes"
                onClick={() => void handleRefresh()}
                disabled={manualRefreshing}
                className="text-[var(--app-muted)] hover:bg-[var(--app-selected)] hover:text-[var(--app-text)]"
              >
                <RefreshIcon className={manualRefreshing ? "size-4 spinning" : "size-4"} />
              </IconButton>
            ) : null}

            <button
              type="button"
              onClick={openCreate}
              className="inline-flex h-8 items-center gap-1.5 rounded-md border border-[var(--app-border)] bg-[var(--app-panel)] px-2.5 text-[12px] font-medium text-[var(--app-text)] transition-colors hover:border-[var(--app-accent)]"
            >
              <PlusIcon className="size-3.5" />
              Create VM
            </button>
            <span className="hidden size-8 place-items-center rounded-md border border-[var(--app-border)] text-[var(--app-muted)] sm:grid">
              <CloudIcon className="size-3.5" />
            </span>
          </div>
        </header>

        {view === "sandbox" && selectedSandbox !== null ? (
          <main className="min-h-0 flex-1">
            <SandboxChat
              sandbox={selectedSandbox}
              images={images}
              onNewVm={openCreate}
              onTerminal={() => openTerminal(selectedSandbox)}
              onLogs={() => openLogs(selectedSandbox)}
            />
          </main>
        ) : (
          <main className="min-h-0 flex-1 overflow-y-auto">
            <div className="mx-auto max-w-[1180px] space-y-6 p-4 sm:p-6 lg:p-8">
              {view === "images" ? (
                <ImagesPage
                  autoOpenPull={openPullOnImages}
                  onAutoOpenConsumed={() => setOpenPullOnImages(false)}
                  onCreateVm={createFromImage}
                />
              ) : (
                <>
                  <section id="overview" aria-label="Overview">
                    <SummaryCards sandboxes={sandboxes} loading={sandboxesQuery.isPending} />
                  </section>

                  <section
                    id="sandboxes"
                    aria-label="Sandboxes"
                    className="overflow-hidden rounded-xl border border-[var(--app-border)] bg-[var(--app-panel)]"
                  >
                    <div className="flex flex-wrap items-center justify-between gap-3 border-b border-[var(--app-border)] px-5 py-3.5">
                      <div>
                        <h2 className="text-[13px] font-semibold text-[var(--app-text)]">
                          All sandboxes
                        </h2>
                        <p className="mt-0.5 text-[11px] text-[var(--app-muted)]">
                          {sandboxes.length} VM{sandboxes.length === 1 ? "" : "s"} · click a sandbox
                          in the sidebar to inspect it
                        </p>
                      </div>
                      <span
                        className="inline-flex items-center gap-1.5 text-[11px] text-[var(--app-muted)]"
                        title={formatDateTime(lastUpdated)}
                      >
                        <ClockIcon className="size-3.5" /> {lastUpdatedLabel}
                      </span>
                    </div>

                    <VmTable
                      sandboxes={sandboxes}
                      images={images}
                      loading={sandboxesQuery.isPending || manualRefreshing}
                      error={listError}
                      pendingAction={pendingAction}
                      onCreate={openCreate}
                      onRetry={() => void handleRefresh()}
                      onDetails={openDetails}
                      onAction={(kind, sandbox) => void handleAction(kind, sandbox)}
                      onExec={openTerminal}
                      onLogs={openLogs}
                    />
                  </section>
                </>
              )}
            </div>
          </main>
        )}
      </div>

      {createOpen ? (
        <CreateVmModal
          open
          initialImage={createImage ?? undefined}
          onClose={() => {
            setCreateOpen(false);
            setCreateImage(null);
          }}
          onGoToImages={() => {
            setCreateOpen(false);
            setCreateImage(null);
            setOpenPullOnImages(true);
            navigate("images");
          }}
        />
      ) : null}

      <Modal
        open={deleteOpen}
        onClose={closeDelete}
        size="sm"
        title="Delete this sandbox?"
        footer={
          <>
            <Button onClick={closeDelete} disabled={deleting}>
              Cancel
            </Button>
            <Button variant="danger" loading={deleting} onClick={() => void confirmDelete()}>
              Delete sandbox
            </Button>
          </>
        }
      >
        <p className="text-sm leading-relaxed text-gray-600 dark:text-gray-300">
          <strong className="font-semibold text-gray-900 dark:text-white">
            {deleteTarget?.name ?? "This sandbox"}
          </strong>{" "}
          and its writable volume will be removed permanently. Any running workload stops
          immediately and the scratch disk is discarded. This cannot be undone.
        </p>
      </Modal>

      {detailSnapshot !== null ? (
        <VmDetailDrawer
          sandbox={detailSandbox}
          open={drawerOpen}
          images={images}
          pendingAction={pendingAction}
          onClose={closeDrawer}
          onAction={(kind, sandbox) => void handleAction(kind, sandbox)}
          onExec={openTerminal}
          onLogs={openLogs}
        />
      ) : null}

      {terminalTarget !== null ? (
        <TerminalDialog sandbox={terminalSandbox} open={terminalOpen} onClose={closeTerminal} />
      ) : null}

      {logsTarget !== null ? (
        <LogsDialog sandbox={logsSandbox} open={logsOpen} onClose={closeLogs} />
      ) : null}
    </div>
  );
}
